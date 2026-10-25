package repo

import (
	"sort"

	"github.com/chenyiwen/p37/internal/cas"
	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
)

// flatTreeEntry 是树的一个叶子条目（目录本身不参与路径级比较）。
type flatTreeEntry struct {
	kind uint8
	mode uint64
	id   hash.ID
}

// flattenStoreTree 把一棵树压成 仓库相对路径 -> 条目。
func flattenStoreTree(store *cas.Store, root hash.ID) (map[string]flatTreeEntry, error) {
	out := make(map[string]flatTreeEntry, 256)
	var walk func(id hash.ID, prefix string) error
	walk = func(id hash.ID, prefix string) error {
		t, err := store.GetTree(id)
		if err != nil {
			return err
		}
		for _, e := range t.Entries {
			p := e.Name
			if prefix != "" {
				p = prefix + "/" + e.Name
			}
			if e.Kind == codec.EntryTree {
				if err := walk(e.ID, p); err != nil {
					return err
				}
				continue
			}
			out[p] = flatTreeEntry{kind: e.Kind, mode: e.Mode, id: e.ID}
		}
		return nil
	}
	if err := walk(root, ""); err != nil {
		return nil, err
	}
	return out, nil
}

// diffFlatPaths 返回两棵树在叶子路径上的差异（新增、删除、内容/类型/权限变化）。
func diffFlatPaths(a, b map[string]flatTreeEntry) []string {
	set := make(map[string]struct{}, len(a)+len(b))
	for p := range a {
		set[p] = struct{}{}
	}
	for p := range b {
		set[p] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		ae, aok := a[p]
		be, bok := b[p]
		if aok && bok && ae.kind == be.kind && ae.mode == be.mode && ae.id == be.id {
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// intersectionPaths 返回两个已排序路径切片的交集。
func intersectionPaths(a, b []string) []string {
	set := make(map[string]struct{}, len(b))
	for _, p := range b {
		set[p] = struct{}{}
	}
	out := make([]string, 0, len(a))
	for _, p := range a {
		if _, ok := set[p]; ok {
			out = append(out, p)
		}
	}
	return out
}

// subtractPaths 返回 a 中不在 b 里的路径（a、b 均已排序）。
func subtractPaths(a, b []string) []string {
	set := make(map[string]struct{}, len(b))
	for _, p := range b {
		set[p] = struct{}{}
	}
	out := make([]string, 0, len(a))
	for _, p := range a {
		if _, ok := set[p]; !ok {
			out = append(out, p)
		}
	}
	return out
}

// subsetOf 报告 a 的每个元素都在 b 里。
func subsetOf(a, b []string) (string, bool) {
	set := make(map[string]struct{}, len(b))
	for _, p := range b {
		set[p] = struct{}{}
	}
	for _, p := range a {
		if _, ok := set[p]; !ok {
			return p, false
		}
	}
	return "", true
}
