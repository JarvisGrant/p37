//go:build linux

package mountmgr

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/chenyiwen/p37/internal/hash"
)

// mountOverlay 以 baseTree 为 lowerdir 挂载工作区（§4.6）。
func (m *Manager) mountOverlay(wsID string, baseTree hash.ID, src ContentSource) error {
	base, err := m.ensureBase(baseTree, src)
	if err != nil {
		return err
	}

	upper := m.upperDir(wsID)
	work := m.workDir(wsID)
	dir := m.WorkspaceDir(wsID)
	for _, d := range []string{upper, work, dir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	// lower 一律单层（D23）。
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", base, upper, work)
	if err := syscall.Mount("overlay", dir, "overlay", 0, opts); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	// 记录引用，供 Close 时释放。
	baseRefs.Store(wsID, baseTree)
	return nil
}

// unmountOverlay 卸载工作区（§4.6、§4.9.2）。
func (m *Manager) unmountOverlay(wsID string) error {
	dir := m.WorkspaceDir(wsID)
	if err := syscall.Unmount(dir, 0); err != nil {
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOENT) {
			return nil // 未挂载
		}
		return fmt.Errorf("mountmgr: 卸载 %s: %w", dir, err)
	}
	return nil
}

// IsMounted 报告路径当前是否为挂载点。
func (m *Manager) IsMounted(wsID string) bool {
	dir := m.WorkspaceDir(wsID)
	var st, parent syscall.Stat_t
	if err := syscall.Stat(dir, &st); err != nil {
		return false
	}
	if err := syscall.Stat(filepath.Dir(dir), &parent); err != nil {
		return false
	}
	// 挂载点的设备号与父目录不同，且 inode 为 1。
	return st.Dev != parent.Dev || st.Ino == 1
}

// Remount 以新的 baseTree 重挂（§4.9.2 步骤 5b）。
//
// 实现策略：把 upper 中相对旧 lower 的内容先在临时目录准备好，再原子替换并
// 重挂。这里采用 simpler 的两步：卸载 -> 替换 upper -> 重挂。
func (m *Manager) Remount(wsID string, newBase hash.ID, src ContentSource,
	prepareUpper func(tmpUpper string) error) error {
	if err := m.unmountOverlay(wsID); err != nil {
		return err
	}

	upper := m.upperDir(wsID)
	work := m.workDir(wsID)
	_ = os.RemoveAll(work)

	tmpUpper := upper + ".new"
	_ = os.RemoveAll(tmpUpper)
	if err := os.MkdirAll(tmpUpper, 0o755); err != nil {
		return err
	}
	if prepareUpper != nil {
		if err := prepareUpper(tmpUpper); err != nil {
			_ = os.RemoveAll(tmpUpper)
			return err
		}
	}
	old := upper + ".old"
	_ = os.RemoveAll(old)
	if err := os.Rename(upper, old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(tmpUpper, upper); err != nil {
		return err
	}
	_ = os.RemoveAll(old)

	if err := m.mountOverlay(wsID, newBase, src); err != nil {
		return err
	}
	return nil
}

// ProbeOverlay 尝试在 dir 下做一次真实的 OverlayFS 挂载，用于部署自检
// （附录 G.4）。成功即返回 nil 并清理现场。
func (m *Manager) ProbeOverlay(dir string) error {
	lower := filepath.Join(dir, "lower")
	upper := filepath.Join(dir, "upper")
	work := filepath.Join(dir, "work")
	target := filepath.Join(dir, "target")
	for _, d := range []string{lower, upper, work, target} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upper, work)
	if err := syscall.Mount("overlay", target, "overlay", 0, opts); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	err := syscall.Unmount(target, 0)
	if err != nil {
		return fmt.Errorf("mountmgr: 自检挂载成功但卸载失败: %w", err)
	}
	return nil
}
