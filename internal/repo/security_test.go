package repo_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/workspace"
)

// TestRebaseDoesNotEscapeViaSymlink 是 A2 的端到端回归。
//
// 场景：工作区里放一个指向仓库外部的符号链接，然后触发 ws_rebase
// （它会写 upper/工作区）。任何写入都**不得**穿过该链接。
func TestRebaseDoesNotEscapeViaSymlink(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "l0\nl1\nl2\n")
	e.seed("sub/inner.txt", "inner\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}

	// 仓库外部的一个「受害文件」。
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("ORIGINAL"), 0o600); err != nil {
		t.Fatal(err)
	}

	w := e.open("", "任务")
	wsDir := e.r.WS.Dir(w.ID)

	// 在工作区里放入恶意符号链接：把 a.txt 换成指向受害文件的链接。
	if err := os.Remove(filepath.Join(wsDir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(wsDir, "a.txt")); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}
	// 再放一个「目录名被链接占位」的情形。
	if err := os.RemoveAll(filepath.Join(wsDir, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(wsDir, "sub")); err != nil {
		t.Fatal(err)
	}

	// 触发 rebase（强制它重放并覆写工作区）。
	if _, err := e.r.WS.Rebase(w.ID); err != nil {
		t.Fatalf("rebase 失败: %v", err)
	}

	// 受害文件必须原封不动。
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ORIGINAL" {
		t.Fatalf("越权写！受害文件被改为 %q", got)
	}

	// 外部目录里不得出现仓库文件。
	des, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range des {
		if d.Name() != "victim.txt" {
			t.Fatalf("越权写入外部目录: %s", d.Name())
		}
	}

	// 工作区目录本身必须仍然存在且可枚举。
	if _, err := os.ReadDir(wsDir); err != nil {
		t.Fatalf("工作区目录不可读: %v", err)
	}
}

// TestRebaseDeletesNestedPaths 是 B1 的回归：
// 旧基准里存在、新基准里不存在的**嵌套**文件必须被删除。
func TestRebaseDeletesNestedPaths(t *testing.T) {
	e := newEnv(t)
	e.seed("src/keep.go", "keep\n")
	e.seed("src/gone.go", "gone\n")
	e.seed("deep/a/b/gone2.txt", "gone2\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	baseHead, _, _ := e.r.Head()

	// 从基线开一个工作区，删掉嵌套文件后落地 → 新版本不再含这些路径。
	w := e.open(baseHead.String(), "清理嵌套文件")
	wsDir := e.r.WS.Dir(w.ID)
	if err := os.Remove(filepath.Join(wsDir, "src", "gone.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(wsDir, "deep", "a", "b", "gone2.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.Land(w.ID); err != nil {
		t.Fatal(err)
	}
	head, _, _ := e.r.Head()

	// 再从基线开一个工作区并 rebase 到新 head：
	// 旧基准里的嵌套文件应当从磁盘上消失。
	w2 := e.open(baseHead.String(), "重挂到新基准")
	wsDir2 := e.r.WS.Dir(w2.ID)
	// 先确认它确实有这些文件（基线里有）。
	if _, err := os.Stat(filepath.Join(wsDir2, "deep", "a", "b", "gone2.txt")); err != nil {
		t.Fatalf("基线应含 deep/a/b/gone2.txt: %v", err)
	}

	if err := e.r.DB.UpdateWorkspaceParent(w2.ID, meta.ParentVersion, head.String(), 0); err != nil {
		t.Fatal(err)
	}
	res, err := e.r.WS.Rebase(w2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Rebased {
		t.Fatalf("rebase 未生效: %+v", res)
	}

	for _, p := range []string{"src/gone.go", "deep/a/b/gone2.txt"} {
		if _, err := os.Stat(filepath.Join(wsDir2, filepath.FromSlash(p))); !os.IsNotExist(err) {
			t.Fatalf("%s 应已被删除（err=%v）", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(wsDir2, "src", "keep.go")); err != nil {
		t.Fatalf("keep.go 不应被删: %v", err)
	}
}

// TestWSIDValidation 覆盖潜在路径穿越的兜底校验。
func TestWSIDValidation(t *testing.T) {
	e := newEnv(t)

	// 非法 ws_id 必须被拒绝，且不得触碰仓库外的路径。
	for _, bad := range []string{
		"../../etc",
		"/etc/passwd",
		"ws_short",
		"ws_XXXXXXXXXXXXXXXX",
		"ws_" + "0123456789abcdef" + "/../x",
	} {
		if _, err := e.r.WS.Info(bad); err == nil {
			t.Errorf("ws_id %q 应被拒绝", bad)
		}
	}

	// WorkspaceDir 对非法 id 必须落在仓库内、且落在不可能存在的哨兵位置。
	dir := e.r.WS.Dir("../../etc")
	rel, err := filepath.Rel(e.root, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("WorkspaceDir 逃逸出仓库: %s", dir)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatalf("哨兵路径不应存在: %s", dir)
	}
	if !strings.Contains(dir, "invalid-ws-id") {
		t.Fatalf("未走哨兵路径: %s", dir)
	}

	// ValidWSID 是路径拼接前的唯一把关点，必须拒绝一切非生成格式。
	for _, bad := range []string{"", "ws_", "ws_0123456789abcdef0", "../../etc"} {
		if mountmgr.ValidWSID(bad) {
			t.Errorf("ValidWSID(%q) 应为 false", bad)
		}
	}
	if !mountmgr.ValidWSID("ws_0123456789abcdef") {
		t.Error("合法 wsID 被拒绝")
	}
}

// TestRepoPermissionsHardened 覆盖 A3。
func TestRepoPermissionsHardened(t *testing.T) {
	e := newEnv(t)
	paths := []string{
		filepath.Join(e.root, ".p37"),
		filepath.Join(e.root, ".p37", "p37.db"),
		filepath.Join(e.root, "workspaces"),
	}
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		perm := fi.Mode().Perm()
		if perm&0o077 != 0 {
			t.Errorf("%s 权限 = %o，其它用户可访问（期望不含 group/other 位）", p, perm)
		}
	}
}

// TestDropKeepsChildrenReparented 顺带覆盖 drop 的级联语义。
func TestDropKeepsChildrenReparented(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "x\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	head, _, _ := e.r.Head()

	parent := e.open(head.String(), "父")
	child, err := e.r.WS.Open(workspace.OpenRequest{
		Parent: parent.ID, TaskDesc: "子"})
	if err != nil {
		t.Fatal(err)
	}

	dropped, reparented, err := e.r.WS.Drop(parent.ID, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || dropped[0] != parent.ID {
		t.Fatalf("dropped = %v", dropped)
	}
	if len(reparented) != 1 || reparented[0] != child.ID {
		t.Fatalf("reparented = %v", reparented)
	}
	got, err := e.r.WS.Get(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ParentKind != meta.ParentVersion || got.ParentID != head.String() {
		t.Fatalf("子未重挂到祖父: %s:%s", got.ParentKind, got.ParentID)
	}
}
