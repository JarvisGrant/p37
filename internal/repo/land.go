package repo

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/op"
	"github.com/chenyiwen/p37/internal/version"
	"github.com/chenyiwen/p37/internal/workspace"
)

// FileStat 是 land 结果里的文件级统计。
type FileStat struct {
	Path     string `json:"path"`
	Added    int    `json:"add"`
	Deleted  int    `json:"del"`
	Kind     string `json:"kind"`
	DestPath string `json:"dest_path,omitempty"`
}

// LandResult 是 land / land_preview 的结果。
type LandResult struct {
	OpsCount  int           `json:"ops_count"`
	Files     []FileStat    `json:"files"`
	Conflicts []op.Conflict `json:"conflicts"`
	CanLand   bool          `json:"can_land"`
	// AutoMerged 是被 §6.3 规则自动合并（不产生冲突）的 hunk 数。
	// 用于让调用方知道「本来会冲突的地方被确定性规则消化掉了多少」。
	AutoMerged int     `json:"auto_merged"`
	VersionID  hash.ID `json:"-"`

	TargetHead    hash.ID `json:"-"`
	WSTree        hash.ID `json:"-"`
	WSOpsEpoch    int64   `json:"-"`
	PreviewID     string  `json:"-"`
	AlreadyLanded bool    `json:"-"`
	State         string  `json:"-"`
}

// ConflictsJSON 返回面向 MCP 的冲突列表（带 key）。
type ConflictView struct {
	Key        string          `json:"key"`
	Path       string          `json:"path"`
	Kind       string          `json:"kind"`
	State      string          `json:"state"`
	Base       []string        `json:"base,omitempty"`
	Lower      SideView        `json:"lower"`
	Upper      SideView        `json:"upper"`
	Candidates []CandidateView `json:"candidates,omitempty"`
}

// SideView 是冲突一侧的可读表示。
type SideView struct {
	Agent string   `json:"agent,omitempty"`
	Task  string   `json:"task,omitempty"`
	Lines []string `json:"lines,omitempty"`
}

// CandidateView 是候选解的可读表示。
type CandidateView struct {
	Kind string `json:"kind"`
	Note string `json:"note,omitempty"`
}

// Preview 是 land 预演（§7.5、D22）。
//
// 「无副作用」的准确含义（实现后明确化）：它**不改变版本、工作区状态与 head**。
// 但它会像其它以工作区为目标的调用一样触发一次采集（§5.2 触发源 b），并把
// 本次判定出的冲突持久化到 conflict 表 —— 否则 `conflict_resolve(key)` 无法
// 由 key 反查工作区，而「preview → resolve → land」正是 D22 想要的流程。
func (r *Repo) Preview(wsID string) (*LandResult, error) {
	res, err := r.plan(wsID, false)
	if err != nil {
		return nil, err
	}
	if err := r.persistConflicts(wsID, res.Conflicts); err != nil {
		return nil, err
	}
	return res, nil
}

// persistConflicts 用本次判定的结果替换该工作区的冲突记录。
func (r *Repo) persistConflicts(wsID string, conflicts []op.Conflict) error {
	if err := r.DB.ClearConflicts(wsID); err != nil {
		return err
	}
	if len(conflicts) == 0 {
		return nil
	}
	rows := make([]meta.ConflictRow, 0, len(conflicts))
	for i := range conflicts {
		c := &conflicts[i]
		rows = append(rows, meta.ConflictRow{
			Key:       c.Key,
			WsID:      wsID,
			OpID:      c.Upper.OpID,
			Path:      c.Path,
			Kind:      c.Kind,
			State:     "open",
			BaseJSON:  linesJSON(c.Base),
			LowerJSON: sideJSON(&c.Lower),
			UpperJSON: sideJSON(&c.Upper),
			CandsJSON: candsJSON(c.Candidat),
		})
	}
	return r.DB.UpsertConflicts(rows)
}

// plan 是 Preview 与 Land 共用的引擎（同一套 rebase，保证结果一致，§7.5）。
func (r *Repo) plan(wsID string, commit bool) (*LandResult, error) {
	fold, err := r.WS.FoldSet(wsID)
	if err != nil {
		return nil, err
	}
	// 1. 校验。
	for _, w := range fold {
		switch w.State {
		case meta.StateActive, meta.StateConflicted:
		default:
			return nil, fmt.Errorf("%w: 折叠集合中的 %s 处于 %s", workspace.ErrWrongState, w.ID, w.State)
		}
	}

	// 2. 强制采集（此时状态仍为 active）。
	//
	// 预演同样需要采集：以工作区为目标的 MCP 调用本身就是隐式同步点
	// （§5.2 触发源 b）。D22 所说的「无副作用」指的是不改变
	// 版本/工作区/冲突/血缘状态，而不是不更新 ops 这份可重建缓存。
	for _, w := range fold {
		if err := r.WS.Sync(w.ID); err != nil {
			return nil, err
		}
	}

	// 4. 当前 head。
	head, ok, err := r.DB.Head()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotInitialized
	}
	hv, err := r.DB.GetVersion(head)
	if err != nil {
		return nil, err
	}
	headTree, err := r.Store.GetTree(hv.Tree)
	if err != nil {
		return nil, err
	}

	// 5. 拼接 op。
	allOps, err := r.WS.CollectOps(fold)
	if err != nil {
		return nil, err
	}
	// 无改动时不允许推进主干 —— 否则会静默产出一个「空版本」，
	// 而它的树会覆盖整个仓库（见 deep_test.go 的回归）。
	if len(allOps) == 0 {
		return &LandResult{OpsCount: 0, CanLand: false}, ErrNotDirty
	}

	// 5b. 若对侧已把某文件重命名并落地，本侧改的还是旧路径 —— 跟随到新路径
	// （§6.3 rename_vs_edit）。只在「单工作区折叠」时构建：多工作区栈里各
	// 自的基准不同，路径映射会互相干扰。
	var follow map[string]string
	if len(fold) == 1 && !fold[0].BaseTree.IsZero() && fold[0].BaseTree != hv.Tree {
		baseTree, err := r.Store.GetTree(fold[0].BaseTree)
		if err != nil {
			return nil, err
		}
		if follow, err = op.RenameFollows(r.Store, baseTree, headTree); err != nil {
			return nil, err
		}
	}

	// 6. 重放。
	leaf := fold[len(fold)-1]
	res, err := op.Replay(headTree, allOps, r.Store, op.Options{
		WsID:       leaf.ID,
		MaxMatches: op.DefaultMaxMatches,
		Resolver:   r.DB.Resolver(),
		Candidates: true,
		Follow:     follow,
	})
	if err != nil {
		return nil, err
	}

	out := &LandResult{
		OpsCount:   len(allOps),
		Conflicts:  res.Conflicts,
		Files:      summarize(allOps),
		CanLand:    len(res.Conflicts) == 0,
		AutoMerged: res.AutoMerged,
	}
	if len(res.Conflicts) > 0 {
		return out, nil
	}

	// 8. 生成 version。
	rp, err := r.DB.GetRepo()
	if err != nil {
		return nil, err
	}
	epoch := uint64(rp.VersionCount / int64(r.epochSize))
	agent := leaf.Name
	if agent == "" {
		agent = "p37"
	}
	v := version.New(head, res.Tree.ID(), uint64(epoch), agent,
		leaf.TaskDesc, time.Now().UnixMilli(), leaf.TaskDesc)
	out.VersionID = v.ID()

	if !commit {
		return out, nil
	}

	// 3b. 采集成功后置 integrating。
	now := time.Now().UnixMilli()
	for _, w := range fold {
		if err := r.DB.UpdateWorkspaceState(w.ID, meta.StateIntegrating, now); err != nil {
			return nil, err
		}
	}

	// 10. 元数据提交（可回滚）。
	foldIDs := make([]string, len(fold))
	for i, w := range fold {
		foldIDs[i] = w.ID
	}
	j := meta.JournalEntry{
		VersionID: v.ID().String(),
		WsID:      leaf.ID,
		Agent:     agent,
		TaskDesc:  r.journalTask(leaf.TaskDesc),
		TS:        v.TS,
		OpsJSON:   journalOps(allOps),
	}
	if err := r.DB.CommitLand(v, j, foldIDs); err != nil {
		return nil, err
	}

	// 9. 唯一的提交点：推进 head。
	if err := r.writeHead(v.ID()); err != nil {
		// 回滚，保持一致性。
		_ = r.DB.RollbackLand(v.ID().String())
		return nil, err
	}
	if err := r.DB.SetMainlineHead(v.ID(), rp.VersionCount+1, uint64(epoch)); err != nil {
		return nil, err
	}

	// 11. 焚毁折叠集合 + 12. 重挂残留子工作区。
	children, err := r.DB.Children(leaf.ID)
	if err != nil {
		return nil, err
	}
	for _, c := range children {
		if err := r.DB.UpdateWorkspaceParent(c.ID, meta.ParentVersion, v.ID().String(), now); err != nil {
			return nil, err
		}
	}
	for _, w := range fold {
		overlay := w.Materialize == meta.MaterializeOverlay
		_ = r.MM.Close(w.ID, false, overlay)
	}
	if err := r.DB.FinishLand(foldIDs); err != nil {
		return nil, err
	}
	return out, nil
}

// Land 把折叠集合折叠为恰好一个 version（§7.2）。
//
// 并发：先取一个队列名额（满则立即返回 LAND_QUEUE_FULL），再取全局 land 锁
// 串行化整条流水线（§7.4）。
func (r *Repo) Land(wsID string) (*LandResult, error) {
	release, err := r.enterLand()
	if err != nil {
		return nil, err
	}
	defer release()

	r.landMu.Lock()
	defer r.landMu.Unlock()

	res, err := r.plan(wsID, true)
	if err != nil {
		return nil, err
	}
	if len(res.Conflicts) > 0 {
		if err := r.persistConflicts(wsID, res.Conflicts); err != nil {
			return nil, err
		}
		if err := r.DB.UpdateWorkspaceState(wsID, meta.StateConflicted, time.Now().UnixMilli()); err != nil {
			return nil, err
		}
		return res, ErrLandConflict
	}
	_ = r.DB.ClearConflicts(wsID)
	return res, nil
}

// Revert 以新 version 承载对某个 version 的逆变更（§7.6）。
//
// 与 Land 共用同一条串行化路径：两者都会推进 head。
func (r *Repo) Revert(versionID hash.ID) (hash.ID, error) {
	release, err := r.enterLand()
	if err != nil {
		return hash.ID{}, err
	}
	defer release()

	r.landMu.Lock()
	defer r.landMu.Unlock()

	target, err := r.DB.GetVersion(versionID)
	if err != nil {
		return hash.ID{}, err
	}
	if target.IsFirst() {
		return hash.ID{}, errors.New("repo: 首版本不可 revert")
	}
	parent, err := r.DB.GetVersion(target.Parent)
	if err != nil {
		return hash.ID{}, err
	}
	baseTree, err := r.Store.GetTree(parent.Tree)
	if err != nil {
		return hash.ID{}, err
	}
	targetTree, err := r.Store.GetTree(target.Tree)
	if err != nil {
		return hash.ID{}, err
	}

	// 正向 op，再取逆。
	fwd, err := r.captureDiff(parent.Tree, target.Tree)
	if err != nil {
		return hash.ID{}, err
	}
	inv := invert(fwd)

	head, ok, err := r.DB.Head()
	if err != nil {
		return hash.ID{}, err
	}
	if !ok {
		return hash.ID{}, ErrNotInitialized
	}
	hv, err := r.DB.GetVersion(head)
	if err != nil {
		return hash.ID{}, err
	}
	headTree, err := r.Store.GetTree(hv.Tree)
	if err != nil {
		return hash.ID{}, err
	}
	_ = baseTree
	_ = targetTree

	res, err := op.Replay(headTree, inv, r.Store, op.Options{
		WsID: "revert", MaxMatches: op.DefaultMaxMatches, Resolver: r.DB.Resolver(),
	})
	if err != nil {
		return hash.ID{}, err
	}
	if len(res.Conflicts) > 0 {
		return hash.ID{}, fmt.Errorf("%w: revert 产生 %d 个冲突", ErrLandConflict, len(res.Conflicts))
	}

	rp, err := r.DB.GetRepo()
	if err != nil {
		return hash.ID{}, err
	}
	epoch := uint64(rp.VersionCount / int64(r.epochSize))
	msg := "revert " + versionID.String()[:16]
	v := version.New(head, res.Tree.ID(), uint64(epoch), "p37-revert", msg,
		time.Now().UnixMilli(), msg)
	j := meta.JournalEntry{
		VersionID: v.ID().String(), Agent: "p37-revert", TaskDesc: r.journalTask(msg),
		TS: v.TS, OpsJSON: journalOps(inv),
	}
	if err := r.DB.CommitLand(v, j, nil); err != nil {
		return hash.ID{}, err
	}
	if err := r.writeHead(v.ID()); err != nil {
		_ = r.DB.RollbackLand(v.ID().String())
		return hash.ID{}, err
	}
	if err := r.DB.SetMainlineHead(v.ID(), rp.VersionCount+1, uint64(epoch)); err != nil {
		return hash.ID{}, err
	}
	return v.ID(), nil
}

// captureDiff 复用 capture.Diff（由两棵树推导 op）。
func (r *Repo) captureDiff(baseID, nextID hash.ID) ([]op.Op, error) {
	bt, err := r.Store.GetTree(baseID)
	if err != nil {
		return nil, err
	}
	nt, err := r.Store.GetTree(nextID)
	if err != nil {
		return nil, err
	}
	return captureDiff(r.Store, "revert", bt, nt, r.CaptureOpt)
}

// invert 把一组 op 取逆（§7.6）。
func invert(ops []op.Op) []op.Op {
	out := make([]op.Op, 0, len(ops))
	for i := len(ops) - 1; i >= 0; i-- {
		o := ops[i]
		o.ModeBefore, o.ModeAfter = o.ModeAfter, o.ModeBefore
		switch o.Kind {
		case op.KindCreate:
			o.Kind = op.KindDelete
			o.BaseFile = o.NewFile
			o.NewFile = hash.ID{}
		case op.KindDelete:
			o.Kind = op.KindCreate
			o.NewFile = o.BaseFile
			o.BaseFile = hash.ID{}
		case op.KindModify:
			for j := range o.Hunks {
				h := &o.Hunks[j]
				h.Insert, h.Remove = h.Remove, h.Insert
				// 前后上下文互换：删除/插入之后的位置关系改变，退回空上下文由
				// place 的锚点搜索处理。
				h.Before, h.After = nil, nil
			}
			o.BaseFile, o.NewFile = o.NewFile, o.BaseFile
		case op.KindBlobPut, op.KindAttr:
			o.BaseFile, o.NewFile = o.NewFile, o.BaseFile
		case op.KindRename:
			o.Path, o.DestPath = o.DestPath, o.Path
		}
		out = append(out, o)
	}
	return out
}

// summarize 统计 op 的文件级影响。
func summarize(ops []op.Op) []FileStat {
	byPath := map[string]*FileStat{}
	order := []string{}
	for i := range ops {
		o := &ops[i]
		key := o.Path
		fs, ok := byPath[key]
		if !ok {
			fs = &FileStat{Path: o.Path, Kind: string(o.Kind)}
			byPath[key] = fs
			order = append(order, key)
		}
		fs.Kind = string(o.Kind)
		if o.DestPath != "" {
			fs.DestPath = o.DestPath
		}
		for _, h := range o.Hunks {
			fs.Added += len(h.Insert)
			fs.Deleted += len(h.Remove)
		}
		switch o.Kind {
		case op.KindCreate, op.KindBlobPut:
			if o.Kind == op.KindCreate {
				fs.Added++
			}
		case op.KindDelete:
			fs.Deleted++
		}
	}
	sort.Strings(order)
	out := make([]FileStat, 0, len(order))
	for _, k := range order {
		out = append(out, *byPath[k])
	}
	return out
}

func linesJSON(ls [][]byte) string {
	return toJSONLines(ls)
}

func sideJSON(s *op.Side) string {
	var lines []string
	for _, h := range s.Hunks {
		for _, l := range h.Remove {
			lines = append(lines, string(l))
		}
		for _, l := range h.Insert {
			lines = append(lines, string(l))
		}
	}
	return toJSONStrings(lines)
}

func candsJSON(cs []op.Candidate) string {
	if len(cs) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, fmt.Sprintf(`{"kind":%q,"note":%q}`, string(c.Kind), c.Note))
	}
	return "[" + strings.Join(parts, ",") + "]"
}
