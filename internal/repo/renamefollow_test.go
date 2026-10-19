package repo_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyiwen/p37/internal/workspace"
)

// TestShortVersionPrefixResolves 回归 S31：短版本 id（p37 log 打印的 16 位）
// 必须能回喂给 ws-open --parent / （CLI show/revert/blame 走同一条解析路径）。
func TestShortVersionPrefixResolves(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "v0\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	head, _, _ := e.r.Head()
	short := head.String()[:16]

	got, err := e.r.DB.FindVersionByPrefix(short)
	if err != nil {
		t.Fatalf("前缀解析失败: %v", err)
	}
	if got != head {
		t.Fatalf("前缀解析 = %s，期望 %s", got, head)
	}

	// 打开工作区时用短前缀作 parent。
	w, err := e.r.WS.Open(workspace.OpenRequest{Parent: short, TaskDesc: "短前缀"})
	if err != nil {
		t.Fatalf("用短前缀打开工作区失败: %v", err)
	}
	if w.BaseVersion != head {
		t.Fatalf("基准版本 = %s，期望 %s", w.BaseVersion, head)
	}

	// 歧义前缀必须报错，不允许猜。
	if _, err := e.r.DB.FindVersionByPrefix(""); err == nil {
		t.Fatal("空前缀应报错")
	}
}

// TestRenameFollowAcrossLand 回归：对侧已把文件重命名并落地后，本侧基于旧
// 路径的改动应跟随到新路径（§6.3 rename_vs_edit），而不是报 missing_base。
func TestRenameFollowAcrossLand(t *testing.T) {
	e := newEnv(t)
	e.seed("old.txt", "v1\nline\n")
	e.seed("other.txt", "keep\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}

	w1 := e.open("", "重命名 old.txt")
	w2 := e.open("", "改 old.txt")

	// W1：重命名。W2：改内容。
	if err := os.Rename(filepath.Join(e.r.WS.Dir(w1.ID), "old.txt"),
		filepath.Join(e.r.WS.Dir(w1.ID), "new.txt")); err != nil {
		t.Fatal(err)
	}
	e.write(w2.ID, "old.txt", "v1\nline\nmore\n")

	if err := e.r.WS.Sync(w1.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.r.WS.Sync(w2.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := e.r.Land(w1.ID); err != nil {
		t.Fatal(err)
	}
	// 此时 head 上 old.txt 已改名为 new.txt；W2 的改动应跟随。
	res, err := e.r.Land(w2.ID)
	if err != nil {
		t.Fatalf("rename 跟随落地失败: %v", err)
	}
	if res.VersionID.IsZero() {
		t.Fatal("未产生版本")
	}

	w3, err := e.r.WS.Open(workspace.OpenRequest{TaskDesc: "校验"})
	if err != nil {
		t.Fatal(err)
	}
	dir := e.r.WS.Dir(w3.ID)
	if _, err := os.Stat(filepath.Join(dir, "old.txt")); !os.IsNotExist(err) {
		t.Fatal("old.txt 应不存在")
	}
	b, err := os.ReadFile(filepath.Join(dir, "new.txt"))
	if err != nil {
		t.Fatalf("new.txt 缺失: %v", err)
	}
	if string(b) != "v1\nline\nmore\n" {
		t.Fatalf("跟随后的内容 = %q", b)
	}
}
