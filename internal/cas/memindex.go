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
	m  map[ObjectKey]objInfo
}

type objInfo struct {
	size    int64
	rawSize int64
}

// NewMemIndex 返回空索引。
func NewMemIndex() *MemIndex { return &MemIndex{m: make(map[ObjectKey]objInfo)} }

// Has 实现 ObjectIndex。
func (mi *MemIndex) Has(id hash.ID) (bool, error) {
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	for k := range mi.m {
		if k.ID == id {
			return true, nil
		}
	}
	return false, nil
}

// HasType 实现 ObjectIndex。
func (mi *MemIndex) HasType(id hash.ID, typ uint8) (bool, error) {
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	_, ok := mi.m[ObjectKey{ID: id, Type: typ}]
	return ok, nil
}

// Type 实现 ObjectIndex。存在多个类型时返回 ErrAmbiguousType。
func (mi *MemIndex) Type(id hash.ID) (uint8, error) {
	ts, err := mi.Types(id)
	if err != nil {
		return 0, err
	}
	switch len(ts) {
	case 0:
		return 0, ErrNotExist
	case 1:
		return ts[0], nil
	default:
		return 0, ErrAmbiguousType
	}
}

// Types 实现 ObjectIndex。
func (mi *MemIndex) Types(id hash.ID) ([]uint8, error) {
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	var out []uint8
	for k := range mi.m {
		if k.ID == id {
			out = append(out, k.Type)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// Add 实现 ObjectIndex。
func (mi *MemIndex) Add(id hash.ID, typ uint8, size, rawSize int64) error {
	mi.mu.Lock()
	defer mi.mu.Unlock()
	mi.m[ObjectKey{ID: id, Type: typ}] = objInfo{size: size, rawSize: rawSize}
	return nil
}

// AddMany 实现 ObjectIndex。
func (mi *MemIndex) AddMany(entries []IndexEntry) error {
	mi.mu.Lock()
	defer mi.mu.Unlock()
	for _, e := range entries {
		mi.m[e.Key] = objInfo{size: e.Size, rawSize: e.RawSize}
	}
	return nil
}

// Remove 实现 ObjectIndex：删除该 id 的全部类型。
func (mi *MemIndex) Remove(id hash.ID) error {
	mi.mu.Lock()
	defer mi.mu.Unlock()
	for k := range mi.m {
		if k.ID == id {
			delete(mi.m, k)
		}
	}
	return nil
}

// RemoveType 实现 ObjectIndex。
func (mi *MemIndex) RemoveType(id hash.ID, typ uint8) error {
	mi.mu.Lock()
	defer mi.mu.Unlock()
	delete(mi.m, ObjectKey{ID: id, Type: typ})
	return nil
}

// Iterate 实现 ObjectIndex，按 (id,type) 升序回调。
func (mi *MemIndex) Iterate(fn func(id hash.ID, typ uint8, size, rawSize int64) error) error {
	mi.mu.RLock()
	keys := make([]ObjectKey, 0, len(mi.m))
	for k := range mi.m {
		keys = append(keys, k)
	}
	mi.mu.RUnlock()

	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ID != keys[j].ID {
			return keys[i].ID.String() < keys[j].ID.String()
		}
		return keys[i].Type < keys[j].Type
	})
	for _, k := range keys {
		mi.mu.RLock()
		info, ok := mi.m[k]
		mi.mu.RUnlock()
		if !ok {
			continue
		}
		if err := fn(k.ID, k.Type, info.size, info.rawSize); err != nil {
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
