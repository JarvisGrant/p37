package workspace

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
)

// VerifyBeforeWrite 校验一组路径的磁盘内容是否仍等于 before 树。
//
// 这是 land_into 写盘前的最后一道乐观锁：即使调用方没传 expected_parent_tree，
// 只要父工作区在「读树 → 重放」窗口里被别人（agent / rebase / 另一个采集）
// 改过，也会在这里被拦住，返回具体路径，而不是覆盖。
//
// 只校验 applySet（即将写入的路径），成本与子会话改动规模相关，与仓库大小无关。
// 调用方必须持有工作区写锁。
func (m *Manager) VerifyBeforeWrite(wsID string, before hash.ID, paths []string) ([]string, error) {
	want, err := m.flattenEntries(before)
	if err != nil {
		return nil, err
	}
	dir := m.mm.WorkspaceDir(wsID)
	var mismatched []string
	for _, p := range paths {
		if _, err := codec.ValidatePath(p); err != nil {
			return nil, fmt.Errorf("%w: 非法路径 %q", ErrWrongState, p)
		}
		exp, exists := want[p]
		if exists && exp.kind == codec.EntryTree {
			continue // 目录本身不落盘；其下的叶子路径各自校验
		}
		abs := filepath.Join(dir, filepath.FromSlash(p))
		act, actExists, err := readDiskEntry(abs)
		if err != nil {
			return nil, err
		}
		if !exists && !actExists {
			continue
		}
		if !exists {
			// 「目录 → 文件」是合法的类型变更：只要目录下的每个叶子都在
			// before 树里（本次会随删除一并清理），就不算父被并发改动。
			if act.kind == codec.EntryTree {
				ok := true
				walkErr := filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if d.IsDir() {
						return nil
					}
					rel, rerr := filepath.Rel(dir, path)
					if rerr != nil {
						return rerr
					}
					rel = filepath.ToSlash(rel)
					if _, found := want[rel]; !found {
						ok = false
						return filepath.SkipAll
					}
					return nil
				})
				if walkErr != nil {
					return nil, walkErr
				}
				if ok {
					continue
				}
			}
			mismatched = append(mismatched, p)
			continue
		}
		if !actExists {
			mismatched = append(mismatched, p)
			continue
		}
		if exp.kind != act.kind || exp.mode != act.mode {
			mismatched = append(mismatched, p)
			continue
		}
		switch exp.kind {
		case codec.EntrySymlink:
			wantTarget, err := m.store.ReadBlob(exp.id)
			if err != nil {
				return nil, err
			}
			if !bytes.Equal(wantTarget, act.data) {
				mismatched = append(mismatched, p)
			}
		default:
			wantData, err := m.store.ReadFileData(exp.id)
			if err != nil {
				return nil, err
			}
			if !bytes.Equal(wantData, act.data) {
				mismatched = append(mismatched, p)
			}
		}
	}
	return mismatched, nil
}

type diskEntry struct {
	kind uint8
	mode uint64
	data []byte
}

// readDiskEntry 读取磁盘条目的内容、类型与权限位。
func readDiskEntry(abs string) (diskEntry, bool, error) {
	fi, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return diskEntry{}, false, nil
		}
		return diskEntry{}, false, err
	}
	mode := uint64(codec.ModeNormal)
	if fi.Mode()&0o111 != 0 {
		mode = codec.ModeExecutable
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(abs)
		if err != nil {
			return diskEntry{}, false, err
		}
		return diskEntry{kind: codec.EntrySymlink, mode: mode, data: []byte(target)}, true, nil
	case fi.IsDir():
		return diskEntry{kind: codec.EntryTree, mode: mode}, true, nil
	case fi.Mode().IsRegular():
		data, err := os.ReadFile(abs)
		if err != nil {
			return diskEntry{}, false, err
		}
		return diskEntry{kind: codec.EntryFile, mode: mode, data: data}, true, nil
	default:
		return diskEntry{}, false, fmt.Errorf("workspace: 不支持的条目类型: %s", abs)
	}
}
