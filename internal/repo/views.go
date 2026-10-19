package repo

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/chenyiwen/p37/internal/capture"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/op"
	"github.com/chenyiwen/p37/internal/tree"
)

// VersionDetail 是 version_show 的返回（附录 K）。
type VersionDetail struct {
	ID     hash.ID    `json:"id"`
	Parent hash.ID    `json:"parent"`
	TS     int64      `json:"ts"`
	Agent  string     `json:"agent"`
	Task   string     `json:"task"`
	Msg    string     `json:"msg"`
	Epoch  uint64     `json:"epoch"`
	First  bool       `json:"first"`
	Files  []FileStat `json:"files"`
}

// VersionShow 返回版本详情（含相对父版本的文件级统计）。
func (r *Repo) VersionShow(id hash.ID) (*VersionDetail, error) {
	v, err := r.DB.GetVersion(id)
	if err != nil {
		if errors.Is(err, meta.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrVersionNotFound, id)
		}
		return nil, err
	}
	d := &VersionDetail{
		ID: v.ID(), Parent: v.Parent, TS: v.TS, Agent: v.Agent,
		Task: v.TaskDesc, Msg: v.Msg, Epoch: v.Epoch, First: v.IsFirst(),
	}
	if !v.IsFirst() {
		ops, err := r.diffVersions(v.Parent, v.ID())
		if err != nil {
			return nil, err
		}
		d.Files = summarize(ops)
	}
	return d, nil
}

// ErrVersionNotFound 表示版本不存在（含已被回收）。
var ErrVersionNotFound = errors.New("repo: 版本不存在")

// diffVersions 由两个版本的树推导 op（用于统计与展示）。
func (r *Repo) diffVersions(a, b hash.ID) ([]op.Op, error) {
	at, err := r.versionTree(a)
	if err != nil {
		return nil, err
	}
	bt, err := r.versionTree(b)
	if err != nil {
		return nil, err
	}
	return capture.Diff("__diff__", at, bt, r.Store, r.CaptureOpt)
}

// versionTree 返回版本的树；**版本已被剪枝时返回空树**（§10.3）。
//
// 为什么这是对的：剪枝之后最老的那个保留版本就成了新的根，它对「空」的差异
// 就是「整个仓库都是新增的」—— 这正是它对使用者应当呈现的样子。
// 若在这里报 ErrVersionNotFound，剪枝一次之后 version_show / version_diff
// 就全废了。
func (r *Repo) versionTree(id hash.ID) (*tree.Tree, error) {
	v, err := r.DB.GetVersion(id)
	if err != nil {
		if errors.Is(err, meta.ErrNotExist) {
			return &tree.Tree{}, nil
		}
		return nil, err
	}
	return r.Store.GetTree(v.Tree)
}

// FileDiff 是 version_diff 的单文件条目。
type FileDiff struct {
	Path    string     `json:"path"`
	Kind    string     `json:"kind"`
	Added   int        `json:"add"`
	Deleted int        `json:"del"`
	Hunks   []HunkView `json:"hunks,omitempty"`
}

// HunkView 是可读的 hunk。
type HunkView struct {
	Before    []string `json:"before,omitempty"`
	Remove    []string `json:"remove,omitempty"`
	Insert    []string `json:"insert,omitempty"`
	After     []string `json:"after,omitempty"`
	StartHint int      `json:"start_hint"`
}

// VersionDiff 返回两版本间差异（附录 K）。
func (r *Repo) VersionDiff(a, b hash.ID, pathFilter string, statOnly bool) ([]FileDiff, error) {
	ops, err := r.diffVersions(a, b)
	if err != nil {
		return nil, err
	}
	byPath := map[string]*FileDiff{}
	var order []string
	for i := range ops {
		o := &ops[i]
		if pathFilter != "" && o.Path != pathFilter {
			continue
		}
		fd, ok := byPath[o.Path]
		if !ok {
			fd = &FileDiff{Path: o.Path, Kind: string(o.Kind)}
			byPath[o.Path] = fd
			order = append(order, o.Path)
		}
		fd.Kind = string(o.Kind)
		for _, h := range o.Hunks {
			fd.Added += len(h.Insert)
			fd.Deleted += len(h.Remove)
			if !statOnly {
				fd.Hunks = append(fd.Hunks, HunkView{
					Before: toLines(h.Before), Remove: toLines(h.Remove),
					Insert: toLines(h.Insert), After: toLines(h.After),
					StartHint: h.StartHint,
				})
			}
		}
	}
	sort.Strings(order)
	out := make([]FileDiff, 0, len(order))
	for _, k := range order {
		out = append(out, *byPath[k])
	}
	return out, nil
}

func toLines(ls [][]byte) []string {
	if len(ls) == 0 {
		return nil
	}
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = string(l)
	}
	return out
}

// ConflictViewFor 把一个持久化的冲突行转成面向 MCP 的视图。
func (r *Repo) ConflictViewFor(row *meta.ConflictRow) ConflictView {
	var base, lower, upper []string
	_ = json.Unmarshal([]byte(row.BaseJSON), &base)
	_ = json.Unmarshal([]byte(row.LowerJSON), &lower)
	_ = json.Unmarshal([]byte(row.UpperJSON), &upper)
	var cands []CandidateView
	var raw []struct {
		Kind string `json:"kind"`
		Note string `json:"note"`
	}
	_ = json.Unmarshal([]byte(row.CandsJSON), &raw)
	for _, c := range raw {
		cands = append(cands, CandidateView{Kind: c.Kind, Note: c.Note})
	}
	return ConflictView{
		Key: row.Key.String(), Path: row.Path, Kind: string(row.Kind),
		State: row.State, Base: base,
		Lower:      SideView{Lines: lower},
		Upper:      SideView{Lines: upper},
		Candidates: cands,
	}
}

// Conflicts 返回某工作区的冲突视图。
func (r *Repo) Conflicts(wsID string, onlyOpen bool) ([]ConflictView, error) {
	rows, err := r.DB.ListConflicts(wsID, onlyOpen)
	if err != nil {
		return nil, err
	}
	out := make([]ConflictView, 0, len(rows))
	for i := range rows {
		out = append(out, r.ConflictViewFor(&rows[i]))
	}
	return out, nil
}

// ConflictByKey 按 key 返回冲突视图。
func (r *Repo) ConflictByKey(key hash.ID) (*ConflictView, error) {
	rows, err := r.DB.ListConflictByKey(key)
	if err != nil {
		return nil, err
	}
	v := r.ConflictViewFor(rows)
	return &v, nil
}

// Resolve 记录一条冲突解决（§6.4）。同 key 重复写入返回 (false, nil)。
func (r *Repo) Resolve(key hash.ID, kind op.ResolutionKind, content [][]byte, by string) (bool, error) {
	rows, err := r.DB.ListConflictByKey(key)
	if err != nil {
		return false, err
	}
	res := op.Resolution{
		Key: key, WsID: rows.WsID, Path: rows.Path,
		Kind: kind, Content: content,
	}
	created, err := r.DB.Resolver().Put(res, by)
	if err != nil {
		return false, err
	}
	if created {
		if err := r.DB.MarkConflictResolved(key); err != nil {
			return false, err
		}
	}
	return created, nil
}

// ReopenIfNoConflicts 在工作区冲突全部解决后把状态恢复为 active。
func (r *Repo) ReopenIfNoConflicts(wsID string) error {
	n, err := r.DB.CountOpenConflicts(wsID)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	w, err := r.DB.GetWorkspace(wsID)
	if err != nil {
		return err
	}
	if w.State == meta.StateConflicted {
		return r.DB.UpdateWorkspaceState(wsID, meta.StateActive, time.Now().UnixMilli())
	}
	return nil
}

// Leases 返回租约视图。
func (r *Repo) Leases() ([]map[string]string, error) {
	ls, err := r.DB.ListLeases()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, map[string]string{
			"path": l.Path, "holder_ws": l.WsID, "mode": l.Mode,
		})
	}
	return out, nil
}

// LeaseViolations 检查某工作区的 op 是否触碰了他人独占的租约（§4.5）。
func (r *Repo) LeaseViolations(wsID string) ([]string, error) {
	ls, err := r.DB.ListLeases()
	if err != nil {
		return nil, err
	}
	exclusive := map[string]string{}
	for _, l := range ls {
		if l.Mode == "exclusive" && l.WsID != wsID {
			exclusive[l.Path] = l.WsID
		}
	}
	if len(exclusive) == 0 {
		return nil, nil
	}
	ops, err := r.DB.ListOps(wsID)
	if err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	for _, o := range ops {
		for p, holder := range exclusive {
			if o.Path == p || strings.HasPrefix(o.Path, p+"/") {
				msg := fmt.Sprintf("%s 被 %s 独占（%s）", o.Path, holder, p)
				if !seen[msg] {
					seen[msg] = true
					out = append(out, msg)
				}
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// WalkHead 沿 head 的 parent 链回溯，验证每个版本与其树都可读（fsck 用）。
func (r *Repo) WalkHead(head hash.ID, fn func(id string) error) error {
	cur := head
	seen := 0
	for {
		v, err := r.DB.GetVersion(cur)
		if err != nil {
			// 剪枝边界：gc 会删掉最老的一批版本，链上会留下指向已删除版本的
			// parent。这时**正常结束**，而不是报错 —— 否则剪枝一次之后
			// fsck 就再也跑不起来了。
			return nil
		}
		if _, err := r.Store.GetTree(v.Tree); err != nil {
			return fmt.Errorf("版本 %s 的树 %s 不可读: %w", cur, v.Tree, err)
		}
		if fn != nil {
			if err := fn(cur.String()); err != nil {
				return err
			}
		}
		seen++
		if seen > 1_000_000 {
			return errors.New("head 链过长，疑似存在环")
		}
		if v.IsFirst() {
			return nil
		}
		cur = v.Parent
	}
}
