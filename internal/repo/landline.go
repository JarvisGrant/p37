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

// ErrParentMoved 表示父工作区在「读树 → 重放 → 写盘」窗口里被改动。
//
// 调用方（P34）应重读父有效树、重跑 preview 后重试；绝不能忽略后继续写。
var ErrParentMoved = errors.New("repo: 父工作区在合并期间被改动")

// ErrPathNotAllowed 表示子工作区改了工作卡允许范围之外的路径。
var ErrPathNotAllowed = errors.New("repo: 子工作区改动了未授权路径")

// ErrWSUnstable 表示采集时有文件正在被写，本轮看不清它的内容。
var ErrWSUnstable = errors.New("repo: 工作区存在不稳定文件")

// ErrLandInvariant 表示重放结果触碰了子工作区没有改动的路径。
//
// 这是实现缺陷的护栏：宁可拒绝合并，也不要把「子会话没改的文件」写回父工作区。
var ErrLandInvariant = errors.New("repo: land 结果越过了子工作区的改动集合")

// ParentMovedError 携带被并发改动的具体路径。
type ParentMovedError struct {
	WSID     string
	Paths    []string
	Expected string
	Current  string
}

func (e *ParentMovedError) Error() string {
	msg := fmt.Sprintf("%s: %s", ErrParentMoved, e.WSID)
	if len(e.Paths) > 0 {
		msg += fmt.Sprintf("（路径: %s）", strings.Join(e.Paths, ", "))
	}
	return msg
}

func (e *ParentMovedError) Unwrap() error { return ErrParentMoved }

// PathNotAllowedError 列出越界路径，供 P34 直接回显给主管。
type PathNotAllowedError struct {
	Paths []string
}

func (e *PathNotAllowedError) Error() string {
	return fmt.Sprintf("%s: %s", ErrPathNotAllowed, strings.Join(e.Paths, ", "))
}

func (e *PathNotAllowedError) Unwrap() error { return ErrPathNotAllowed }

// UnstableWSError 列出本轮无法稳定读取的路径。
type UnstableWSError struct {
	WSID  string
	Paths []string
}

func (e *UnstableWSError) Error() string {
	return fmt.Sprintf("%s: %s（路径: %s）", ErrWSUnstable, e.WSID, strings.Join(e.Paths, ", "))
}

func (e *UnstableWSError) Unwrap() error { return ErrWSUnstable }

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
		req := map[string]string{
			"idempotency_key": opt.IdempotencyKey, "ws_id": wsID,
			"target": opt.Line, "fold": opt.Fold,
		}
		if storedReq, res, ok := r.getLandIdem("ws_land", opt.IdempotencyKey); ok {
			if err := checkLandIdem(req, storedReq); err != nil {
				return nil, err
			}
			if res == nil {
				res = &LandResult{}
			}
			res.AlreadyLanded, res.Replayed, res.State = true, true, "already_landed"
			res.Line = opt.Line
			if head, err := r.DB.LineHead(opt.Line); err == nil {
				res.CurrentLineHead = head
			}
			return res, nil
		}
		if v, _ := r.DB.FindVersionByIdempotency("ws_land", opt.IdempotencyKey, time.Now().UnixMilli()); v != nil {
			head, _ := r.DB.LineHead(opt.Line)
			return &LandResult{CanLand: true, AlreadyLanded: true, VersionID: v.ID(),
				LandedVersion: v.ID(), TargetHead: head, CurrentLineHead: head,
				State: "already_landed", Line: opt.Line}, nil
		}
	}
	// 幂等：工作区已 land 过直接返回原版本（即使工作区已被销毁）。
	if vid, landed, err := r.DB.GetLandedWS(wsID); err != nil {
		return nil, err
	} else if landed {
		id, _ := hash.Parse(vid)
		return &LandResult{CanLand: true, AlreadyLanded: true, VersionID: id,
			LandedVersion: id, TargetHead: line.Head, CurrentLineHead: line.Head,
			State: "already_landed", Line: opt.Line}, nil
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

	// 折叠集合可能包含多个工作区：按 ws_id 升序一次性锁住，避免 rebase 在
	// 「采集 → 读有效树 → 读 ops」之间改写。
	lockIDs := make([]string, 0, len(fold))
	for _, fw := range fold {
		lockIDs = append(lockIDs, fw.ID)
	}
	unlockFold := r.WS.LockWorkspaces(lockIDs)
	defer unlockFold()
	for _, fw := range fold {
		if err := r.WS.SyncLocked(fw.ID); err != nil {
			return nil, err
		}
	}
	wsTree, err := r.WS.EffectiveTreeLocked(wsID)
	if err != nil {
		return nil, err
	}
	wsEpoch := int64(0)
	if w, werr := r.WS.Get(wsID); werr == nil {
		wsEpoch = w.OpsEpoch
	}
	res := &LandResult{WSTree: wsTree, WSOpsEpoch: wsEpoch, TargetHead: line.Head,
		PreviewID: wsID, ChildTree: wsTree, BaseTree: fold[0].BaseTree}
	if opt.ExpectedWSTree != "" {
		exp, err := hash.Parse(opt.ExpectedWSTree)
		if err != nil || exp != wsTree {
			return nil, fmt.Errorf("%w: workspace tree 已变化", ErrTargetMoved)
		}
	}
	if opt.ExpectedWSOpsEpoch > 0 && opt.ExpectedWSOpsEpoch != wsEpoch {
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
		for _, id := range foldIDs {
			r.WS.ReleaseNameClaims(id)
		}
	} else {
		for _, fw := range fold {
			_ = r.WS.MarkLanded(fw.ID)
		}
	}
	res.LandedVersion = v.ID()
	res.Line = opt.Line
	r.putLandIdem("ws_land", opt.IdempotencyKey, map[string]string{
		"idempotency_key": opt.IdempotencyKey, "ws_id": wsID,
		"target": opt.Line, "fold": opt.Fold,
	}, res)
	return res, nil
}

// LandResult 的扩展字段在 land.go 中定义；这里只补方法所需的辅助。

// PreviewInto 预演把 child 的变更并入 parent workspace。
func (r *Repo) PreviewInto(wsID, parentWsID string) (*LandResult, error) {
	return r.PreviewIntoWithOptions(wsID, LandIntoOptions{ParentWsID: parentWsID})
}

// PreviewIntoWithOptions 是带完整参数的 dry-run。
func (r *Repo) PreviewIntoWithOptions(wsID string, opt LandIntoOptions) (*LandResult, error) {
	opt.Preview = true
	r.landMu.Lock()
	defer r.landMu.Unlock()
	return r.landIntoLocked(wsID, opt)
}

// LandInto 把 child 的变更并入直接父工作区，不产生版本。
func (r *Repo) LandInto(wsID, parentWsID, expectedParentTree, previewID, idemKey string) (*LandResult, error) {
	return r.LandIntoWithOptions(wsID, LandIntoOptions{
		ParentWsID:         parentWsID,
		ExpectedParentTree: expectedParentTree,
		PreviewID:          previewID,
		IdempotencyKey:     idemKey,
	})
}

// LandIntoOptions 是 ws_land_into 的完整参数。
type LandIntoOptions struct {
	ParentWsID         string
	ExpectedParentTree string
	ExpectedChildTree  string
	ExpectedBaseTree   string
	PreviewID          string
	IdempotencyKey     string
	// Preview=true 时只计算、不落盘、不改子工作区状态。
	Preview bool
	// AllowedPaths / AllowedPrefixes 是工作卡授权范围；空表示不限制。
	// 越界路径在 preview 里以 out_of_scope_paths 返回，在 land 时直接拒绝。
	AllowedPaths    []string
	AllowedPrefixes []string
	// StrictMerge=true 时「父子双方都改过的同一路径」一律出冲突，不做自动合并。
	StrictMerge bool
}

// LandIntoWithOptions 是 ws_land_into 的实现入口。
func (r *Repo) LandIntoWithOptions(wsID string, opt LandIntoOptions) (*LandResult, error) {
	parentWsID, idemKey := opt.ParentWsID, opt.IdempotencyKey
	checkIdem := func() (*LandResult, bool, error) {
		if idemKey == "" {
			return nil, false, nil
		}
		req := map[string]string{"idempotency_key": idemKey, "ws_id": wsID, "parent_ws_id": parentWsID}
		storedReq, res, ok := r.getLandIdem("ws_land_into", idemKey)
		if !ok {
			return nil, false, nil
		}
		if err := checkLandIdem(req, storedReq); err != nil {
			return nil, false, err
		}
		if res == nil {
			res = &LandResult{}
		}
		res.AlreadyLanded, res.Replayed, res.State = true, true, "already_landed"
		res.ParentWSID = parentWsID
		return res, true, nil
	}
	checkLanded := func() (*LandResult, bool) {
		info, landed, err := r.DB.GetLandedWSInfo(wsID)
		if err != nil || !landed {
			return nil, false
		}
		id, _ := hash.Parse(info.VersionID)
		return &LandResult{CanLand: true, AlreadyLanded: true, TargetHead: id,
			WSTree: id, PreviewID: wsID, ParentWSID: parentWsID,
			State: "already_landed"}, true
	}
	if res, ok, err := checkIdem(); err != nil {
		return nil, err
	} else if ok {
		return res, nil
	}
	if res, ok := checkLanded(); ok {
		return res, nil
	}
	release, err := r.enterLand()
	if err != nil {
		return nil, err
	}
	defer release()
	r.landMu.Lock()
	defer r.landMu.Unlock()
	// 拿到 landMu 之后再查一次幂等/已落地：并发同 key 的两个请求都可能在
	// 入口检查之后才到达这里，第二次必须重放而不是撞上「子工作区已删除」。
	if res, ok, err := checkIdem(); err != nil {
		return nil, err
	} else if ok {
		return res, nil
	}
	if res, ok := checkLanded(); ok {
		return res, nil
	}
	return r.landIntoLocked(wsID, opt)
}

func (r *Repo) landIntoLocked(wsID string, opt LandIntoOptions) (*LandResult, error) {
	preview, parentWsID := opt.Preview, opt.ParentWsID
	// 兼容旧调用方：如果传入的是历史 hidden preview 工作区（mg_*），仍然
	// 做 24h 过期校验；新协议下 preview_id == 子工作区 id，不再单独建 preview。
	if opt.PreviewID != "" && opt.PreviewID != wsID {
		if pw, err := r.DB.GetWorkspace(opt.PreviewID); err == nil {
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

	// 子 → 父持写锁：合并/校验/落盘期间禁止 ws_rebase / ws_drop / 另一次采集
	// 改写这两个工作区。所有 land 已由 landMu 串行化，因此不会出现反向持锁。
	res, err := func() (*LandResult, error) {
		unlockChild := r.WS.LockWorkspace(wsID)
		defer unlockChild()
		unlockParent := r.WS.LockWorkspace(parentWsID)
		defer unlockParent()
		return r.mergeIntoParentLocked(wsID, parentWsID, opt)
	}()
	if err != nil || res == nil {
		return res, err
	}
	if len(res.Conflicts) > 0 {
		// 冲突挂在**子工作区**上：conflict_list(child) / conflict_resolve_all(child)
		// 直接可用，不再依赖调用方记住 hidden preview id。
		if err := r.persistConflicts(wsID, res.Conflicts); err != nil {
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
	// 提交已经完成（mergeIntoParentLocked 内已写 journal、落盘、重采集）。
	res.ParentWSID = parentWsID
	r.dropAfterLand(res, wsID, false)
	r.cleanupLandIntoJournal(wsID, parentWsID)
	// 老协议遗留的 hidden preview：成功时顺手清掉。
	if opt.PreviewID != "" && opt.PreviewID != wsID {
		_ = r.DB.ClearConflicts(opt.PreviewID)
		r.dropAfterLand(res, opt.PreviewID, true)
	}
	r.putLandIdem("ws_land_into", opt.IdempotencyKey, map[string]string{
		"idempotency_key": opt.IdempotencyKey, "ws_id": wsID, "parent_ws_id": parentWsID,
	}, res)
	return res, nil
}

// mergeIntoParentLocked 在已持有子/父工作区写锁的前提下完成一次三方合并。
func (r *Repo) mergeIntoParentLocked(wsID, parentWsID string, opt LandIntoOptions) (*LandResult, error) {
	// 等锁期间状态可能变化，重新校验一次。
	child, err := r.WS.Get(wsID)
	if err != nil {
		return nil, err
	}
	if child.State != meta.StateActive && child.State != meta.StateConflicted {
		return nil, fmt.Errorf("%w: child 状态为 %s", workspace.ErrWrongState, child.State)
	}
	parent, err := r.WS.Get(parentWsID)
	if err != nil {
		return nil, err
	}
	if parent.Hidden || parent.Kind == "merge" || parent.LandedTS != 0 || parent.State != meta.StateActive {
		return nil, fmt.Errorf("%w: 父工作区不可写", workspace.ErrWrongState)
	}

	// land 的采集必须看全：禁用 stat/惰性快路径，并拒绝「正在被写」的文件。
	// 否则父的最后一笔改动会被当成未变，随后被结果树覆盖。
	if err := r.syncForLandLocked(wsID); err != nil {
		return nil, err
	}
	if err := r.syncForLandLocked(parentWsID); err != nil {
		return nil, err
	}
	// 重新读取：Sync 刚推进了 ops_epoch，结果里要报告同步后的版本。
	if child, err = r.WS.Get(wsID); err != nil {
		return nil, err
	}

	childTree, err := r.WS.EffectiveTreeLocked(wsID)
	if err != nil {
		return nil, err
	}
	parentTree, err := r.WS.EffectiveTreeLocked(parentWsID)
	if err != nil {
		return nil, err
	}
	if opt.ExpectedParentTree != "" {
		exp, perr := hash.Parse(opt.ExpectedParentTree)
		if perr != nil {
			return nil, fmt.Errorf("repo: expected_parent_tree 非法: %w", perr)
		}
		if exp != parentTree {
			return nil, &TokenMismatchError{Field: "parent_tree", Expected: exp.String(), Current: parentTree.String()}
		}
	}
	if opt.ExpectedChildTree != "" {
		exp, perr := hash.Parse(opt.ExpectedChildTree)
		if perr != nil {
			return nil, fmt.Errorf("repo: expected_child_tree 非法: %w", perr)
		}
		if exp != childTree {
			return nil, &TokenMismatchError{Field: "child_tree", Expected: exp.String(), Current: childTree.String()}
		}
	}
	if opt.ExpectedBaseTree != "" {
		exp, perr := hash.Parse(opt.ExpectedBaseTree)
		if perr != nil {
			return nil, fmt.Errorf("repo: expected_base_tree 非法: %w", perr)
		}
		if exp != child.BaseTree {
			return nil, &TokenMismatchError{Field: "base_tree", Expected: exp.String(), Current: child.BaseTree.String()}
		}
	}

	childBase, err := r.Store.GetTree(child.BaseTree)
	if err != nil {
		return nil, err
	}
	childEffective, err := r.Store.GetTree(childTree)
	if err != nil {
		return nil, err
	}
	parentTreeObj, err := r.Store.GetTree(parentTree)
	if err != nil {
		return nil, err
	}

	baseFlat, err := flattenStoreTree(r.Store, child.BaseTree)
	if err != nil {
		return nil, err
	}
	childFlat, err := flattenStoreTree(r.Store, childTree)
	if err != nil {
		return nil, err
	}
	parentFlat, err := flattenStoreTree(r.Store, parentTree)
	if err != nil {
		return nil, err
	}
	childChanged := diffFlatPaths(baseFlat, childFlat)
	parentChanged := diffFlatPaths(baseFlat, parentFlat)
	bothChanged := intersectionPaths(childChanged, parentChanged)

	outOfScope := outOfScopePaths(childChanged, opt.AllowedPaths, opt.AllowedPrefixes)
	if len(outOfScope) > 0 && !opt.Preview {
		return nil, &PathNotAllowedError{Paths: outOfScope}
	}

	ops, err := capture.Diff("__land_into__", childBase, childEffective, r.Store, r.CaptureOpt)
	if err != nil {
		return nil, err
	}
	var forceConflict map[string]bool
	if opt.StrictMerge && len(bothChanged) > 0 {
		forceConflict = make(map[string]bool, len(bothChanged))
		for _, p := range bothChanged {
			forceConflict[p] = true
		}
	}
	// 冲突 key 含 WsID：这里必须**恒用子工作区 id**，不能因为调用方传了
	// preview_id 就换 key 域。否则「preview → resolve → 带 preview_id 重试」
	// 会算出新 key，解决记录永远匹配不上（沙盒 #66.2 的死循环）。
	replayed, err := op.Replay(parentTreeObj, ops, r.Store, op.Options{
		WsID: wsID, MaxMatches: op.DefaultMaxMatches,
		Resolver: r.DB.Resolver(), Candidates: true,
		ForceConflictPaths: forceConflict,
	})
	if err != nil {
		return nil, err
	}

	res := &LandResult{
		OpsCount: len(ops), Files: summarize(ops), Conflicts: replayed.Conflicts,
		CanLand: len(replayed.Conflicts) == 0, AutoMerged: replayed.AutoMerged,
		TargetHead: parentTree, WSTree: childTree, PreviewID: wsID,
		BaseTree: child.BaseTree, ChildTree: childTree,
		ParentTreeBefore: parentTree, ParentTreeAfter: parentTree,
		ChildOnlyChangedPaths:  subtractPaths(childChanged, parentChanged),
		ParentOnlyChangedPaths: subtractPaths(parentChanged, childChanged),
		BothChangedPaths:       bothChanged,
		AutoMergedPaths:        replayed.AutoMergedPaths,
		OutOfScopePaths:        outOfScope,
	}
	conflictSet := conflictPathSet(replayed.Conflicts)
	res.ConflictPaths = sortedKeys(conflictSet)
	res.WSOpsEpoch = child.OpsEpoch
	// 结果树中「与父树不同」的路径就是本次真正要写的集合。
	resultFlat, err := flattenStoreTree(r.Store, replayed.Tree.ID())
	if err != nil {
		return nil, err
	}
	applyPaths := diffFlatPaths(parentFlat, resultFlat)
	// 不变量：只允许写子会话真正改过的路径。违反说明合并实现越界，
	// 宁可拒绝也不能把「子会话没碰的文件」写回父工作区。
	if p, ok := subsetOf(applyPaths, childChanged); !ok {
		return nil, fmt.Errorf("%w: %s", ErrLandInvariant, p)
	}
	res.AppliedPaths = applyPaths
	res.Paths = buildPathStates(baseFlat, parentFlat, childFlat, resultFlat, conflictSet, res.AutoMergedPaths)
	if len(outOfScope) > 0 {
		// preview 不报错，但必须让调用方看见「这次不能直接落」。
		res.CanLand = false
	}

	if len(replayed.Conflicts) > 0 {
		return res, nil
	}
	if opt.Preview {
		return res, nil
	}
	// 写前逐路径校验：父工作区在这些路径上必须仍等于 parentTreeBefore。
	// 这是 expected_parent_tree 之外的第二道锁，能挡住「读树之后 agent 直接写盘」。
	mismatched, err := r.WS.VerifyBeforeWrite(parentWsID, parentTree, applyPaths)
	if err != nil {
		return nil, err
	}
	if len(mismatched) > 0 {
		return nil, &ParentMovedError{WSID: parentWsID, Paths: mismatched,
			Expected: parentTree.String()}
	}
	// 提交：先写 journal，再只写差异路径，最后重新采集父 ops。
	if err := r.DB.InsertLandIntoJournal(wsID, parentWsID, parentTree.String(),
		replayed.Tree.ID().String()); err != nil {
		return nil, err
	}
	if err := r.WS.ApplyTreeDeltaLocked(parentWsID, parentTree, replayed.Tree); err != nil {
		return nil, err
	}
	after, err := r.WS.EffectiveTreeLocked(parentWsID)
	if err != nil {
		return nil, err
	}
	res.ParentTreeAfter = after
	res.TargetHead = after
	res.State = "done"
	if after != replayed.Tree.ID() {
		afterFlat, ferr := flattenStoreTree(r.Store, after)
		if ferr == nil {
			res.PostApplyMismatch = diffFlatPaths(resultFlat, afterFlat)
		}
		msg := fmt.Sprintf("落盘后父有效树与重放结果不同（%s）", after.String())
		if len(res.PostApplyMismatch) > 0 {
			msg += fmt.Sprintf("；差异路径: %s", strings.Join(res.PostApplyMismatch, ", "))
		}
		res.Warnings = append(res.Warnings, msg)
		log.Printf("p37: %s", msg)
	}
	if err := r.DB.InsertLandedWSWithTarget(wsID, after.String(),
		"workspace", parentWsID, time.Now().UnixMilli()); err != nil {
		return nil, err
	}
	_ = r.DB.ClearConflicts(wsID)
	return res, nil
}

// syncForLandLocked 是全量采集 + 不稳定文件重试。
func (r *Repo) syncForLandLocked(wsID string) error {
	const attempts = 3
	for i := 0; i < attempts; i++ {
		if err := r.WS.SyncStrictLocked(wsID); err != nil {
			return err
		}
		unstable := r.WS.LastUnstable(wsID)
		if len(unstable) == 0 {
			return nil
		}
		if i == attempts-1 {
			return &UnstableWSError{WSID: wsID, Paths: unstable}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

func (r *Repo) cleanupLandIntoJournal(wsID, parentWsID string) {
	if js, err := r.DB.ListLandIntoJournal(); err == nil {
		for _, j := range js {
			if j.WsID == wsID && j.ParentWsID == parentWsID {
				_ = r.DB.DeleteLandIntoJournal(j.ID)
			}
		}
	}
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
		before, err := hash.Parse(j.BeforeTree)
		if err != nil {
			continue
		}
		// 只重放 before→result 的差异路径：崩溃恢复同样不能整树覆盖父目录。
		if err := r.WS.ApplyTreeDelta(j.ParentWsID, before, target); err != nil {
			continue
		}
		_ = r.DB.DeleteLandIntoJournal(j.ID)
	}
	return nil
}
