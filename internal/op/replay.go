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
	// Follow 把「工作区采集时的旧路径」映射到「目标基准里的新路径」，
	// 用于跟随对侧已经落地的重命名（§6.3 rename_vs_edit）。
	// nil 表示不做跟随。
	Follow map[string]string
	// NoAutoMergePaths 里的路径禁用 §6.3 的不重叠行自动合并。
	//
	// merge_policy=strict 时由调用方填入「父子双方都改过」的路径集合：
	// 同一路径双方都动过就必须显式裁决，不静默取并集。
	NoAutoMergePaths map[string]bool
	// ForceConflictPaths 里的路径跳过 place 直接进入冲突判定。
	//
	// 与 NoAutoMergePaths 的区别：即使 hunk 能在目标里唯一定位（strict 语义下
	// 也算「双方都改过」），也强制交给调用方裁决。
	ForceConflictPaths map[string]bool
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
	// AutoMerged 是本次被 §6.3 规则**自动合并**（不产生冲突）的 hunk 数。
	//
	// 例如两侧改的是同一文件的不同行 —— 这是最常见的「冲突」，
	// 按 §6.3 它根本不需要人来裁决。
	AutoMerged int
	// AutoMergedPaths 是被自动合并消化掉的路径（去重、稳定顺序）。
	AutoMergedPaths []string

	autoMergedSeen map[string]bool
}

func (r *ReplayResult) markAutoMerged(path string) {
	if r.autoMergedSeen == nil {
		r.autoMergedSeen = map[string]bool{}
	}
	if r.autoMergedSeen[path] {
		return
	}
	r.autoMergedSeen[path] = true
	r.AutoMergedPaths = append(r.AutoMergedPaths, path)
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
		return c.conflict(o, ConflictOverlappingEdit, fileHunks(e, c.cs), insertHunks(o))
	}
	if o.NewFile.IsZero() {
		return fmt.Errorf("op: create 缺少 NewFile (op=%s)", o.ID)
	}
	c.entries[o.Path] = tree.Entry{
		Name: baseName(o.Path), Mode: createdMode(o), Kind: o.EntryKindOf(), ID: o.NewFile,
	}
	return nil
}

func (c *replayCtx) applyDelete(o *Op) error {
	e, ok := c.entries[o.Path]
	if !ok {
		return nil // 幂等：已经不存在，视为达成
	}
	if e.ID != o.BaseFile {
		return c.conflict(o, ConflictDeleteVsEdit, fileHunks(e, c.cs), nil)
	}
	delete(c.entries, o.Path)
	return nil
}

func (c *replayCtx) applyAttr(o *Op) error {
	path := c.target(o.Path)
	e, ok := c.entries[path]
	if !ok {
		return c.conflictAt(path, o, ConflictMissingBase, nil, insertHunks(o))
	}
	if e.ID != o.BaseFile {
		// 内容已被第三方改动。若只是 EOL 属性变化，候选为取 upper（§6.3）。
		return c.conflictAt(path, o, ConflictAttr, fileHunks(e, c.cs), insertHunks(o))
	}
	if o.NewFile.IsZero() {
		return fmt.Errorf("op: attr 缺少 NewFile (op=%s)", o.ID)
	}
	e.ID = o.NewFile
	if o.ModeAfter != nil {
		e.Mode = resolveMode(o, e.Mode)
	}
	c.entries[path] = e
	return nil
}

func (c *replayCtx) applyBlobPut(o *Op) error {
	path := c.target(o.Path)
	e, ok := c.entries[path]

	// 文件当前不存在。
	if !ok {
		if !o.BaseFile.IsZero() {
			return c.conflictAt(path, o, ConflictMissingBase, nil, insertHunks(o))
		}
		c.entries[path] = tree.Entry{
			Name: baseName(path), Mode: createdMode(o), Kind: o.EntryKindOf(), ID: o.NewFile,
		}
		return nil
	}

	switch e.ID {
	case o.NewFile:
		if o.ModeAfter != nil {
			e.Mode = resolveMode(o, e.Mode)
			c.entries[path] = e
		}
		return nil // 已经是我们想要的内容：幂等
	case o.BaseFile:
		e.ID = o.NewFile
		if o.ModeAfter != nil {
			e.Mode = resolveMode(o, e.Mode)
		}
		c.entries[path] = e
		return nil
	}

	// 目标已被第三方改动：二进制/大文件永不自动合并（§6.5）。
	return c.conflictAt(path, o, ConflictBinary, fileHunks(e, c.cs), insertHunks(o))
}

func (c *replayCtx) applyRename(o *Op) error {
	e, ok := c.entries[o.Path]
	if !ok {
		return c.conflict(o, ConflictMissingBase, nil, insertHunks(o))
	}
	if o.DestPath == "" {
		return fmt.Errorf("op: rename 缺少 DestPath (op=%s)", o.ID)
	}
	if _, taken := c.entries[o.DestPath]; taken {
		return c.conflict(o, ConflictRenameVsEdit, nil, nil)
	}
	if e.ID != o.BaseFile {
		// §6.3：「rename_vs_edit 且内容未分叉 -> 跟随重命名，在新路径应用」。
		//
		// 内容被对侧改过并不构成冲突 —— 把本侧的内容改动当作 hunk 放到**新路径
		// 的内容**上。只有某个 hunk 确实放不下（两侧改了同一段）才报冲突。
		ne := e
		ne.Name = o.DestPath
		if o.EntryKind != 0 {
			ne.Kind = o.EntryKindOf()
		}
		if o.ModeAfter != nil {
			ne.Mode = resolveMode(o, e.Mode)
		}
		c.entries[o.DestPath] = ne
		delete(c.entries, o.Path)
		if len(o.Hunks) == 0 {
			return nil // 纯改名（本侧没改内容）
		}
		return c.applyHunksTo(o.DestPath, o)
	}

	ne := e
	ne.Name = o.DestPath
	if o.EntryKind != 0 {
		ne.Kind = o.EntryKindOf()
	}
	if o.ModeAfter != nil {
		ne.Mode = resolveMode(o, e.Mode)
	}
	c.entries[o.DestPath] = ne
	delete(c.entries, o.Path)
	// 本侧对内容也有改动时同样放到新路径上。
	if len(o.Hunks) > 0 {
		return c.applyHunksTo(o.DestPath, o)
	}
	return nil
}

func (c *replayCtx) applyModify(o *Op) error { return c.applyHunksTo(c.target(o.Path), o) }

// target 在路径已被对侧重命名时给出新路径。
//
// 仅当旧路径在目标里**不存在**、且跟随目标存在时才改道；否则保持原样，
// 以免误伤「对侧只是新增了同名路径」之类的正常情况。
func (c *replayCtx) target(path string) string {
	if _, ok := c.entries[path]; ok {
		return path
	}
	if c.opt.Follow == nil {
		return path
	}
	np, ok := c.opt.Follow[path]
	if !ok || np == "" {
		return path
	}
	if _, ok := c.entries[np]; ok {
		return np
	}
	return path
}

// applyHunksTo 把 op 的 hunks 放到 path 上（rename 会改道到新路径）。
func (c *replayCtx) applyHunksTo(path string, o *Op) error {
	cur, ok, err := c.contentOf(path)
	if err != nil {
		return err
	}
	if !ok {
		return c.conflictAt(path, o, ConflictMissingBase, nil, insertHunks(o))
	}
	mode := c.entries[path].Mode

	// 逐 hunk 放置，**原地推进**目标。
	placed := cur
	force := c.opt.ForceConflictPaths[path]
	for hi := range o.Hunks {
		h := &o.Hunks[hi]
		if !force {
			if next, ok := Place(h, placed, h.StartHint, c.opt.MaxMatches); ok {
				placed = next
				continue
			}
			// §6.3 第一条规则：两侧改的是**不重叠的行** —— 自动都保留。
			//
			// 判据（要求唯一匹配，不唯一就退回冲突，绝不猜）：
			//   Remove 非空 -> 本侧要替换的那段原文在目标里仍**唯一**存在，
			//                  说明对侧没碰这些行，直接把 Remove 换成 Insert。
			//   Remove 为空 -> 对侧在同一锚点插过内容，本侧内容接在其后。
			if next, ok := autoMerge(h, placed); ok && !c.opt.NoAutoMergePaths[path] {
				placed = next
				c.res.AutoMerged++
				c.res.markAutoMerged(path)
				continue
			}
		}

		// 未决冲突：先看是否已有记录的 resolution（§6.4）。
		lower := Side{OpID: o.ID, Hunks: []Hunk{c.lowerHunk(h, placed)}}
		upper := Side{OpID: o.ID, Hunks: []Hunk{*h}}
		key := ConflictKey(c.opt.WsID, path, lower.ContentHash(), upper.ContentHash())

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
			Path:  path,
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
	c.entries[path] = tree.Entry{
		Name: baseName(path), Mode: resolveMode(o, mode), Kind: codec.EntryFile, ID: id,
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
func (c *replayCtx) conflict(o *Op, kind ConflictKind, lower, upper []Hunk) error {
	return c.conflictAt(o.Path, o, kind, lower, upper)
}

// conflictAt 在指定路径上记录一个非行级冲突（rename 用新路径）。
func (c *replayCtx) conflictAt(path string, o *Op, kind ConflictKind, lower, upper []Hunk) error {
	ls := Side{OpID: o.ID, Hunks: lower}
	us := Side{OpID: o.ID, Hunks: upper}
	key := ConflictKey(c.opt.WsID, path, ls.ContentHash(), us.ContentHash())

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
		Path:  path,
		Kind:  kind,
		Lower: ls,
		Upper: us,
	}
	if c.opt.Candidates {
		conf.Candidat = candidatesFor(conf)
	}
	c.res.Conflicts = append(c.res.Conflicts, conf)
	return nil
}

// applyWholeResolution 对整文件级冲突应用一条已记录的解决。
func (c *replayCtx) applyWholeResolution(o *Op, r Resolution) error {
	switch r.Kind {
	case ResolveTakeUpper:
		switch o.Kind {
		case KindDelete:
			return nil // 放弃删除
		case KindRename:
			// 跟随重命名：把 old 移到 new（内容沿用对侧当前的）。
			if e, ok := c.entries[o.Path]; ok {
				e.Name = o.DestPath
				c.entries[o.DestPath] = e
				delete(c.entries, o.Path)
			}
			return nil
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

// candidatesFor 合成候选解（§6.3）。全部是确定性的、可解释的规则，不引入 LLM；
// 规则覆盖不到的冲突返回空列表，由调用方交给 AI 裁决。
//
// 「两侧改不重叠行」「同锚点插入」这两条会在 applyModify 里被 autoMerge **自动**
// 消化掉，根本不会走到这里；能走到这里的 overlapping_edit 一定是两侧确实改了
// 同一段内容的硬冲突。
func candidatesFor(c Conflict) []Candidate {
	var out []Candidate
	seen := map[ResolutionKind]bool{}
	add := func(k ResolutionKind, note string) {
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, Candidate{Kind: k, Note: note})
	}

	switch c.Kind {
	case ConflictAttr:
		// §6.3：「attr_conflict -> 取 upper」。
		add(ResolveTakeUpper, "采用新的 EOL 属性")
		add(ResolveTakeLower, "保留当前属性")
	case ConflictBinary:
		// §6.3：「binary 且任一侧未变 -> 取变化的一侧」。未变的一侧在重放的
		// 快路径里已经被消化（id 相等即幂等），能走到这里的都是**两侧都变了**，
		// 因此只能二选一。
		add(ResolveTakeUpper, "采用本侧的新内容")
		add(ResolveTakeLower, "保留当前内容")
	case ConflictDeleteVsEdit:
		// §6.3：「delete_vs_edit 且修改全落在被删区间内 -> 取删除」。
		// 本实现的 delete 是**整文件**删除，被删区间即整个文件，
		// 修改必然落在其中，因此「取删除」总是可用，候选固定为两条。
		add(ResolveTakeLower, "仍然删除（取删除）")
		add(ResolveTakeUpper, "保留被删文件（放弃 delete）")
	case ConflictRenameVsEdit:
		add(ResolveTakeLower, "放弃重命名，保留原路径")
		add(ResolveTakeUpper, "跟随重命名，采用本侧内容")
	case ConflictOverlappingEdit:
		add(ResolveTakeLower, "保留当前内容")
		add(ResolveTakeUpper, "采用本侧内容")
		if len(c.Upper.Hunks) == 1 && len(c.Upper.Hunks[0].Remove) == 0 {
			// §6.3：「两侧在同锚点插入 -> 按 op seq 拼接」。本侧 op 的 seq
			// 必然晚于目标里已有的内容，因此本侧拼接在**后面**。
			add(ResolveBoth, "两侧都保留（本侧内容拼在当前内容之后）")
		}
	case ConflictMissingBase:
		add(ResolveTakeUpper, "按本侧内容创建")
		add(ResolveTakeLower, "放弃本侧改动")
	}
	return out
}
