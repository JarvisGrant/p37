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
	return m.mountWithLower(wsID, baseTree, base)
}

// mountWithLower 用一个已物化的基底目录挂载工作区。
//
// baseTree 是基底树的 id（记进 baseRefs 供释放引用计数用），base 是它的目录。
func (m *Manager) mountWithLower(wsID string, baseTree hash.ID, base string) error {
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
	baseRefs.Store(wsID, baseTree)
	return nil
}

// unmountOverlay 卸载工作区（§4.6、§4.9.2）。
func (m *Manager) unmountOverlay(wsID string) error {
	// 先确认它真的是挂载点：copy 模式/已卸载的目录直接跳过。
	// 否则在受限环境（无 CAP_SYS_ADMIN、seccomp、容器 runner）里
	// umount 会返回 EPERM，被误判成「清理失败」，而其实无事可做。
	if !m.IsMounted(wsID) {
		return nil
	}
	dir := m.WorkspaceDir(wsID)
	if err := syscall.Unmount(dir, 0); err != nil {
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOENT) {
			return nil // 未挂载
		}
		return fmt.Errorf("mountmgr: 卸载 %s: %w", dir, err)
	}
	return nil
}

// Remount 恢复一个工作区的挂载（daemon 启动时用，§7.7 步骤 3）。
//
// 只重建挂载：基底按 base_tree 复用/物化，**upper 层原样保留**，因此工作区
// 内容不会丢。
func (m *Manager) Remount(wsID string, baseTree hash.ID, src ContentSource) error {
	if m.IsMounted(wsID) {
		return nil
	}
	base, err := m.ensureBase(baseTree, src)
	if err != nil {
		return err
	}
	return m.mountWithLower(wsID, baseTree, base)
}

// Relower 把工作区的 lowerdir 换成 newBase，**upper 层原样保留**（§4.9.2）。
//
// 与「整树覆写」的区别：换 lower 之后，工作区里那些「与旧基准相同、与新基准
// 也相同」的文件根本不需要落进 upper —— 它们直接从新 lower 读。只有真正改过
// 的文件才在 upper 里。64 并发 × 10 万文件时这是数量级的差别。
//
// 安全性：只卸载/重挂，不动 upper 里的任何字节。
func (m *Manager) Relower(wsID string, newBase hash.ID, src ContentSource) error {
	if err := m.unmountOverlay(wsID); err != nil {
		return err
	}
	base, err := m.ensureBase(newBase, src)
	if err != nil {
		return err
	}
	// overlayfs 的 workdir 必须与本次挂载匹配；换 lower 时清掉重建最稳。
	work := m.workDir(wsID)
	if err := os.RemoveAll(work); err != nil {
		return err
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		return err
	}

	dir := m.WorkspaceDir(wsID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", base, m.upperDir(wsID), work)
	if err := syscall.Mount("overlay", dir, "overlay", 0, opts); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	// 释放旧基底的引用，改记新的。
	m.releaseBase(wsID)
	baseRefs.Store(wsID, newBase)
	return nil
}

// IsMounted 报告工作区目录当前是否为挂载点。
//
// 通过 /proc/self/mountinfo 判断，比比较 st_dev 可靠（bind mount、tmpfs 等
// 场景下 st_dev 会误导）。
func (m *Manager) IsMounted(wsID string) bool {
	return IsMountPoint(m.WorkspaceDir(wsID))
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
	if err := syscall.Unmount(target, 0); err != nil {
		return fmt.Errorf("mountmgr: 自检挂载成功但卸载失败: %w", err)
	}
	return nil
}
