package repo

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/chenyiwen/p37/internal/capture"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/op"
	"github.com/chenyiwen/p37/internal/version"
	"github.com/chenyiwen/p37/internal/workspace"
)

// Line 相关错误。
var (
	ErrLineNotFound       = errors.New("repo: line 不存在")
	ErrLineExists         = errors.New("repo: line 已存在")
	ErrLineProtected      = errors.New("repo: line 受保护")
	ErrTargetNotAncestor  = errors.New("repo: 目标不是当前 head 的祖先")
	ErrTargetMoved        = errors.New("repo: 目标 head 已变化")
	ErrSourceMoved        = errors.New("repo: source head 已变化")
	ErrMergeBaseAmbiguous = errors.New("repo: merge base 不唯一")
	ErrMergeExpired       = errors.New("repo: merge preview 已过期")
)

// CreateLine 从 version/line/mainline 创建命名 line。
func (r *Repo) CreateLine(name string, from string, protected bool) (*meta.Line, error) {
	if !validLineName(name) {
		return nil, fmt.Errorf("repo: 非法 line 名 %q", name)
	}
	head, err := r.resolveRef(from, true)
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	if err := r.DB.CreateLine(name, head, head, protected, now); err != nil {
		if errors.Is(err, meta.ErrLineExists) {
			// 同名 line 视为幂等：返回已有 line，不改变它的 head。
			return r.DB.GetLine(name)
		}
		return nil, err
	}
	return r.DB.GetLine(name)
}

// Lines 返回全部 line。
func (r *Repo) Lines() ([]meta.Line, error) { return r.DB.ListLines() }

// Line 读取一条 line。
func (r *Repo) Line(name string) (*meta.Line, error) {
	l, err := r.DB.GetLine(name)
	if err != nil {
		if errors.Is(err, meta.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrLineNotFound, name)
		}
		return nil, err
	}
	if l.DeletedTS != 0 {
		return nil, fmt.Errorf("%w: %s", ErrLineNotFound, name)
	}
	return l, nil
}

// DeleteLine 删除一条 line。
func (r *Repo) DeleteLine(name string, force bool) error {
	r.landMu.Lock()
	defer r.landMu.Unlock()
	l, err := r.Line(name)
	if err != nil {
		return err
	}
	def, _ := r.DB.DefaultLine()
	if name == def || l.Protected {
		if !force {
			return fmt.Errorf("%w: %s", ErrLineProtected, name)
		}
	}
	// 不允许删除仍有活跃工作区的 line（工作区以 base_line 记录）。
	if n, err := r.lineWorkspaceCount(name); err != nil {
		return err
	} else if n > 0 {
		return fmt.Errorf("%w: line %s 仍有 %d 个活跃工作区", workspace.ErrWrongState, name, n)
	}
	return r.DB.DeleteLine(name, time.Now().UnixMilli())
}

func (r *Repo) lineWorkspaceCount(name string) (int, error) {
	all, err := r.DB.ListWorkspaces()
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range all {
		if all[i].Hidden || all[i].BaseLine != name || all[i].LandedTS != 0 {
			continue
		}
		n++
	}
	return n, nil
}

// ProtectLine 设置保护位。
func (r *Repo) ProtectLine(name string, protected, force bool) error {
	l, err := r.Line(name)
	if err != nil {
		return err
	}
	def, _ := r.DB.DefaultLine()
	if name == def && !protected && !force {
		return fmt.Errorf("%w: 默认 line 取消保护需要 force", ErrLineProtected)
	}
	if l.Protected && !protected && !force {
		return fmt.Errorf("%w: %s", ErrLineProtected, name)
	}
	return r.DB.SetLineProtected(name, protected, time.Now().UnixMilli())
}

// ResetLine 把 line head 移到历史版本。
func (r *Repo) ResetLine(name string, target string, force bool, reason string, expectedHead string) (*meta.Line, error) {
	r.landMu.Lock()
	defer r.landMu.Unlock()
	l, err := r.Line(name)
	if err != nil {
		return nil, err
	}
	tid, err := r.resolveVersionRef(target)
	if err != nil {
		return nil, err
	}
	if l.Protected && !force {
		return nil, fmt.Errorf("%w: %s", ErrLineProtected, name)
	}
	if !force {
		ok, err := r.DB.IsAncestor(tid, l.Head)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: %s 不是 %s 的祖先", ErrTargetNotAncestor, tid, l.Head)
		}
	}
	if expectedHead != "" {
		exp, err := r.resolveVersionRef(expectedHead)
		if err != nil {
			return nil, err
		}
		if exp != l.Head {
			return nil, fmt.Errorf("%w: current=%s", ErrTargetMoved, l.Head)
		}
	}
	if force && reason == "" {
		return nil, errors.New("repo: force reset 必须提供 reason")
	}
	now := time.Now().UnixMilli()
	if err := r.DB.SetLineHeadIf(name, tid, l.Head, now); err != nil {
		if errors.Is(err, meta.ErrLineHeadMoved) {
			return nil, ErrTargetMoved
		}
		return nil, err
	}
	if err := r.syncMainlineRefIfDefault(name, tid); err != nil {
		return nil, err
	}
	_ = r.DB.RecordLineReset(name, l.Head, tid, reason, force, now)
	return r.DB.GetLine(name)
}

// LineLog 返回一条 line 的祖先历史。
func (r *Repo) LineLog(name string, limit int, beforeTS int64, beforeID string) ([]VersionSummary, error) {
	if name == "mainline" {
		name, _ = r.DB.DefaultLine()
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	l, err := r.Line(name)
	if err != nil {
		return nil, err
	}
	var bid hash.ID
	if beforeID != "" {
		bid, err = r.resolveVersionRef(beforeID)
		if err != nil {
			return nil, err
		}
	}
	rows, err := r.DB.LogFrom(l.Head, limit, beforeTS, bid)
	if err != nil {
		return nil, err
	}
	out := make([]VersionSummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, VersionSummary{
			ID: row.ID, Parent: row.Parent, TS: row.TS,
			Agent: row.Agent, Task: row.Task, Epoch: row.Epoch,
		})
	}
	return out, nil
}

// LineCompare 返回两条 line 的 ahead/behind 与 merge base。
type LineCompare struct {
	A          string     `json:"a"`
	B          string     `json:"b"`
	AHead      hash.ID    `json:"a_head"`
	BHead      hash.ID    `json:"b_head"`
	Ahead      int        `json:"ahead"`
	Behind     int        `json:"behind"`
	MergeBase  hash.ID    `json:"merge_base"`
	MergeBases []hash.ID  `json:"merge_bases,omitempty"`
	Diverged   bool       `json:"diverged"`
	AChanged   []FileStat `json:"a_files,omitempty"`
	BChanged   []FileStat `json:"b_files,omitempty"`
}

// CompareLines 对比两条 line。
func (r *Repo) CompareLines(a, b string) (*LineCompare, error) {
	def, _ := r.DB.DefaultLine()
	if a == "mainline" {
		a = def
	}
	if b == "mainline" {
		b = def
	}
	la, err := r.Line(a)
	if err != nil {
		return nil, err
	}
	lb, err := r.Line(b)
	if err != nil {
		return nil, err
	}
	as, err := r.DB.Ancestors(la.Head)
	if err != nil {
		return nil, err
	}
	bs, err := r.DB.Ancestors(lb.Head)
	if err != nil {
		return nil, err
	}
	out := &LineCompare{A: a, B: b, AHead: la.Head, BHead: lb.Head}
	for id := range as {
		if !bs[id] {
			out.Ahead++
		}
	}
	for id := range bs {
		if !as[id] {
			out.Behind++
		}
	}
	base, all, err := r.DB.MergeBases(la.Head, lb.Head)
	if err != nil {
		return nil, err
	}
	out.MergeBase = base
	out.MergeBases = all
	out.Diverged = out.Ahead > 0 && out.Behind > 0
	if !base.IsZero() {
		if bv, err := r.DB.GetVersion(base); err == nil {
			if bt, err := r.Store.GetTree(bv.Tree); err == nil {
				if av, err := r.DB.GetVersion(la.Head); err == nil {
					if at, err := r.Store.GetTree(av.Tree); err == nil {
						if ops, err := capture.Diff("__cmp_a__", bt, at, r.Store, r.CaptureOpt); err == nil {
							out.AChanged = summarize(ops)
						}
					}
				}
				if bv2, err := r.DB.GetVersion(lb.Head); err == nil {
					if bt2, err := r.Store.GetTree(bv2.Tree); err == nil {
						if ops, err := capture.Diff("__cmp_b__", bt, bt2, r.Store, r.CaptureOpt); err == nil {
							out.BChanged = summarize(ops)
						}
					}
				}
			}
		}
	}
	return out, nil
}

// LineGraphNode 是版本图的节点。
type LineGraphNode struct {
	ID      hash.ID   `json:"id"`
	Parents []hash.ID `json:"parents"`
	TS      int64     `json:"ts"`
	Agent   string    `json:"agent"`
	Task    string    `json:"task"`
	Epoch   uint64    `json:"epoch"`
	Lines   []string  `json:"lines"`
}

// LineGraph 返回版本图。
func (r *Repo) LineGraph(lineNames []string, limit int) (map[string]any, error) {
	if limit <= 0 {
		limit = 200
	}
	lines, err := r.DB.ListLines()
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, n := range lineNames {
		want[n] = true
	}
	ids := map[hash.ID]bool{}
	lineOf := map[hash.ID][]string{}
	for i := range lines {
		l := &lines[i]
		if len(want) > 0 && !want[l.Name] {
			continue
		}
		anc, err := r.DB.Ancestors(l.Head)
		if err != nil {
			return nil, err
		}
		for id := range anc {
			ids[id] = true
			lineOf[id] = append(lineOf[id], l.Name)
		}
	}
	nodes := make([]LineGraphNode, 0, len(ids))
	for id := range ids {
		v, err := r.DB.GetVersion(id)
		if err != nil {
			continue
		}
		ps, _ := r.DB.ParentIDs(id)
		nodes = append(nodes, LineGraphNode{
			ID: id, Parents: ps, TS: v.TS, Agent: v.Agent, Task: v.TaskDesc,
			Epoch: v.Epoch, Lines: lineOf[id],
		})
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].TS != nodes[j].TS {
			return nodes[i].TS > nodes[j].TS
		}
		return nodes[i].ID.String() > nodes[j].ID.String()
	})
	if len(nodes) > limit {
		nodes = nodes[:limit]
	}
	edges := make([]map[string]any, 0)
	for _, n := range nodes {
		for _, p := range n.Parents {
			edges = append(edges, map[string]any{"from": p.String(), "to": n.ID.String(), "kind": "parent"})
		}
	}
	return map[string]any{"nodes": nodes, "edges": edges, "truncated": len(ids) > limit}, nil
}

// LineInfo 汇总 line 详情。
func (r *Repo) LineInfo(name string) (map[string]any, error) {
	l, err := r.Line(name)
	if err != nil {
		return nil, err
	}
	def, _ := r.DB.DefaultLine()
	cmp := &LineCompare{}
	if def != "" && def != name {
		cmp, _ = r.CompareLines(name, def)
	}
	n, _ := r.lineWorkspaceCount(name)
	return map[string]any{
		"name": l.Name, "head": l.Head.String(), "protected": l.Protected,
		"created_from": l.CreatedFrom.String(), "created_ts": l.CreatedTS,
		"updated_ts": l.UpdatedTS, "workspace_count": n,
		"ahead": cmp.Ahead, "behind": cmp.Behind,
		"merge_base": cmp.MergeBase.String(),
	}, nil
}

// MergeResult 是 line_merge / preview 的结果。
type MergeResult struct {
	MergeID       string        `json:"merge_id,omitempty"`
	Source        string        `json:"source"`
	Target        string        `json:"target"`
	SourceHead    hash.ID       `json:"source_head"`
	TargetHead    hash.ID       `json:"target_head"`
	BaseVersion   hash.ID       `json:"base_version"`
	VersionID     hash.ID       `json:"version_id"`
	FastForward   bool          `json:"fast_forward"`
	AlreadyMerged bool          `json:"already_merged"`
	NoOp          bool          `json:"no_op"`
	CanMerge      bool          `json:"can_merge"`
	Conflicts     []op.Conflict `json:"conflicts,omitempty"`
	Files         []FileStat    `json:"files,omitempty"`
	AutoMerged    int           `json:"auto_merged"`
	Warnings      []string      `json:"warnings,omitempty"`
}

// MergeOptions 控制 line_merge。
type MergeOptions struct {
	Preview            bool
	NoFF               bool
	MergeID            string
	ExpectedHead       string
	ExpectedSourceHead string
	IdempotencyKey     string
	TaskDesc           string
}

// MergeLines 把 source 的变更并入 target。preview=true 时不推进 target。
func (r *Repo) MergeLines(source, target string, opt MergeOptions) (*MergeResult, error) {
	source = strings.TrimPrefix(source, "line:")
	target = strings.TrimPrefix(target, "line:")
	if source == "mainline" || target == "mainline" {
		def, _ := r.DB.DefaultLine()
		if source == "mainline" {
			source = def
		}
		if target == "mainline" {
			target = def
		}
	}
	release, err := r.enterLand()
	if err != nil {
		return nil, err
	}
	defer release()
	r.landMu.Lock()
	defer r.landMu.Unlock()

	if opt.IdempotencyKey != "" && !opt.Preview {
		if v, _ := r.DB.FindVersionByIdempotency("line_merge", opt.IdempotencyKey, time.Now().UnixMilli()); v != nil {
			head, _ := r.DB.LineHead(target)
			return &MergeResult{Source: source, Target: target, VersionID: v.ID(),
				TargetHead: head, SourceHead: v.Parent}, nil
		}
		if ir, _ := r.DB.GetIdempotencyResult("line_merge", opt.IdempotencyKey, time.Now().UnixMilli()); ir != nil {
			var out MergeResult
			if json.Unmarshal([]byte(ir.ResultJSON), &out) == nil {
				return &out, nil
			}
		}
	}

	var (
		sourceHead, targetHead hash.ID
		baseID                 hash.ID
	)
	if opt.MergeID != "" {
		mw, err := r.DB.GetWorkspace(opt.MergeID)
		if err != nil {
			return nil, fmt.Errorf("%w: merge_id %s", workspace.ErrNotFound, opt.MergeID)
		}
		if mw.Kind != "merge" || mw.LandedTS != 0 {
			return nil, fmt.Errorf("%w: merge_id 已失效", workspace.ErrWrongState)
		}
		if time.Now().UnixMilli()-mw.CreatedTS > int64(24*time.Hour/time.Millisecond) {
			return nil, ErrMergeExpired
		}
		target = mw.BaseLine
		source = mw.Name
		sourceHead = mw.BaseVersion // 约定：merge workspace 的 base_version 存 source head
		th, err := hash.Parse(mw.ParentID)
		if err != nil {
			return nil, err
		}
		targetHead = th
		if opt.MergeID != "" {
			if tl, err := r.Line(target); err == nil && tl.Head != targetHead {
				return nil, fmt.Errorf("%w: current=%s", ErrTargetMoved, tl.Head)
			}
			if sl, err := r.Line(source); err == nil && sl.Head != sourceHead {
				return nil, fmt.Errorf("%w: current=%s", ErrSourceMoved, sl.Head)
			}
		}
	}
	if target == "" || source == "" {
		return nil, errors.New("repo: 缺少 source/target")
	}
	sl, err := r.Line(source)
	if err != nil {
		return nil, err
	}
	tl, err := r.Line(target)
	if err != nil {
		return nil, err
	}
	if sourceHead.IsZero() {
		sourceHead = sl.Head
	}
	if targetHead.IsZero() {
		targetHead = tl.Head
	}
	if opt.MergeID == "" && source == target {
		return nil, errors.New("repo: source 与 target 不能相同")
	}
	if opt.ExpectedSourceHead != "" {
		exp, err := r.resolveVersionRef(opt.ExpectedSourceHead)
		if err != nil {
			return nil, err
		}
		if exp != sl.Head {
			return nil, fmt.Errorf("%w: current=%s", ErrSourceMoved, sl.Head)
		}
	}
	if opt.ExpectedHead != "" {
		exp, err := r.resolveVersionRef(opt.ExpectedHead)
		if err != nil {
			return nil, err
		}
		if exp != tl.Head {
			return nil, fmt.Errorf("%w: current=%s", ErrTargetMoved, tl.Head)
		}
	}
	if opt.MergeID == "" {
		if opt.ExpectedHead != "" && tl.Head.String() != opt.ExpectedHead {
			if exp, err := r.resolveVersionRef(opt.ExpectedHead); err != nil || exp != tl.Head {
				return nil, fmt.Errorf("%w: current=%s", ErrTargetMoved, tl.Head)
			}
		}
	}

	if sourceHead == targetHead {
		return &MergeResult{Source: source, Target: target, SourceHead: sourceHead,
			TargetHead: targetHead, AlreadyMerged: true, NoOp: true, CanMerge: true}, nil
	}
	// target 是 source 祖先 -> fast-forward（no_ff 时强制 merge 版本）。
	if ok, _ := r.DB.IsAncestor(targetHead, sourceHead); ok && !opt.NoFF {
		if opt.Preview {
			return &MergeResult{Source: source, Target: target, SourceHead: sourceHead,
				TargetHead: targetHead, BaseVersion: targetHead, FastForward: true, CanMerge: true}, nil
		}
		if err := r.DB.SetLineHeadIf(target, sourceHead, targetHead, time.Now().UnixMilli()); err != nil {
			if errors.Is(err, meta.ErrLineHeadMoved) {
				return nil, ErrTargetMoved
			}
			return nil, err
		}
		if err := r.syncMainlineRefIfDefault(target, sourceHead); err != nil {
			return nil, err
		}
		res := &MergeResult{Source: source, Target: target, SourceHead: sourceHead,
			TargetHead: sourceHead, BaseVersion: targetHead, FastForward: true, CanMerge: true}
		r.putIdem("line_merge", opt.IdempotencyKey, res)
		return res, nil
	}
	if ok, _ := r.DB.IsAncestor(sourceHead, targetHead); ok {
		return &MergeResult{Source: source, Target: target, SourceHead: sourceHead,
			TargetHead: targetHead, AlreadyMerged: true, NoOp: true, CanMerge: true}, nil
	}

	baseID, err = r.DB.MergeBase(sourceHead, targetHead)
	if err != nil {
		if errors.Is(err, meta.ErrMergeBaseAmbiguous) {
			return nil, ErrMergeBaseAmbiguous
		}
		return nil, err
	}
	baseV, err := r.DB.GetVersion(baseID)
	if err != nil {
		return nil, err
	}
	srcV, err := r.DB.GetVersion(sourceHead)
	if err != nil {
		return nil, err
	}
	tgtV, err := r.DB.GetVersion(targetHead)
	if err != nil {
		return nil, err
	}
	baseTree, err := r.Store.GetTree(baseV.Tree)
	if err != nil {
		return nil, err
	}
	srcTree, err := r.Store.GetTree(srcV.Tree)
	if err != nil {
		return nil, err
	}
	tgtTree, err := r.Store.GetTree(tgtV.Tree)
	if err != nil {
		return nil, err
	}
	ops, err := capture.Diff("__merge__", baseTree, srcTree, r.Store, r.CaptureOpt)
	if err != nil {
		return nil, err
	}
	mergeID := opt.MergeID
	if mergeID == "" && opt.Preview {
		mergeID, err = newMergeID()
		if err != nil {
			return nil, err
		}
	}
	replayID := mergeID
	if replayID == "" {
		replayID = "__merge_commit__"
	}
	replayed, err := op.Replay(tgtTree, ops, r.Store, op.Options{
		WsID: replayID, MaxMatches: op.DefaultMaxMatches, Resolver: r.DB.Resolver(), Candidates: true,
	})
	if err != nil {
		return nil, err
	}
	res := &MergeResult{
		MergeID: mergeID, Source: source, Target: target, SourceHead: sourceHead,
		TargetHead: targetHead, BaseVersion: baseID,
		Conflicts: replayed.Conflicts, Files: summarize(ops),
		AutoMerged: replayed.AutoMerged, CanMerge: len(replayed.Conflicts) == 0,
	}
	if opt.Preview {
		if mergeID != "" && opt.MergeID == "" {
			// 创建隐藏 merge workspace，承载冲突与后续 resolution。
			w := meta.Workspace{
				ID: mergeID, Name: source, ParentKind: meta.ParentVersion,
				ParentID: targetHead.String(), BaseTree: tgtTree.ID(),
				BaseVersion: sourceHead, BaseLine: target, TaskDesc: "line_merge",
				State: meta.StateConflicted, Materialize: meta.MaterializeCopy,
				Hidden: true, Kind: "merge", CreatedTS: time.Now().UnixMilli(),
				UpdatedTS: time.Now().UnixMilli(),
			}
			// BaseVersion 用来存 source head（约定见上）；同时保留 target head 在 parent_id。
			if err := r.DB.InsertWorkspace(w); err != nil {
				return nil, err
			}
			if err := r.persistConflicts(mergeID, replayed.Conflicts); err != nil {
				return nil, err
			}
		}
		return res, nil
	}
	if len(replayed.Conflicts) > 0 {
		return res, ErrLandConflict
	}
	if !res.CanMerge {
		return res, ErrLandConflict
	}
	rp, err := r.DB.GetRepo()
	if err != nil {
		return nil, err
	}
	epoch := uint64(rp.VersionCount / int64(r.epochSize))
	task := opt.TaskDesc
	if task == "" {
		task = "merge " + source + " -> " + target
	}
	v := version.NewMerge([]hash.ID{targetHead, sourceHead}, replayed.Tree.ID(),
		epoch, "p37-merge", task, time.Now().UnixMilli(), task)
	j := meta.JournalEntry{
		VersionID: v.ID().String(), WsID: mergeID, Agent: "p37-merge",
		TaskDesc: r.journalTask(task), TS: v.TS, OpsJSON: journalOps(ops),
	}
	if err := r.DB.CommitMerge(v, j, target, targetHead, "line_merge", opt.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := r.syncMainlineRefIfDefault(target, v.ID()); err != nil {
		return nil, err
	}
	if mergeID != "" {
		_ = r.DB.ClearConflicts(mergeID)
		if _, _, err := r.WS.Drop(mergeID, true, true); err != nil &&
			!errors.Is(err, workspace.ErrNotFound) {
			msg := fmt.Sprintf("merge 已提交，但清理 merge 工作区 %s 失败: %v", mergeID, err)
			log.Printf("p37: %s", msg)
			res.Warnings = append(res.Warnings, msg)
		}
	}
	res.VersionID = v.ID()
	res.TargetHead = v.ID()
	r.putIdem("line_merge", opt.IdempotencyKey, res)
	return res, nil
}

func (r *Repo) putIdem(tool, key string, v any) {
	if key == "" {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	now := time.Now().UnixMilli()
	_ = r.DB.PutIdempotencyResult(tool, key, string(b), now,
		now+int64(7*24*time.Hour/time.Millisecond))
}

func newMergeID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "mg_" + hex.EncodeToString(b[:]), nil
}

// resolveRef 解析 version:/line:/mainline 或裸版本 id。
func (r *Repo) resolveRef(ref string, allowEmpty bool) (hash.ID, error) {
	if ref == "" {
		if !allowEmpty {
			return hash.ID{}, errors.New("repo: 缺少 ref")
		}
		def, err := r.DB.DefaultLine()
		if err != nil {
			return hash.ID{}, err
		}
		return r.DB.LineHead(def)
	}
	switch {
	case len(ref) > 5 && ref[:5] == "line:":
		h, err := r.DB.LineHead(ref[5:])
		if err != nil {
			if errors.Is(err, meta.ErrNotExist) {
				return hash.ID{}, fmt.Errorf("%w: %s", ErrLineNotFound, ref[5:])
			}
			return hash.ID{}, err
		}
		return h, nil
	case ref == "mainline":
		def, err := r.DB.DefaultLine()
		if err != nil {
			return hash.ID{}, err
		}
		return r.DB.LineHead(def)
	case len(ref) > 8 && ref[:8] == "version:":
		return r.resolveVersionRef(ref[8:])
	case len(ref) > 10 && ref[:10] == "workspace:":
		return hash.ID{}, fmt.Errorf("repo: ref %q 不是版本引用", ref)
	}
	// 裸版本 id / 唯一前缀
	if id, err := r.resolveVersionRef(ref); err == nil {
		return id, nil
	}
	// 也接受裸 line 名。
	if h, err := r.DB.LineHead(ref); err == nil {
		return h, nil
	}
	return hash.ID{}, fmt.Errorf("%w: %s", ErrVersionNotFound, ref)
}

func (r *Repo) resolveVersionRef(ref string) (hash.ID, error) {
	ref = strings.TrimPrefix(ref, "version:")
	if ref == "" {
		return hash.ID{}, ErrVersionNotFound
	}
	if id, err := hash.Parse(ref); err == nil {
		if _, err := r.DB.GetVersion(id); err == nil {
			return id, nil
		}
	}
	if len(ref) >= 4 && len(ref) < hash.HexLen && isHex(ref) {
		id, err := r.DB.FindVersionByPrefix(ref)
		if err != nil {
			return hash.ID{}, fmt.Errorf("%w: %s", ErrVersionNotFound, ref)
		}
		return id, nil
	}
	return hash.ID{}, fmt.Errorf("%w: %s", ErrVersionNotFound, ref)
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// RevertTo 在目标 line 上产生一个反向版本（version_revert）。
func (r *Repo) RevertTo(versionID string, targetLine string, expectedHead string, idemKey string) (hash.ID, error) {
	release, err := r.enterLand()
	if err != nil {
		return hash.ID{}, err
	}
	defer release()
	r.landMu.Lock()
	defer r.landMu.Unlock()

	id, err := r.resolveVersionRef(versionID)
	if err != nil {
		return hash.ID{}, err
	}
	if targetLine == "" {
		targetLine, err = r.DB.DefaultLine()
		if err != nil {
			return hash.ID{}, err
		}
	}
	line, err := r.Line(targetLine)
	if err != nil {
		return hash.ID{}, err
	}
	if expectedHead != "" {
		exp, err := r.resolveVersionRef(expectedHead)
		if err != nil || exp != line.Head {
			return hash.ID{}, fmt.Errorf("%w: current=%s", ErrTargetMoved, line.Head)
		}
	}
	target, err := r.DB.GetVersion(id)
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
	fwd, err := r.captureDiff(parent.Tree, target.Tree)
	if err != nil {
		return hash.ID{}, err
	}
	inv := invert(fwd)
	headV, err := r.DB.GetVersion(line.Head)
	if err != nil {
		return hash.ID{}, err
	}
	headTree, err := r.Store.GetTree(headV.Tree)
	if err != nil {
		return hash.ID{}, err
	}
	replayed, err := op.Replay(headTree, inv, r.Store, op.Options{
		WsID: "revert", MaxMatches: op.DefaultMaxMatches, Resolver: r.DB.Resolver(),
	})
	if err != nil {
		return hash.ID{}, err
	}
	if len(replayed.Conflicts) > 0 {
		return hash.ID{}, fmt.Errorf("%w: revert 产生 %d 个冲突", ErrLandConflict, len(replayed.Conflicts))
	}
	rp, err := r.DB.GetRepo()
	if err != nil {
		return hash.ID{}, err
	}
	epoch := uint64(rp.VersionCount / int64(r.epochSize))
	msg := "revert " + id.String()[:16]
	v := version.New(line.Head, replayed.Tree.ID(), epoch, "p37-revert", msg,
		time.Now().UnixMilli(), msg)
	j := meta.JournalEntry{VersionID: v.ID().String(), Agent: "p37-revert",
		TaskDesc: r.journalTask(msg), TS: v.TS, OpsJSON: journalOps(inv)}
	if err := r.DB.CommitLineLand(v, j, targetLine, nil, "version_revert", idemKey, line.Head); err != nil {
		return hash.ID{}, err
	}
	if err := r.syncMainlineRefIfDefault(targetLine, v.ID()); err != nil {
		return hash.ID{}, err
	}
	return v.ID(), nil
}

func validLineName(name string) bool {
	if name == "" || len(name) > 128 || name[0] == '/' || name[len(name)-1] == '/' {
		return false
	}
	if strings.Contains(name, "//") || strings.Contains(name, "..") {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == '-' || c == '/' {
			continue
		}
		return false
	}
	return true
}
