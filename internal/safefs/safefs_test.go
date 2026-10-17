package safefs

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteFileUnderReplacesSymlink 是 A2 的核心回归：
// 目标已存在为指向外部的符号链接时，写入**不得**穿过链接。
func TestWriteFileUnderReplacesSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "authorized_keys")
	if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 工作区里放一个指向外部的符号链接。
	if err := os.Symlink(victim, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}

	if err := WriteFileUnder(root, "escape", []byte("PWNED"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 外部文件必须原封不动。
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Fatalf("写穿了符号链接！外部文件被改为 %q", got)
	}

	// 工作区里的条目必须变成常规文件，且内容正确。
	fi, err := os.Lstat(filepath.Join(root, "escape"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("工作区内条目类型 = %v，期望常规文件", fi.Mode())
	}
	in, _ := os.ReadFile(filepath.Join(root, "escape"))
	if string(in) != "PWNED" {
		t.Fatalf("工作区内内容 = %q", in)
	}
}

// TestEnsureDirUnderReplacesSymlinkedDir 覆盖 A2 的目录分支：
// 目标树说「这里是目录」，而工作区里是符号链接 → 必须替换而不是跟随。
func TestEnsureDirUnderReplacesSymlinkedDir(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	if err := os.Symlink(outside, filepath.Join(root, "sub")); err != nil {
		t.Fatal(err)
	}

	if err := EnsureDirUnder(root, "sub"); err != nil {
		t.Fatal(err)
	}
	// 后续写入必须落在工作区内。
	if err := WriteFileUnder(root, "sub/payload.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 外部目录必须仍然为空。
	des, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(des) != 0 {
		t.Fatalf("写穿到外部目录：%v", des)
	}

	fi, err := os.Lstat(filepath.Join(root, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("sub 应为真实目录，实为 %v", fi.Mode())
	}
}

// TestNestedSymlinkInMiddlePath 覆盖「中间路径段是符号链接」。
func TestNestedSymlinkInMiddlePath(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	if err := os.Symlink(outside, filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileUnder(root, "a/b/c.txt", []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	des, _ := os.ReadDir(outside)
	if len(des) != 0 {
		t.Fatalf("中间路径段写穿：%v", des)
	}
	got, err := os.ReadFile(filepath.Join(root, "a", "b", "c.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "data" {
		t.Fatalf("内容 = %q", got)
	}
}

// TestSymlinkUnderReplacesExisting 断言符号链接原语会替换既有条目。
func TestSymlinkUnderReplacesExisting(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "l"), []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SymlinkUnder(root, "l", "target"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(root, "l"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("未替换为符号链接")
	}
	if got, _ := os.Readlink(filepath.Join(root, "l")); got != "target" {
		t.Fatalf("链接目标 = %q", got)
	}
}

// TestRemoveUnderDoesNotFollowSymlink 断言删除符号链接不会删掉其目标。
func TestRemoveUnderDoesNotFollowSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "l")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveUnder(root, "l"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("外部目标被删掉了: %v", err)
	}
}

// TestRejectsBadRel 覆盖相对路径校验。
func TestRejectsBadRel(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"", "/abs", "../escape", "a/../../b", "a//b", "a/./b", "."} {
		if err := WriteFileUnder(root, rel, []byte("x"), 0o644); err == nil {
			t.Errorf("相对路径 %q 应被拒绝", rel)
		}
	}
}

// TestHardenDir 断言目录权限被收紧为 0700。
func TestHardenDir(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "meta")
	if err := HardenDir(p); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("权限 = %v，期望 0700", fi.Mode().Perm())
	}
}
