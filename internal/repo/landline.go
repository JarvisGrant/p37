package repo

import (
	"errors"
	"fmt"
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
		for i := range kids {
			k := &kids[i]
			if k.Hidden || k.LandedTS != 0 {
				continue
			}
			switch k.State {
			case meta.StateActive, meta.StateConflicted, meta.StateRebasing:
				return nil, fmt.Errorf("%w: %s", ErrHasActiveChildren, k.ID)
			}
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
			_ = r.MM.Close(fw.ID, false, overlay)
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
	child, err := r.WS.Get(wsID)
	if err != nil {
		return nil, err
	}
	if child.ParentKind != meta.ParentWorkspace || child.ParentID != parentWsID {
		return nil, fmt.Errorf("%w: child 的直接父不是 %s", ErrTargetMoved, parentWsID)
	}
	parent, err := r.WS.Get(parentWsID)
	if err != nil {
		return nil, err
	}
	if parent.Hidden || parent.Kind == "merge" || parent.LandedTS != 0 {
		return nil, fmt.Errorf("%w: 父工作区不可写", workspace.ErrWrongState)
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
	replayID := previewID
	if replayID == "" {
		replayID = wsID
	}
	replayed, err := op.Replay(parentTreeObj, ops, r.Store, op.Options{
		WsID: replayID, MaxMatches: op.DefaultMaxMatches,
		Resolver: r.DB.Resolver(), Candidates: true,
	})
	if err != nil {
		return nil, err
	}
	res := &LandResult{
		OpsCount: len(ops), Files: summarize(ops), Conflicts: replayed.Conflicts,
		CanLand: len(replayed.Conflicts) == 0, AutoMerged: replayed.AutoMerged,
		TargetHead: parentTree, WSTree: childTree, PreviewID: previewID,
	}
	if len(replayed.Conflicts) > 0 {
		pid := previewID
		if pid == "" {
			pid, err = newMergeID()
			if err != nil {
				return nil, err
			}
			res.PreviewID = pid
			// 隐藏 preview workspace 承载冲突；parent_tree 存在 BaseTree。
			w := meta.Workspace{
				ID: pid, Name: parentWsID, ParentKind: meta.ParentWorkspace,
				ParentID: parentWsID, BaseTree: parentTree, BaseVersion: child.BaseVersion,
				BaseLine: "", TaskDesc: "land_into_preview", State: meta.StateConflicted,
				Materialize: meta.MaterializeCopy, Hidden: true, Kind: "preview",
				CreatedTS: time.Now().UnixMilli(), UpdatedTS: time.Now().UnixMilli(),
			}
			if err := r.DB.InsertWorkspace(w); err != nil {
				return nil, err
			}
		}
		if err := r.persistConflicts(pid, replayed.Conflicts); err != nil {
			return nil, err
		}
		return res, nil
	}
	if preview {
		return res, nil
	}
	// 提交：把结果树写回父工作区目录并重新采集父 ops。
	if err := r.WS.ApplyTree(parentWsID, replayed.Tree); err != nil {
		return nil, err
	}
	res.TargetHead = replayed.Tree.ID()
	res.State = "done"
	if err := r.DB.InsertLandedWS(wsID, replayed.Tree.ID().String(), time.Now().UnixMilli()); err != nil {
		return nil, err
	}
	_, _, _ = r.WS.Drop(wsID, true, false)
	if previewID != "" {
		_ = r.DB.ClearConflicts(previewID)
		_, _, _ = r.WS.Drop(previewID, true, true)
	}
	if idemKey != "" {
		r.putIdem("ws_land_into", idemKey, res)
	}
	return res, nil
}
