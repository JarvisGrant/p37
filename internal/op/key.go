package op

import (
	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
)

// 本文件实现 §6.4 的冲突标识与解决。
//
// ConflictKey = BLAKE3( "p37-conflict-v1" || ws_id || path
//                      || lower_content_hash || upper_content_hash )
//
// **key 中不得包含 op_id / hunk_index（G7）**：ops 表被定义为可重建的纯缓存
// （§5.4），重建后 op_id 会重新分配；若 key 含它们，一次崩溃恢复就会让所有
// 已记录的 resolution 静默失效，直接违反 §14.1 的不变量 5。
//
// 去掉这两个字段后，两侧内容完全相同但位于不同位置的冲突会得到同一个 key。
// 这是**良性碰撞**：内容相同则正确的解决方式本来就相同。

// keyDomain 是 key 的域分隔前缀。
const keyDomain = "p37-conflict-v1"

var keyDomainBytes = []byte(keyDomain)

// ConflictKind 是冲突分类（§6.2）。
type ConflictKind string

// 冲突分类取值。
const (
	ConflictOverlappingEdit ConflictKind = "overlapping_edit"
	ConflictDeleteVsEdit    ConflictKind = "delete_vs_edit"
	ConflictRenameVsEdit    ConflictKind = "rename_vs_edit"
	ConflictBinary          ConflictKind = "binary"
	ConflictMissingBase     ConflictKind = "missing_base"
	ConflictAttr            ConflictKind = "attr_conflict"
)

// Valid 报告冲突分类是否已知。
func (c ConflictKind) Valid() bool {
	switch c {
	case ConflictOverlappingEdit, ConflictDeleteVsEdit, ConflictRenameVsEdit,
		ConflictBinary, ConflictMissingBase, ConflictAttr:
		return true
	}
	return false
}

// Side 描述冲突的一侧。
type Side struct {
	OpID    string
	EOL     uint8
	Content [][]byte // 该侧的内容（去掉了上下文，即插入后的结果片段）
	Hunks   []Hunk
}

// ContentHash 返回该侧内容的哈希，用于 ConflictKey。
//
// 定义为 BLAKE3(canonical(before, remove, after)) 各行——即完整的模式内容，
// 而不是插入结果。任一字节变化都会改变 key，从而使旧 resolution 自动失效。
func (s *Side) ContentHash() hash.ID {
	e := codec.NewEncoder()
	e.Str(keyDomain)
	e.Uvarint(uint64(len(s.Hunks)))
	for i := range s.Hunks {
		h := &s.Hunks[i]
		e.Uvarint(uint64(len(h.Before)))
		for _, l := range h.Before {
			e.Bytes(l)
		}
		e.Uvarint(uint64(len(h.Remove)))
		for _, l := range h.Remove {
			e.Bytes(l)
		}
		e.Uvarint(uint64(len(h.Insert)))
		for _, l := range h.Insert {
			e.Bytes(l)
		}
		e.Uvarint(uint64(len(h.After)))
		for _, l := range h.After {
			e.Bytes(l)
		}
	}
	return hash.Sum(e.Out())
}

// ConflictKey 由 §6.4 的公式计算冲突 key。
func ConflictKey(wsID, path string, lower, upper hash.ID) hash.ID {
	return hash.SumParts(
		keyDomainBytes,
		[]byte(wsID),
		[]byte{0},
		[]byte(path),
		[]byte{0},
		lower[:],
		upper[:],
	)
}

// Conflict 是一次待决冲突。
type Conflict struct {
	Key      hash.ID
	WsID     string
	Path     string
	Kind     ConflictKind
	Lower    Side
	Upper    Side
	Base     [][]byte // 冲突位置在基准中的原始片段（含上下文）
	Candidat []Candidate
}

// Candidate 是一个候选解（§6.3）。
type Candidate struct {
	Kind    ResolutionKind
	Content [][]byte // custom 时的替换内容
	Note    string
}

// ResolutionKind 是解决方式。
type ResolutionKind string

// 解决方式取值。
const (
	ResolveTakeLower ResolutionKind = "take_lower"
	ResolveTakeUpper ResolutionKind = "take_upper"
	ResolveBoth      ResolutionKind = "both"
	ResolveCustom    ResolutionKind = "custom"
)

// Valid 报告解决方式是否已知。
func (r ResolutionKind) Valid() bool {
	switch r {
	case ResolveTakeLower, ResolveTakeUpper, ResolveBoth, ResolveCustom:
		return true
	}
	return false
}

// Resolution 是一条不可变的解决记录（§6.4）。
type Resolution struct {
	Key       hash.ID
	WsID      string
	Path      string
	LowerHash hash.ID
	UpperHash hash.ID
	Kind      ResolutionKind
	Content   [][]byte // custom 时的替换行
}

// Resolver 按 ConflictKey 查已记录的解决。未命中返回 (zero, false, nil)。
type Resolver interface {
	Lookup(key hash.ID) (Resolution, bool, error)
}

// MemResolver 是 Resolver 的内存实现（测试与单次 land 用）。
type MemResolver struct {
	M map[string]Resolution
}

// NewMemResolver 返回空解析器。
func NewMemResolver() *MemResolver { return &MemResolver{M: map[string]Resolution{}} }

// Lookup 实现 Resolver。
func (m *MemResolver) Lookup(key hash.ID) (Resolution, bool, error) {
	r, ok := m.M[key.String()]
	return r, ok, nil
}

// Put 记录一条解决。已存在且不完全相同时返回 false。
func (m *MemResolver) Put(r Resolution) bool {
	k := r.Key.String()
	if old, ok := m.M[k]; ok {
		return old.Kind == r.Kind
	}
	m.M[k] = r
	return true
}

var _ Resolver = (*MemResolver)(nil)
