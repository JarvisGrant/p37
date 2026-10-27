package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestExecutableFromReadOnlyDir 是“从只读挂载点执行”的回归代理：
// 把 p37 编到一个 0555 目录里再执行，确认二进制不依赖可写的自身目录。
func TestExecutableFromReadOnlyDir(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	if runtime.GOOS == "windows" {
		t.Skip("windows")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	bin := filepath.Join(binDir, "p37")
	build := exec.Command("go", "build", "-trimpath", "-o", bin, "./cmd/p37")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建失败: %v\n%s", err, out)
	}

	if err := os.Chmod(binDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(binDir, 0o755) })

	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("从只读目录执行 --version 失败: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "version=") {
		t.Fatalf("版本输出异常: %s", out)
	}

	repoDir := t.TempDir()
	if out, err := exec.Command(bin, "init", repoDir, "--allow-nonpersistent").CombinedOutput(); err != nil {
		t.Fatalf("从只读目录执行 init 失败: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(repoDir, ".p37", "p37.db")); err != nil {
		t.Fatalf("init 没有在仓库内落库: %v", err)
	}
}
