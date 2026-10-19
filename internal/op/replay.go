package op

import (
	"errors"
	"fmt"
	"strings"

	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/tree"
)

// 本文件实现 §6.5 的各类 op 重放语义。
//
// 与 §5.1 字段的对应关系（实现后明确化）：
//
//	BaseFile  ≡ 期望的当前 FileContent id；全零表示「该路径不应存在」
//	NewFile   ≡ create / attr / blob_put 的新 FileContent id（≡ blob_after）
//
// create / attr / blob_put 的内容直接以 FileContent id 携带（内容已在 CAS 中
// 去重），因此不需要把整文件塞进 hunks。

// ErrNoContent 表示对象存储中缺少所需对象。
var ErrNoContent = errors.New("op: 缺少内容对象")

// ContentStore 提供文件内容的读取与写入，供重放引擎使用。
type ContentStore interface {
	// GetFileContent 读取 FileContent 对象。
	GetFileContent(id hash.ID) (*tree.FileContent, error)
	// ReadFileData 拼接出文件的原始字节。
	ReadFileData(id hash.ID) ([]byte, error)
	// ReadBlob 读取一个 Blob 的原文（符号链接的目标以 Blob 存放）。
	ReadBlob(id hash.ID) ([]byte, error)
	// PutFileData 对内容分块、写入 CAS，返回新的 FileContent id。
	PutFileData(data []byte) (hash.ID, error)
	// GetTree 读取 Tree 对象（重放需要展开嵌套路径）。
	GetTree(id hash.ID) (*tree.Tree, error)
	// PutTree 写入 Tree 对象（重放结束后重建嵌套树）。
	PutTree(t *tree.Tree) (hash.ID, error)
}

// Options 控制一次重放。
type Options struct {
	// WsID 参与 ConflictKey 的计算（§6.4）。
	WsID string
	// MaxMatches 是 place 的匹配上限；0 表示 DefaultMaxMatches。
	MaxMatches int
	// Resolver 查询已记录的解决；nil 表示不查。
	Resolver Resolver
	// Candidates 为真时合成候选解（§6.3）。
	Candidates bool
}

// ReplayResult 是一次重放的结果。
type ReplayResult struct {
	// Tree 是重放成功后的树；存在未决冲突时**仍然完整**（可继续 land 预览），
	// 但调用方必须检查 Conflicts 是否为空才能提交。
	Tree *tree.Tree
	// Conflicts 是未决冲突，按出现顺序。
	Conflicts []Conflict
	// Resolved 是本次通过已记录 resolution 自动消解的冲突数。
	Resolved int
}

// Replay 把 ops 依次重放到 base 树上（§6.5）。
//
// 确定性：同一 (base, ops, cs, opt) 必须产生完全相同的 Tree 与 Conflicts。
func Replay(base *tree.Tree, ops []Op, cs ContentStore, opt Options) (*ReplayResult, error) {
	if opt.MaxMatches <= 0 {
		opt.MaxMatches = DefaultMaxMatches
	}

	// op 携带完整路径，因此先在扁平命名空间上工作，最后重建嵌套树。
	entries, err := flatten(cs, base)
	if err != nil {
		return nil, err
	}

	res := &ReplayResult{}

	ctx := &replayCtx{
		entries: entries,
		cs:      cs,
		opt:     opt,
		res:     res,
	}

	for i := range ops {
		if err := ctx.apply(&ops[i]); err != nil {
			return nil, err
		}
	}

	out, err := unflatten(cs, entries)
	if err != nil {
		return nil, err
	}
	res.Tree = out
	return res, nil
}

type replayCtx struct {
	entries map[string]tree.Entry
	cs      ContentStore
	opt     Options
	res     *ReplayResult
}

// apply 分派到各 kind 的规则。
func (c *replayCtx) apply(o *Op) error {
	switch o.Kind {
	case KindCreate:
		return c.applyCreate(o)
	case KindModify:
		return c.applyModify(o)
	case KindDelete:
		return c.applyDelete(o)
	case KindRename:
		return c.applyRename(o)
	case KindAttr:
		return c.applyAttr(o)
	case KindBlobPut:
		return c.applyBlobPut(o)
	default:
		return fmt.Errorf("op: 未知 kind %q (op=%s)", o.Kind, o.ID)
	}
}

// contentOf 返回路径当前的内容行；文件不存在返回 (nil, false, nil)。
func (c *replayCtx) contentOf(path string) ([][]byte, bool, error) {
	e, ok := c.entries[path]
	if !ok || e.Kind != codec.EntryFile {
		return nil, false, nil
	}
	data, err := c.cs.ReadFileData(e.ID)
	if err != nil {
		return nil, false, err
	}
	return tree.SplitLines(data), true, nil
}

func (c *replayCtx) applyCreate(o *Op) error {
	if e, ok := c.entries[o.Path]; ok {
		return c.conflict(o, ConflictOverlappingEdit, fileHunks(e, c.cs), insertHunks(o), nil)
	}
	if o.NewFile.IsZero() {
		return fmt.Errorf("op: create 缺少 NewFile (op=%s)", o.ID)
	}
	c.entries[o.Path] = tree.Entry{
		Name: baseName(o.Path), Mode: codec.ModeNormal, Kind: o.EntryKindOf(), ID: o.NewFile,
	}
	return nil
}

func (c *replayCtx) applyDelete(o *Op) error {
	e, ok := c.entries[o.Path]
	if !ok {
		return nil // 幂等：已经不存在，视为达成
	}
	if e.ID != o.BaseFile {
		return c.conflict(o, ConflictDeleteVsEdit, fileHunks(e, c.cs), nil,
			[]Candidate{{Kind: ResolveTakeUpper, Note: "保留被删文件（即放弃 delete）"},
				{Kind: ResolveTakeLower, Note: "仍然删除"}})
	}
	delete(c.entries, o.Path)
	return nil
}

func (c *replayCtx) applyAttr(o *Op) error {
	e, ok := c.entries[o.Path]
	if !ok {
		return c.conflict(o, ConflictMissingBase, nil, insertHunks(o), nil)
	}
	if e.ID != o.BaseFile {
		// 内容已被第三方改动。若只是 EOL 属性变化，候选为取 upper（§6.3）。
		return c.conflict(o, ConflictAttr, fileHunks(e, c.cs), insertHunks(o),
			[]Candidate{{Kind: ResolveTakeUpper, Note: "采用新的 EOL 属性"}})
	}
	if o.NewFile.IsZero() {
		return fmt.Errorf("op: attr 缺少 NewFile (op=%s)", o.ID)
	}
	e.ID = o.NewFile
	c.entries[o.Path] = e
	return nil
}

func (c *replayCtx) applyBlobPut(o *Op) error {
	e, ok := c.entries[o.Path]

	// 文件当前不存在。
	if !ok {
		if !o.BaseFile.IsZero() {
			return c.conflict(o, ConflictMissingBase, nil, insertHunks(o), nil)
		}
		c.entries[o.Path] = tree.Entry{
			Name: o.Path, Mode: codec.ModeNormal, Kind: o.EntryKindOf(), ID: o.NewFile,
		}
		return nil
	}

	switch e.ID {
	case o.NewFile:
		return nil // 已经是我们想要的内容：幂等
	case o.BaseFile:
		e.ID = o.NewFile
		c.entries[o.Path] = e
		return nil
	}

	// 目标已被第三方改动：二进制/大文件永不自动合并（§6.5）。
	return c.conflict(o, ConflictBinary, fileHunks(e, c.cs), insertHunks(o),
		[]Candidate{{Kind: ResolveTakeLower, Note: "保留当前内容"},
			{Kind: ResolveTakeUpper, Note: "采用新内容"}})
}

func (c *replayCtx) applyRename(o *Op) error {
	e, ok := c.entries[o.Path]
	if !ok {
		return c.conflict(o, ConflictMissingBase, nil, insertHunks(o), nil)
	}
	if o.DestPath == "" {
		return fmt.Errorf("op: rename 缺少 DestPath (op=%s)", o.ID)
	}
	if _, taken := c.entries[o.DestPath]; taken {
		return c.conflict(o, ConflictRenameVsEdit, nil, nil,
			[]Candidate{{Kind: ResolveTakeLower, Note: "放弃重命名，保留原路径"}})
	}
	if e.ID != o.BaseFile {
		return c.conflict(o, ConflictRenameVsEdit, fileHunks(e, c.cs), insertHunks(o), nil)
	}

	ne := e
	ne.Name = o.DestPath
	if o.EntryKind != 0 {
		ne.Kind = o.EntryKindOf()
	}
	c.entries[o.DestPath] = ne
	delete(c.entries, o.Path)
	return nil
}

func (c *replayCtx) applyModify(o *Op) error {
	cur, ok, err := c.contentOf(o.Path)
	if err != nil {
		return err
	}
	if !ok {
		return c.conflict(o, ConflictMissingBase, nil, insertHunks(o), nil)
	}

	// 逐 hunk 放置，**原地推进**目标。
	placed := cur
	for hi := range o.Hunks {
		h := &o.Hunks[hi]
		next, ok := Place(h, placed, h.StartHint, c.opt.MaxMatches)
		if ok {
			placed = next
			continue
		}

		// 未决冲突：先看是否已有记录的 resolution（§6.4）。
		lower := Side{OpID: o.ID, Hunks: []Hunk{c.lowerHunk(h, placed)}}
		upper := Side{OpID: o.ID, Hunks: []Hunk{*h}}
		key := ConflictKey(c.opt.WsID, o.Path, lower.ContentHash(), upper.ContentHash())

		if r, found, err := c.lookup(key); err != nil {
			return err
		} else if found {
			placed = applyResolution(r, placed, h)
			c.res.Resolved++
			continue
		}

		conf := Conflict{
			Key:   key,
			WsID:  c.opt.WsID,
			Path:  o.Path,
			Kind:  ConflictOverlappingEdit,
			Lower: lower,
			Upper: upper,
			Base:  h.Before,
		}
		if c.opt.Candidates {
			conf.Candidat = candidatesFor(conf)
		}
		c.res.Conflicts = append(c.res.Conflicts, conf)
		// 冲突不改变目标；保留当前内容继续处理后续 op（预览语义）。
	}

	data := tree.JoinLines(placed)
	id, err := c.cs.PutFileData(data)
	if err != nil {
		return err
	}
	c.entries[o.Path] = tree.Entry{
		Name: baseName(o.Path), Mode: codec.ModeNormal, Kind: codec.EntryFile, ID: id,
	}
	return nil
}

// lowerHunk 为「当前实际内容」构造冲突下侧，取 hunk 锚点所覆盖的位置。
//
// 位置以 StartHint 为基准并做钳制，因此总是良定义的；这使 conflict 的
// lower 侧精确反映**目标当前的内容**，从而让 ConflictKey 正确承载
// 「自动失效」语义（§6.4）。
func (c *replayCtx) lowerHunk(h *Hunk, placed [][]byte) Hunk {
	start := clamp(h.StartHint, 0, len(placed))
	end := min(len(placed), start+len(h.Remove))
	return Hunk{
		Before: sliceOf(placed, max(0, start-len(h.Before)), start),
		Remove: sliceOf(placed, start, end),
		After:  sliceOf(placed, end, min(len(placed), end+len(h.After))),
	}
}

// baseName 返回路径的最后一段。
//
// 注意：条目存在扁平命名空间里（key 是完整路径），但 tree.Entry.Name 必须只是
// 单段名 —— unflatten 会自行拆分路径，这里保持一致以免日后被误用。
func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (c *replayCtx) lookup(key hash.ID) (Resolution, bool, error) {
	if c.opt.Resolver == nil {
		return Resolution{}, false, nil
	}
	return c.opt.Resolver.Lookup(key)
}

// conflict 记录一个非行级冲突。
func (c *replayCtx) conflict(o *Op, kind ConflictKind, lower, upper []Hunk, cands []Candidate) error {
	ls := Side{OpID: o.ID, Hunks: lower}
	us := Side{OpID: o.ID, Hunks: upper}
	key := ConflictKey(c.opt.WsID, o.Path, ls.ContentHash(), us.ContentHash())

	if r, found, err := c.lookup(key); err != nil {
		return err
	} else if found {
		c.res.Resolved++
		if err := c.applyWholeResolution(o, r); err != nil {
			return err
		}
		return nil
	}

	conf := Conflict{
		Key:   key,
		WsID:  c.opt.WsID,
		Path:  o.Path,
		Kind:  kind,
		Lower: ls,
		Upper: us,
	}
	if c.opt.Candidates {
		conf.Candidat = cands
	}
	c.res.Conflicts = append(c.res.Conflicts, conf)
	return nil
}

// applyWholeResolution 对整文件级冲突应用一条已记录的解决。
func (c *replayCtx) applyWholeResolution(o *Op, r Resolution) error {
	switch r.Kind {
	case ResolveTakeUpper:
		if o.Kind == KindDelete {
			return nil // 放弃删除
		}
		if !o.NewFile.IsZero() {
			c.entries[o.Path] = tree.Entry{
				Name: o.Path, Mode: codec.ModeNormal, Kind: o.EntryKindOf(), ID: o.NewFile,
			}
		}
		return nil
	case ResolveTakeLower:
		if o.Kind == KindDelete {
			delete(c.entries, o.Path)
		}
		return nil
	default:
		// both / custom 对整文件级冲突无确定语义，视为未解决。
		return nil
	}
}

// applyResolution 对行级冲突应用一条已记录的解决。
//
// 语义（实现后明确化，务必与 §6.4 的「自动失效」配合理解）：
//
//	take_upper  先按常规 place 尝试；锚点已被破坏时，退化为「在 hint 位置
//	            用本侧的 insert 替换等长的当前行」——总是良定义，永不失败。
//	take_lower  保持目标现状（恒等）。
//	both        在冲突位置**之前**插入本侧内容，保留当前内容。
//	custom      在冲突位置用给定内容替换等长的当前行。
//
// 之所以允许在锚点失效时按 hint 落位：真正的语义保真由 ConflictKey 承担
// —— 只要目标当前内容与记录 resolution 时不同，key 就会变化，冲突会自动
// 重新浮现，交回给调用方决策。
func applyResolution(r Resolution, placed [][]byte, h *Hunk) [][]byte {
	switch r.Kind {
	case ResolveTakeLower:
		return placed
	case ResolveTakeUpper:
		if out, ok := Place(h, placed, h.StartHint, DefaultMaxMatches); ok {
			return out
		}
		return spliceAt(placed, h.StartHint, len(h.Remove), h.Insert)
	case ResolveBoth:
		return spliceAt(placed, h.StartHint, 0, h.Insert)
	case ResolveCustom:
		return spliceAt(placed, h.StartHint, len(h.Remove), r.Content)
	default:
		return placed
	}
}

// spliceAt 把 placed[start:start+n] 替换为 ins；索引自动钳制。
func spliceAt(placed [][]byte, start, n int, ins [][]byte) [][]byte {
	start = clamp(start, 0, len(placed))
	end := min(len(placed), start+n)
	if end < start {
		end = start
	}
	out := make([][]byte, 0, len(placed)-(end-start)+len(ins))
	out = append(out, placed[:start]...)
	out = append(out, ins...)
	out = append(out, placed[end:]...)
	return out
}

// fileHunks 把一个已存在的文件表示为「整体替换」的 hunk（用于冲突展示）。
func fileHunks(e tree.Entry, cs ContentStore) []Hunk {
	data, err := cs.ReadFileData(e.ID)
	if err != nil {
		return nil
	}
	return []Hunk{{Remove: tree.SplitLines(data)}}
}

// insertHunks 把一个 op 表示为「整体插入」的 hunk（用于冲突展示）。
func insertHunks(o *Op) []Hunk {
	if !o.NewFile.IsZero() {
		return []Hunk{{Insert: [][]byte{[]byte("(" + o.NewFile.String() + ")")}}}
	}
	if len(o.Hunks) > 0 {
		return o.Hunks
	}
	return nil
}

func sliceOf(src [][]byte, from, to int) [][]byte {
	if from >= to || from < 0 {
		return nil
	}
	to = min(to, len(src))
	from = min(from, to)
	out := make([][]byte, 0, to-from)
	out = append(out, src[from:to]...)
	return out
}

// candidatesFor 合成候选解（§6.3）。仅实现确定性最强的几条规则；
// 无法确定时返回空列表（调用方应交给 AI 决策）。
func candidatesFor(c Conflict) []Candidate {
	var out []Candidate
	out = append(out, Candidate{Kind: ResolveTakeLower, Note: "保留当前内容"})
	out = append(out, Candidate{Kind: ResolveTakeUpper, Note: "采用来者的内容"})
	if len(c.Upper.Hunks) == 1 && len(c.Upper.Hunks[0].Remove) == 0 {
		out = append(out, Candidate{Kind: ResolveBoth, Note: "两侧都保留（插入到当前内容之后）"})
	}
	return out
}
