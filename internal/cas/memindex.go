package cas

import (
	"sort"
	"sync"

	"github.com/chenyiwen/p37/internal/hash"
)

// MemIndex 是 ObjectIndex 的内存实现。
//
// 生产环境使用元数据库（附录 J 的 object_index 表）；内存实现用于测试，
// 以及索引损坏时的临时降级路径。
type MemIndex struct {
	mu sync.RWMutex
	m  map[hash.ID]objInfo
}

type objInfo struct {
	typ     uint8
	size    int64
	rawSize int64
}

// NewMemIndex 返回空索引。
func NewMemIndex() *MemIndex { return &MemIndex{m: make(map[hash.ID]objInfo)} }

// Has 实现 ObjectIndex。
func (mi *MemIndex) Has(id hash.ID) (bool, error) {
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	_, ok := mi.m[id]
	return ok, nil
}

// Type 实现 ObjectIndex。
func (mi *MemIndex) Type(id hash.ID) (uint8, error) {
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	info, ok := mi.m[id]
	if !ok {
		return 0, ErrNotExist
	}
	return info.typ, nil
}

// Add 实现 ObjectIndex。
func (mi *MemIndex) Add(id hash.ID, typ uint8, size, rawSize int64) error {
	mi.mu.Lock()
	defer mi.mu.Unlock()
	mi.m[id] = objInfo{typ: typ, size: size, rawSize: rawSize}
	return nil
}

// Remove 实现 ObjectIndex。
func (mi *MemIndex) Remove(id hash.ID) error {
	mi.mu.Lock()
	defer mi.mu.Unlock()
	delete(mi.m, id)
	return nil
}

// Iterate 实现 ObjectIndex，按 id 的字节序升序回调。
func (mi *MemIndex) Iterate(fn func(id hash.ID, typ uint8, size, rawSize int64) error) error {
	mi.mu.RLock()
	ids := make([]hash.ID, 0, len(mi.m))
	for id := range mi.m {
		ids = append(ids, id)
	}
	mi.mu.RUnlock()

	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	for _, id := range ids {
		mi.mu.RLock()
		info, ok := mi.m[id]
		mi.mu.RUnlock()
		if !ok {
			continue
		}
		if err := fn(id, info.typ, info.size, info.rawSize); err != nil {
			return err
		}
	}
	return nil
}

// Len 返回索引中的对象数（测试用）。
func (mi *MemIndex) Len() int {
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	return len(mi.m)
}

var _ ObjectIndex = (*MemIndex)(nil)
