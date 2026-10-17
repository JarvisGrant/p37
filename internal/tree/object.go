// Package tree 实现 FileContent 与 Tree 两种对象的编解码、排序与查找。
//
// 规格依据：附录 A.3。所有编码都是确定性的：同一逻辑对象在任何平台、
// 任何顺序下必须产生同一个 id。
package tree

import (
	"fmt"
	"sort"

	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
)

// FileContent 是一个文件的 chunk 列表 + 总长度 + EOL 属性（附录 A.3）。
//
// 即使单 chunk 文件也使用本对象，不做内联：换取统一处理与未来扩展位
// （§3.1）。代价是对象数增加。
type FileContent struct {
	TotalSize int64
	EOL       uint8 // codec.EOLlf / EOLcrlf / EOLmixed
	Chunks    []hash.ID
}

// Encode 返回 FileContent 的确定性编码（不含 id）。
func (fc *FileContent) Encode() []byte {
	e := codec.NewEncoder()
	e.U8(codec.TypeFileContent)
	e.Uvarint(uint64(fc.TotalSize))
	e.Uvarint(uint64(len(fc.Chunks)))
	e.U8(fc.EOL)
	for _, c := range fc.Chunks {
		e.Hash(c)
	}
	return e.Out()
}

// ID 返回 FileContent 的内容寻址标识。
func (fc *FileContent) ID() hash.ID { return hash.Sum(fc.Encode()) }

// DecodeFileContent 解析 FileContent 编码。
func DecodeFileContent(b []byte) (*FileContent, error) {
	d := codec.NewDecoder(b)
	if typ := d.U8(); typ != codec.TypeFileContent {
		return nil, fmt.Errorf("tree: 期望 FileContent(0x%02x)，得到 0x%02x", codec.TypeFileContent, typ)
	}
	total := d.Uvarint()
	count := d.Uvarint()
	eol := d.U8()
	if err := d.Err(); err != nil {
		return nil, err
	}
	if count > MaxChunksPerFile {
		return nil, fmt.Errorf("tree: chunk 数 %d 超过上限 %d", count, MaxChunksPerFile)
	}
	if eol > codec.EOLmixed {
		return nil, fmt.Errorf("tree: 非法 EOL 值 %d", eol)
	}
	chunks := make([]hash.ID, count)
	for i := range chunks {
		chunks[i] = d.Hash()
	}
	if err := d.Finish(); err != nil {
		return nil, err
	}
	return &FileContent{TotalSize: int64(total), EOL: eol, Chunks: chunks}, nil
}

// MaxChunksPerFile 是单文件 chunk 数上限（§13）。
const MaxChunksPerFile = 1_000_000

// MaxTreeEntries 是单棵树条目数上限（§13）。
const MaxTreeEntries = 1_000_000

// Entry 是树中的一个条目。
type Entry struct {
	Name string // 已 NFC 归一化的单段名
	Mode uint64 // codec.ModeNormal / ModeExecutable
	Kind uint8  // codec.EntryFile / EntryTree / EntrySymlink
	ID   hash.ID
}

// Tree 是一个目录的条目列表，始终按 Name 的原始字节序升序（附录 A.3）。
type Tree struct {
	Entries []Entry
}

// Encode 返回 Tree 的确定性编码。调用方必须保证 Entries 已排序；
// 未排序时会先排序（原地）。
func (t *Tree) Encode() []byte {
	t.Sort()
	e := codec.NewEncoder()
	e.U8(codec.TypeTree)
	e.Uvarint(uint64(len(t.Entries)))
	for _, en := range t.Entries {
		e.Str(en.Name)
		e.Uvarint(en.Mode)
		e.U8(en.Kind)
		e.Hash(en.ID)
	}
	return e.Out()
}

// ID 返回 Tree 的内容寻址标识。
func (t *Tree) ID() hash.ID { return hash.Sum(t.Encode()) }

// Sort 按 Name 的原始字节序升序排序（大小写敏感，§3.3）。
func (t *Tree) Sort() {
	sort.Slice(t.Entries, func(i, j int) bool { return t.Entries[i].Name < t.Entries[j].Name })
}

// DecodeTree 解析 Tree 编码，并校验条目有序且名合法。
func DecodeTree(b []byte) (*Tree, error) {
	d := codec.NewDecoder(b)
	if typ := d.U8(); typ != codec.TypeTree {
		return nil, fmt.Errorf("tree: 期望 Tree(0x%02x)，得到 0x%02x", codec.TypeTree, typ)
	}
	count := d.Uvarint()
	if err := d.Err(); err != nil {
		return nil, err
	}
	if count > MaxTreeEntries {
		return nil, fmt.Errorf("tree: 条目数 %d 超过上限 %d", count, MaxTreeEntries)
	}
	t := &Tree{Entries: make([]Entry, 0, count)}
	for i := uint64(0); i < count; i++ {
		var en Entry
		en.Name = d.Str()
		en.Mode = d.Uvarint()
		en.Kind = d.U8()
		en.ID = d.Hash()
		if err := d.Err(); err != nil {
			return nil, err
		}
		if err := validateName(en.Name); err != nil {
			return nil, err
		}
		if en.Kind > codec.EntrySymlink {
			return nil, fmt.Errorf("tree: 非法条目种类 %d", en.Kind)
		}
		if en.Mode > codec.ModeExecutable {
			return nil, fmt.Errorf("tree: 非法模式 %d", en.Mode)
		}
		if i > 0 && t.Entries[len(t.Entries)-1].Name >= en.Name {
			return nil, fmt.Errorf("tree: 条目未按字节序升序或存在重复: %q >= %q",
				t.Entries[len(t.Entries)-1].Name, en.Name)
		}
		t.Entries = append(t.Entries, en)
	}
	if err := d.Finish(); err != nil {
		return nil, err
	}
	return t, nil
}

// validateName 校验树条目的单段名（§13）。
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("tree: 空条目名")
	}
	if len(name) > 255 {
		return fmt.Errorf("tree: 条目名超过 255 字节: %q", name)
	}
	for i := 0; i < len(name); i++ {
		if name[i] == '/' {
			return fmt.Errorf("tree: 条目名含 /: %q", name)
		}
	}
	switch name {
	case ".", "..":
		return fmt.Errorf("tree: 非法条目名 %q", name)
	}
	return nil
}

// Lookup 二分查找条目，返回其下标；未找到返回 -1。
func (t *Tree) Lookup(name string) int {
	i := sort.Search(len(t.Entries), func(i int) bool { return t.Entries[i].Name >= name })
	if i < len(t.Entries) && t.Entries[i].Name == name {
		return i
	}
	return -1
}

// Get 返回条目。
func (t *Tree) Get(name string) (Entry, bool) {
	if i := t.Lookup(name); i >= 0 {
		return t.Entries[i], true
	}
	return Entry{}, false
}

// Upsert 插入或替换一个条目。返回是否发生了修改。
func (t *Tree) Upsert(en Entry) bool {
	if i := t.Lookup(en.Name); i >= 0 {
		if t.Entries[i] == en {
			return false
		}
		t.Entries[i] = en
		return true
	}
	t.Entries = append(t.Entries, en)
	t.Sort()
	return true
}

// Remove 删除一个条目。返回是否发生了删除。
func (t *Tree) Remove(name string) bool {
	if i := t.Lookup(name); i >= 0 {
		t.Entries = append(t.Entries[:i], t.Entries[i+1:]...)
		return true
	}
	return false
}

// Clone 深拷贝一棵树。
func (t *Tree) Clone() *Tree {
	cp := &Tree{Entries: make([]Entry, len(t.Entries))}
	copy(cp.Entries, t.Entries)
	return cp
}

// Equal 报告两棵树是否逐条目相同（不含 id 计算）。
func (t *Tree) Equal(o *Tree) bool {
	if len(t.Entries) != len(o.Entries) {
		return false
	}
	for i := range t.Entries {
		if t.Entries[i] != o.Entries[i] {
			return false
		}
	}
	return true
}
