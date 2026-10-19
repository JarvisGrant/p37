//go:build !linux

package mountmgr

import (
	"github.com/chenyiwen/p37/internal/hash"
)

// mountOverlay 在非 Linux 平台不可用（macOS 开发机走 copy 模式，§15.3）。
func (m *Manager) mountOverlay(wsID string, baseTree hash.ID, src ContentSource) error {
	return ErrUnavailable
}

// unmountOverlay 非 Linux 平台无挂载可卸。
func (m *Manager) unmountOverlay(wsID string) error { return nil }

// IsMounted 非 Linux 平台恒为 false（copy 模式无挂载概念）。
func (m *Manager) IsMounted(wsID string) bool { return false }

// Remount 非 Linux 平台不可用；copy 模式没有挂载需要恢复。
func (m *Manager) Remount(wsID string, baseTree hash.ID, src ContentSource) error {
	return nil
}

// ProbeOverlay 非 Linux 平台恒为不可用。
func (m *Manager) ProbeOverlay(dir string) error { return ErrUnavailable }
