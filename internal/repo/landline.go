package repo

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/chenyiwen/p37/internal/capture"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/op"
	"github.com/chenyiwen/p37/internal/version"
	"github.com/chenyiwen/p37/internal/workspace"
)

// ErrHasActiveChildren 表示工作区仍有活跃子工作区，不能直接 land 到 line。
var ErrHasActiveChildren = errors.New("repo: 工作区仍有活跃子工作区")
var ErrTargetInvalid = errors.New("repo: 非法 land 目标")
var ErrPreviewExpired = errors.New("repo: land_into preview 已过期")

// ChildRef 是被父工作区 land 阻塞的子工作区。
type ChildRef struct {
	WSID  string `json:"ws_id"`
	State string `json:"state"`
}

// ActiveChildrenError 让 ws_land 的 ws_has_active_children 带上结构化列表，
// 调用方可以据此决定「先解冲突」还是「显式 drop 子工作区」。
type ActiveChildrenError struct {
	WSID     string
	Children []ChildRef
}

func (e *ActiveChildrenError) Error() string {
	ids := make([]string, 0, len(e.Children))
	for _, c := range e.Children {
		ids = append(ids, c.WSID+"("+c.State+")")
	}
	return fmt.Sprintf("%s: %s", ErrHasActiveChildren, strings.Join(ids, ", "))
}

// Unwrap 保持 errors.Is(err, ErrHasActiveChildren) 成立。
func (e *ActiveChildrenError) Unwrap() error { return ErrHasActiveChildren }

// dropAfterLand 在 land 已经提交之后清理工作区。
//
// 提交成功后再返回错误会让调用方误以为 land 失败，因此这里只记录日志并把
// 警告带回 LandResult；配额回收由 workspace.Drop 保证（元数据先删）。
func (r *Repo) dropAfterLand(res *LandResult, wsID string, keepDir bool) {
	dropped, _, err := r.WS.Drop(wsID, true, keepDir)
	if err == nil || errors.Is(err, workspace.ErrNotFound) {
		_ = dropped
		return
	}
	msg := fmt.Sprintf("land 已提交，但清理工作区 %s 失败: %v", wsID, err)
	log.Printf("p37: %s", msg)
	if res != nil {
		res.Warnings = append(res.Warnings, msg)
	}
}

// LandOptions 控制 land 行为。
type LandOptions struct {
	Line               string
	Fold               string // self | stack
	DropWorkspace      bool
	ExpectedHead       string
	ExpectedWSTree     string
	ExpectedWSOpsEpoch int64
	IdempotencyKey     string
	TaskDesc           string
}

// LandTo 把工作区折叠到指定命名 line。
func (r *Repo) LandTo(wsID string, opt LandOptions) (*LandResult, error) {
	release, err := r.enterLand()
	if err != nil {
		return nil, err
	}
	defer release()
	r.landMu.Lock()
	defer r.landMu.Unlock()
	return r.landToLocked(wsID, opt, true)
}

// PreviewTo 预演 land 到 line，不推进 head。
func (r *Repo) PreviewTo(wsID string, opt LandOptions) (*LandResult, error) {
	r.landMu.Lock()
	defer r.landMu.Unlock()
	return r.landToLocked(wsID, opt, false)
}

func (r *Repo) landToLocked(wsID string, opt LandOptions, commit bool) (*LandResult, error) {
	if opt.Line == "" {
		def, err := r.DB.DefaultLine()
		if err != nil {
			return nil, err
		}
		opt.Line = def
	}
	if opt.Fold == "" {
		opt.Fold = "self"
	}
	if opt.Fold != "self" && opt.Fold != "stack" {
		return nil, fmt.Errorf("repo: 非法 fold %q", opt.Fold)
	}
	line, err := r.Line(opt.Line)
	if err != nil {
		return nil, err
	}
	if opt.IdempotencyKey != "" {
		if v, _ := r.DB.FindVersionByIdempotency("ws_land", opt.IdempotencyKey, time.Now().UnixMilli()); v != nil {
			head, _ := r.DB.LineHead(opt.Line)
			return &LandResult{CanLand: true, AlreadyLanded: true, VersionID: v.ID(),
				TargetHead: head, State: "already_landed"}, nil
		}
	}
	// 幂等：工作区已 land 过直接返回原版本（即使工作区已被销毁）。
	if vid, landed, err := r.DB.GetLandedWS(wsID); err != nil {
		return nil, err
	} else if landed {
		id, _ := hash.Parse(vid)
		return &LandResult{CanLand: true, AlreadyLanded: true, VersionID: id,
			TargetHead: line.Head, State: "already_landed"}, nil
	}
	w, err := r.WS.Get(wsID)
	if err != nil {
		return nil, err
	}
	if w.Hidden || w.Kind == "merge" {
		return nil, fmt.Errorf("%w: 隐藏工作区不能直接 land", workspace.ErrWrongState)
	}
	if opt.Fold == "self" && w.ParentKind == meta.ParentWorkspace {
		return nil, fmt.Errorf("%w: 子工作区必须用 ws_land_into 或 fold=stack", ErrTargetInvalid)
	}

	var fold []*meta.Workspace
	if opt.Fold == "stack" {
		fold, err = r.WS.FoldSet(wsID)
		if err != nil {
			return nil, err
		}
	} else {
		fold = []*meta.Workspace{w}
	}
	// fold=self 时，活跃子工作区必须先把成果交回父工作区。
	if opt.Fold == "self" {
		kids, err := r.DB.Children(wsID)
		if err != nil {
			return nil, err
		}
		var blockers []ChildRef
		for i := range kids {
			k := &kids[i]
			if k.Hidden || k.LandedTS != 0 {
				continue
			}
			switch k.State {
			case meta.StateActive, meta.StateConflicted, meta.StateRebasing:
				blockers = append(blockers, ChildRef{WSID: k.ID, State: k.State})
			}
		}
		if len(blockers) > 0 {
			return nil, &ActiveChildrenError{WSID: wsID, Children: blockers}
		}
	}

	for _, fw := range fold {
		if err := r.WS.Sync(fw.ID); err != nil {
			return nil, err
		}
	}
	// 预览令牌：工作区有效树 / ops epoch。
	wsTree, err := r.WS.EffectiveTree(wsID)
	if err != nil {
		return nil, err
	}
	opsCount, _ := r.DB.CountOps(wsID)
	res := &LandResult{WSTree: wsTree, WSOpsEpoch: opsCount, TargetHead: line.Head,
		PreviewID: wsID}
	if opt.ExpectedWSTree != "" {
		exp, err := hash.Parse(opt.ExpectedWSTree)
		if err != nil || exp != wsTree {
			return nil, fmt.Errorf("%w: workspace tree 已变化", ErrTargetMoved)
		}
	}
	if opt.ExpectedWSOpsEpoch > 0 && opt.ExpectedWSOpsEpoch != opsCount {
		return nil, fmt.Errorf("%w: workspace ops 已变化", ErrTargetMoved)
	}
	if opt.ExpectedHead != "" {
		exp, err := r.resolveVersionRef(opt.ExpectedHead)
		if err != nil {
			return nil, err
		}
		if exp != line.Head {
			return nil, fmt.Errorf("%w: current=%s", ErrTargetMoved, line.Head)
		}
	}

	headV, err := r.DB.GetVersion(line.Head)
	if err != nil {
		return nil, err
	}
	headTree, err := r.Store.GetTree(headV.Tree)
	if err != nil {
		return nil, err
	}
	allOps, err := r.WS.CollectOps(fold)
	if err != nil {
		return nil, err
	}
	if len(allOps) == 0 {
		res.CanLand = false
		return res, ErrNotDirty
	}
	var follow map[string]string
	if len(fold) == 1 && !fold[0].BaseTree.IsZero() && fold[0].BaseTree != headV.Tree {
		baseTree, err := r.Store.GetTree(fold[0].BaseTree)
		if err != nil {
			return nil, err
		}
		if follow, err = op.RenameFollows(r.Store, baseTree, headTree); err != nil {
			return nil, err
		}
	}
	replayed, err := op.Replay(headTree, allOps, r.Store, op.Options{
		WsID: wsID, MaxMatches: op.DefaultMaxMatches,
		Resolver: r.DB.Resolver(), Candidates: true, Follow: follow,
	})
	if err != nil {
		return nil, err
	}
	res.OpsCount = len(allOps)
	res.Conflicts = replayed.Conflicts
	res.Files = summarize(allOps)
	res.CanLand = len(replayed.Conflicts) == 0
	res.AutoMerged = replayed.AutoMerged
	if len(replayed.Conflicts) > 0 {
		res.State = "conflict"
		if err := r.persistConflicts(wsID, replayed.Conflicts); err != nil {
			return nil, err
		}
		if commit {
			_ = r.DB.UpdateWorkspaceState(wsID, meta.StateConflicted, time.Now().UnixMilli())
		}
		return res, ErrLandConflict
	}
	if !commit {
		return res, nil
	}
	leaf := fold[len(fold)-1]
	rp, err := r.DB.GetRepo()
	if err != nil {
		return nil, err
	}
	epoch := uint64(rp.VersionCount / int64(r.epochSize))
	agent := leaf.Name
	if agent == "" {
		agent = "p37"
	}
	task := leaf.TaskDesc
	if opt.TaskDesc != "" {
		task = opt.TaskDesc
	}
	v := version.New(line.Head, replayed.Tree.ID(), epoch, agent, task,
		time.Now().UnixMilli(), task)
	res.VersionID = v.ID()
	j := meta.JournalEntry{
		VersionID: v.ID().String(), WsID: leaf.ID, Agent: agent,
		TaskDesc: r.journalTask(task), TS: v.TS, OpsJSON: journalOps(allOps),
	}
	foldIDs := make([]string, len(fold))
	for i, fw := range fold {
		foldIDs[i] = fw.ID
	}
	if err := r.DB.CommitLineLand(v, j, opt.Line, foldIDs, "ws_land", opt.IdempotencyKey, line.Head); err != nil {
		if errors.Is(err, meta.ErrLineHeadMoved) {
			return nil, fmt.Errorf("%w", ErrTargetMoved)
		}
		return nil, err
	}
	res.TargetHead = v.ID()
	res.State = "done"
	// drop 或保留为只读 landed。
	if opt.DropWorkspace {
		for _, fw := range fold {
			overlay := fw.Materialize == meta.MaterializeOverlay
			if err := r.MM.Close(fw.ID, false, overlay); err != nil {
				msg := fmt.Sprintf("land 已提交，但清理工作区 %s 目录失败: %v", fw.ID, err)
				log.Printf("p37: %s", msg)
				res.Warnings = append(res.Warnings, msg)
			}
		}
		if err := r.DB.FinishLand(foldIDs); err != nil {
			return nil, err
		}
	} else {
		for _, fw := range fold {
			_ = r.WS.MarkLanded(fw.ID)
		}
	}
	return res, nil
}

// LandResult 的扩展字段在 land.go 中定义；这里只补方法所需的辅助。

// PreviewInto 预演把 child 的变更并入 parent workspace。
func (r *Repo) PreviewInto(wsID, parentWsID string) (*LandResult, error) {
	r.landMu.Lock()
	defer r.landMu.Unlock()
	return r.landIntoLocked(wsID, parentWsID, true, "", "", "")
}

// LandInto 把 child 的变更并入直接父工作区，不产生版本。
func (r *Repo) LandInto(wsID, parentWsID, expectedParentTree, previewID, idemKey string) (*LandResult, error) {
	if vid, landed, err := r.DB.GetLandedWS(wsID); err == nil && landed {
		id, _ := hash.Parse(vid)
		return &LandResult{CanLand: true, AlreadyLanded: true, TargetHead: id,
			State: "already_landed"}, nil
	}
	release, err := r.enterLand()
	if err != nil {
		return nil, err
	}
	defer release()
	r.landMu.Lock()
	defer r.landMu.Unlock()
	return r.landIntoLocked(wsID, parentWsID, false, expectedParentTree, previewID, idemKey)
}

func (r *Repo) landIntoLocked(wsID, parentWsID string, preview bool,
	expectedParentTree, previewID, idemKey string) (*LandResult, error) {
	// 兼容旧调用方：如果传入的是历史 hidden preview 工作区（mg_*），仍然
	// 做 24h 过期校验；新协议下 preview_id == 子工作区 id，不再单独建 preview。
	if previewID != "" && previewID != wsID {
		if pw, err := r.DB.GetWorkspace(previewID); err == nil {
			if time.Now().UnixMilli()-pw.CreatedTS > int64(24*time.Hour/time.Millisecond) {
				return nil, ErrPreviewExpired
			}
		}
	}
	child, err := r.WS.Get(wsID)
	if err != nil {
		return nil, err
	}
	if child.ParentKind != meta.ParentWorkspace || child.ParentID != parentWsID {
		return nil, fmt.Errorf("%w: child 的直接父不是 %s", ErrTargetMoved, parentWsID)
	}
	// conflicted 允许重试：冲突解决后直接再调 ws_land_into 即可，
	// 不必先让调用方猜状态。真正的冲突会在重放后再次返回。
	if child.State != meta.StateActive && child.State != meta.StateConflicted {
		return nil, fmt.Errorf("%w: child 状态为 %s", workspace.ErrWrongState, child.State)
	}
	parent, err := r.WS.Get(parentWsID)
	if err != nil {
		return nil, err
	}
	if parent.Hidden || parent.Kind == "merge" || parent.LandedTS != 0 {
		return nil, fmt.Errorf("%w: 父工作区不可写", workspace.ErrWrongState)
	}
	if parent.State != meta.StateActive {
		return nil, fmt.Errorf("%w: 父工作区状态为 %s", workspace.ErrWrongState, parent.State)
	}
	if err := r.WS.Sync(parentWsID); err != nil {
		return nil, err
	}
	if err := r.WS.Sync(wsID); err != nil {
		return nil, err
	}
	childTree, err := r.WS.EffectiveTree(wsID)
	if err != nil {
		return nil, err
	}
	childBase, err := r.Store.GetTree(child.BaseTree)
	if err != nil {
		return nil, err
	}
	childEffective, err := r.Store.GetTree(childTree)
	if err != nil {
		return nil, err
	}
	parentTree, err := r.WS.EffectiveTree(parentWsID)
	if err != nil {
		return nil, err
	}
	if expectedParentTree != "" {
		exp, err := hash.Parse(expectedParentTree)
		if err != nil || exp != parentTree {
			return nil, fmt.Errorf("%w: parent tree 已变化", ErrTargetMoved)
		}
	}
	ops, err := capture.Diff("__land_into__", childBase, childEffective, r.Store, r.CaptureOpt)
	if err != nil {
		return nil, err
	}
	parentTreeObj, err := r.Store.GetTree(parentTree)
	if err != nil {
		return nil, err
	}
	// 冲突 key 含 WsID：这里必须**恒用子工作区 id**，不能因为调用方传了
	// preview_id 就换 key 域。否则「preview → resolve → 带 preview_id 重试」
	// 会算出新 key，解决记录永远匹配不上（沙盒 #66.2 的死循环）。
	replayed, err := op.Replay(parentTreeObj, ops, r.Store, op.Options{
		WsID: wsID, MaxMatches: op.DefaultMaxMatches,
		Resolver: r.DB.Resolver(), Candidates: true,
	})
	if err != nil {
		return nil, err
	}
	res := &LandResult{
		OpsCount: len(ops), Files: summarize(ops), Conflicts: replayed.Conflicts,
		CanLand: len(replayed.Conflicts) == 0, AutoMerged: replayed.AutoMerged,
		TargetHead: parentTree, WSTree: childTree, PreviewID: wsID,
	}
	if len(replayed.Conflicts) > 0 {
		// 冲突挂在**子工作区**上：conflict_list(child) / conflict_resolve_all(child)
		// 直接可用，不再依赖调用方记住 hidden preview id。
		if err := r.persistConflicts(wsID, replayed.Conflicts); err != nil {
			return nil, err
		}
		res.State = "conflict"
		if !preview {
			if err := r.DB.UpdateWorkspaceState(wsID, meta.StateConflicted, time.Now().UnixMilli()); err != nil {
				return nil, err
			}
		}
		return res, nil
	}
	if preview {
		return res, nil
	}
	// 提交：先写 journal，再把结果树写回父工作区目录并重新采集父 ops。
	if err := r.DB.InsertLandIntoJournal(wsID, parentWsID, parentTree.String(),
		replayed.Tree.ID().String()); err != nil {
		return nil, err
	}
	if err := r.WS.ApplyTree(parentWsID, replayed.Tree); err != nil {
		return nil, err
	}
	res.TargetHead = replayed.Tree.ID()
	res.State = "done"
	if err := r.DB.InsertLandedWS(wsID, replayed.Tree.ID().String(), time.Now().UnixMilli()); err != nil {
		return nil, err
	}
	_ = r.DB.ClearConflicts(wsID)
	r.dropAfterLand(res, wsID, false)
	if js, err := r.DB.ListLandIntoJournal(); err == nil {
		for _, j := range js {
			if j.WsID == wsID && j.ParentWsID == parentWsID {
				_ = r.DB.DeleteLandIntoJournal(j.ID)
			}
		}
	}
	// 老协议遗留的 hidden preview：成功时顺手清掉。
	if previewID != "" && previewID != wsID {
		_ = r.DB.ClearConflicts(previewID)
		r.dropAfterLand(res, previewID, true)
	}
	if idemKey != "" {
		r.putIdem("ws_land_into", idemKey, res)
	}
	return res, nil
}

// reconcileLandInto 在启动时补做未完成的 ws_land_into。
//
// 提交协议：journal(before_tree,result_tree) → 写父目录并重算 ops → 删 journal。
// 崩溃后如果 result_tree 对象仍在，重放 ApplyTree 是幂等的；失败则保留 journal。
func (r *Repo) reconcileLandInto() error {
	rows, err := r.DB.ListLandIntoJournal()
	if err != nil {
		return err
	}
	for _, j := range rows {
		if _, err := r.DB.GetWorkspace(j.ParentWsID); err != nil {
			_ = r.DB.DeleteLandIntoJournal(j.ID)
			continue
		}
		tid, err := hash.Parse(j.ResultTree)
		if err != nil {
			continue
		}
		target, err := r.Store.GetTree(tid)
		if err != nil {
			continue
		}
		if err := r.WS.ApplyTree(j.ParentWsID, target); err != nil {
			continue
		}
		_ = r.DB.DeleteLandIntoJournal(j.ID)
	}
	return nil
}
