package remotesync

import (
	"fmt"

	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/repo"
)

// collectVersionObjects 返回这些版本（版本对象 + 各自树的闭包）涉及的
// 全部 CAS 对象及其类型。
//
// 版本对象本身不在树里，必须显式纳入，否则对端拿不到 Version 链，
// version_show / version_diff / blame 都会缺数据。
func collectVersionObjects(r *repo.Repo, versions []hash.ID) (map[hash.ID]uint8, error) {
	set := make(map[hash.ID]uint8, 1024)
	for _, vid := range versions {
		// 版本是**元数据行**（version 表），不是 CAS 对象；这里只要它的树。
		v, err := r.DB.GetVersion(vid)
		if err != nil {
			return nil, fmt.Errorf("remotesync: 读取版本 %s: %w", vid, err)
		}
		if err := collectTree(r, v.Tree, set); err != nil {
			return nil, err
		}
	}
	return set, nil
}

// collectTree 递归收集一棵树及其引用的内容对象。
func collectTree(r *repo.Repo, id hash.ID, set map[hash.ID]uint8) error {
	if _, ok := set[id]; ok {
		return nil
	}
	set[id] = codec.TypeTree
	t, err := r.Store.GetTree(id)
	if err != nil {
		return fmt.Errorf("remotesync: 读取树 %s: %w", id, err)
	}
	for _, e := range t.Entries {
		switch e.Kind {
		case codec.EntryTree:
			if err := collectTree(r, e.ID, set); err != nil {
				return err
			}
		case codec.EntrySymlink:
			set[e.ID] = codec.TypeBlob
		default: // EntryFile：id 指向 FileContent，其 chunks 才指向 Blob
			if _, ok := set[e.ID]; ok {
				continue
			}
			fc, err := r.Store.GetFileContent(e.ID)
			if err != nil {
				return fmt.Errorf("remotesync: 读取文件内容 %s: %w", e.ID, err)
			}
			set[e.ID] = codec.TypeFileContent
			for _, c := range fc.Chunks {
				set[c] = codec.TypeBlob
			}
		}
	}
	return nil
}

// isAncestor 报告 a 是否是 b 的祖先（全零视为所有版本的祖先）。
func isAncestor(r *repo.Repo, a, b hash.ID) (bool, error) {
	if a.IsZero() {
		return true, nil
	}
	if a == b {
		return true, nil
	}
	chain, err := r.DB.VersionChain(b)
	if err != nil {
		return false, err
	}
	for _, id := range chain {
		if id == a {
			return true, nil
		}
	}
	return false, nil
}
