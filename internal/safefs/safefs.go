// Package safefs 提供「不穿过符号链接」的文件系统写入原语。
//
// 为什么需要它：把版本树物化或覆写到工作区时，工作区里可能**已经存在**由
// agent 创建的符号链接。若直接 os.WriteFile / os.MkdirAll：
//
//	工作区里有  sub -> /home/u/.ssh
//	目标树里有  目录 sub
//	→ MkdirAll 跟随链接成功返回，随后把文件写进 /home/u/.ssh
//
// 这是真实的越权写（agent 是半可信方）。本包的所有写入都先 Lstat 目标，
// 若类型与预期不符就先删除，绝不跟随。
package safefs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrBadRel 表示传入的相对路径非法。
var ErrBadRel = errors.New("safefs: 非法相对路径")

// splitRel 校验并切分仓库相对路径。
func splitRel(rel string) ([]string, error) {
	if rel == "" {
		return nil, ErrBadRel
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") {
		return nil, fmt.Errorf("%w: %q 不允许绝对路径", ErrBadRel, rel)
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for _, p := range parts {
		switch p {
		case "", ".", "..":
			return nil, fmt.Errorf("%w: %q 含非法路径段", ErrBadRel, rel)
		}
	}
	return parts, nil
}

// ensureParents 逐级确保 rel 的父目录都是**真实目录**。
//
// 若某一级已存在但不是目录（符号链接或常规文件），**删除并重建为目录** ——
// 目标树的语义就是「这里应该是目录」，因此替换是正确的。返回最终路径。
func ensureParents(root, rel string) (string, error) {
	parts, err := splitRel(rel)
	if err != nil {
		return "", err
	}
	cur := root
	for _, p := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, p)
		fi, err := os.Lstat(cur)
		switch {
		case err == nil && fi.IsDir():
			// 已是真实目录。
		case err == nil:
			// 符号链接或文件：替换为真实目录。
			if err := os.RemoveAll(cur); err != nil {
				return "", err
			}
			if err := os.Mkdir(cur, 0o755); err != nil {
				return "", err
			}
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(cur, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
				return "", err
			}
		default:
			return "", err
		}
	}
	return filepath.Join(cur, parts[len(parts)-1]), nil
}

// EnsureDirUnder 在 root 之下确保 rel 是真实目录。
func EnsureDirUnder(root, rel string) error {
	p, err := ensureParents(root, rel)
	if err != nil {
		return err
	}
	fi, err := os.Lstat(p)
	switch {
	case err == nil && fi.IsDir():
		return nil
	case err == nil:
		if err := os.RemoveAll(p); err != nil {
			return err
		}
		return os.Mkdir(p, 0o755)
	case errors.Is(err, os.ErrNotExist):
		if err := os.Mkdir(p, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		return nil
	default:
		return err
	}
}

// WriteFileUnder 在 root 之下安全地写入一个常规文件。
//
// 若目标已存在且不是常规文件（符号链接、目录），先删除。
func WriteFileUnder(root, rel string, data []byte, mode os.FileMode) error {
	p, err := ensureParents(root, rel)
	if err != nil {
		return err
	}
	if fi, err := os.Lstat(p); err == nil && !fi.Mode().IsRegular() {
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return os.WriteFile(p, data, mode)
}

// SymlinkUnder 在 root 之下安全地创建一个符号链接（目标内容原样写入）。
//
// 与其它原语不同：这里**必须**先移除任何既有条目，否则 os.Symlink 会失败，
// 更重要的是不能让调用方误以为链接已更新。
func SymlinkUnder(root, rel, target string) error {
	p, err := ensureParents(root, rel)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(p); err != nil {
		return err
	}
	return os.Symlink(target, p)
}

// RemoveUnder 删除 root 之下的 rel（符号链接只删链接本身）。
func RemoveUnder(root, rel string) error {
	p, err := ensureParents(root, rel)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// HardenDir 把目录权限收紧为 0700（仓库元数据与工作区只允许属主访问）。
func HardenDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

// HardenFile 把文件权限收紧为 0600。
func HardenFile(path string) error {
	if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
