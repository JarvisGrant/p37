//go:build linux

package mountmgr_test

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/chenyiwen/p37/internal/mountmgr"
)

// 需要 root + CAP_SYS_ADMIN 的真机 bind mount 测试；CI 非 root 自动跳过。
func TestIsMountPointBindMount(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("需要 root 才能做 bind mount")
	}
	src := t.TempDir()
	dst := t.TempDir()
	if err := unix.Mount(src, dst, "", unix.MS_BIND, ""); err != nil {
		t.Skipf("bind mount 不可用: %v", err)
	}
	defer func() { _ = unix.Unmount(dst, 0) }()

	if !mountmgr.IsMountPoint(dst) {
		t.Fatalf("bind mount 目标应识别为挂载点: %s", dst)
	}
}
