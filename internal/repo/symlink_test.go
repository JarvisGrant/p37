package repo_test

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSymlinkRoundTrip 覆盖符号链接的完整生命周期：
//
//	工作区里建链接 → 采集 → 落地 → 从新版本开工作区 → 链接仍是链接、目标不变
//
// 这同时回归两个问题：
//   - op 层丢失条目类型（链接退化成常规文件）
//   - 物化符号链接时误走 ReadFileData（链接条目存的是 Blob）
func TestSymlinkRoundTrip(t *testing.T) {
	e := newEnv(t)
	e.seed("real.txt", "real content\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}

	w := e.open("", "加一个符号链接")
	wsDir := e.r.WS.Dir(w.ID)

	if err := os.Symlink("real.txt", filepath.Join(wsDir, "link.txt")); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(wsDir, "abs.link")); err != nil {
		t.Fatal(err)
	}

	res, err := e.r.Land(w.ID)
	if err != nil {
		t.Fatalf("land 失败: %v", err)
	}
	v, err := e.r.DB.GetVersion(res.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := e.r.Store.GetTree(v.Tree)
	if err != nil {
		t.Fatal(err)
	}

	// 落地后的树里，两个条目都必须仍是符号链接。
	for _, name := range []string{"link.txt", "abs.link"} {
		en, ok := tr.Get(name)
		if !ok {
			t.Fatalf("树中缺少 %s", name)
		}
		if en.Kind != 2 { // codec.EntrySymlink
			t.Fatalf("%s 的条目类型 = %d，期望 2（符号链接）", name, en.Kind)
		}
	}

	// 从新版本开一个工作区：物化必须成功，且链接仍是链接。
	w2 := e.open(res.VersionID.String(), "验证物化")
	dir2 := e.r.WS.Dir(w2.ID)

	fi, err := os.Lstat(filepath.Join(dir2, "link.txt"))
	if err != nil {
		t.Fatalf("link.txt 未物化: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link.txt 退化为 %v，期望符号链接", fi.Mode())
	}
	if got, _ := os.Readlink(filepath.Join(dir2, "link.txt")); got != "real.txt" {
		t.Fatalf("link.txt 目标 = %q", got)
	}
	if got, _ := os.Readlink(filepath.Join(dir2, "abs.link")); got != "/etc/hostname" {
		t.Fatalf("abs.link 目标 = %q", got)
	}
}

// TestSymlinkToFileTransition 覆盖「符号链接 ↔ 常规文件」的类型变化。
func TestSymlinkToFileTransition(t *testing.T) {
	e := newEnv(t)
	e.seed("f.txt", "original\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}

	// 先把 f.txt 换成符号链接并落地。
	w := e.open("", "换成链接")
	dir := e.r.WS.Dir(w.ID)
	if err := os.Remove(filepath.Join(dir, "f.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(dir, "f.txt")); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}
	res, err := e.r.Land(w.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 再从新版本开工作区，把 f.txt 换回常规文件并落地。
	w2 := e.open(res.VersionID.String(), "换回文件")
	dir2 := e.r.WS.Dir(w2.ID)
	p := filepath.Join(dir2, "f.txt")
	if fi, err := os.Lstat(p); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("前置条件不成立：f.txt 应为符号链接（%v, %v）", fi, err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("now a file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res2, err := e.r.Land(w2.ID)
	if err != nil {
		t.Fatal(err)
	}

	v, _ := e.r.DB.GetVersion(res2.VersionID)
	tr, _ := e.r.Store.GetTree(v.Tree)
	en, ok := tr.Get("f.txt")
	if !ok {
		t.Fatal("f.txt 应存在")
	}
	if en.Kind != 0 { // codec.EntryFile
		t.Fatalf("f.txt 条目类型 = %d，期望 0（常规文件）", en.Kind)
	}
	data, err := e.r.Store.ReadFileData(en.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "now a file\n" {
		t.Fatalf("内容 = %q", data)
	}
}
