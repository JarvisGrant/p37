package repo_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/workspace"
)

// TestExecutableBitSurvivesOps 回归 S30：权限位（可执行位）必须能经过
// create / modify / attr / blob_put 等 op 重放而不丢失。
//
// 早先 op 不承载权限位，replay 一律按 ModeNormal 落条目 —— 新建的可执行
// 文件、以及改过内容或属性的可执行文件，落地后都会退化成 0644。
func TestExecutableBitSurvivesOps(t *testing.T) {
	e := newEnv(t)

	// 源：一个可执行脚本、一个普通文本、一个稍后要改权限的文本。
	seedExec(t, e.src, "tool.sh", "#!/bin/sh\necho one\n")
	e.seed("a.txt", "l0\nl1\nl2\n")
	e.seed("plain.txt", "plain\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}

	w := e.open("", "改可执行位")
	dir := e.r.WS.Dir(w.ID)

	// 1) 改可执行文件的内容（modify op，权限位不变）。
	writeFileMode(t, filepath.Join(dir, "tool.sh"), "#!/bin/sh\necho two\n", 0o755)
	// 2) 新建一个可执行文件（create op）。
	writeFileMode(t, filepath.Join(dir, "new.sh"), "#!/bin/sh\necho new\n", 0o700)
	// 3) 只改普通文件的权限位（attr op，内容不变）。
	if err := os.Chmod(filepath.Join(dir, "a.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 4) 普通文件内容改动（modify op）后仍是普通文件。
	e.write(w.ID, "plain.txt", "plain changed\n")

	if err := e.r.WS.Sync(w.ID); err != nil {
		t.Fatal(err)
	}

	// op 应携带权限位：create/modify/attr 各一。
	ops, err := e.r.DB.ListOps(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range ops {
		if o.Kind == "attr" && o.Path == "a.txt" {
			if o.ModeAfter == nil || *o.ModeAfter != codec.ModeExecutable {
				t.Fatalf("attr op 未携带可执行位: %+v", o)
			}
		}
	}

	res, err := e.r.Land(w.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 校验落地后的树：权限位必须正确。
	w2, err := e.r.WS.Open(workspace.OpenRequest{TaskDesc: "校验"})
	if err != nil {
		t.Fatal(err)
	}
	dir2 := e.r.WS.Dir(w2.ID)
	checkMode(t, filepath.Join(dir2, "tool.sh"), true, "改过内容的可执行文件")
	checkMode(t, filepath.Join(dir2, "new.sh"), true, "新建的可执行文件")
	checkMode(t, filepath.Join(dir2, "a.txt"), true, "只改权限位的文件")
	checkMode(t, filepath.Join(dir2, "plain.txt"), false, "普通文件")

	v, _ := e.r.DB.GetVersion(res.VersionID)
	tr, _ := e.r.Store.GetTree(v.Tree)
	for _, name := range []string{"tool.sh", "new.sh", "a.txt"} {
		fc, ok := tr.Get(name)
		if !ok {
			t.Fatalf("版本树缺少 %s", name)
		}
		if fc.Mode != codec.ModeExecutable {
			t.Fatalf("版本树中 %s 的权限位 = %d，期望可执行", name, fc.Mode)
		}
	}
	if fc, ok := tr.Get("plain.txt"); ok && fc.Mode != codec.ModeNormal {
		t.Fatalf("plain.txt 不应是可执行")
	}
}

// TestExecutableBitRevert 回归：revert 一个权限位变化应还原旧权限位。
func TestExecutableBitRevert(t *testing.T) {
	e := newEnv(t)
	e.seed("x.txt", "body\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}

	// 第一版：把 x.txt 变成可执行。
	w := e.open("", "变可执行")
	if err := os.Chmod(filepath.Join(e.r.WS.Dir(w.ID), "x.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.r.WS.Sync(w.ID); err != nil {
		t.Fatal(err)
	}
	res, err := e.r.Land(w.ID)
	if err != nil {
		t.Fatal(err)
	}

	// revert 它：权限位应回到普通文件。
	if _, err := e.r.Revert(res.VersionID); err != nil {
		t.Fatal(err)
	}
	head, _, _ := e.r.Head()
	v, _ := e.r.DB.GetVersion(head)
	tr, _ := e.r.Store.GetTree(v.Tree)
	fc, ok := tr.Get("x.txt")
	if !ok {
		t.Fatal("树缺少 x.txt")
	}
	if fc.Mode != codec.ModeNormal {
		t.Fatalf("revert 后权限位 = %d，期望普通文件", fc.Mode)
	}
}

func seedExec(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFileMode(t, p, content, 0o755)
}

func writeFileMode(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func checkMode(t *testing.T, p string, wantExec bool, label string) {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	gotExec := info.Mode()&0o111 != 0
	if gotExec != wantExec {
		t.Fatalf("%s: 可执行位 = %v，期望 %v（mode=%v）", label, gotExec, wantExec, info.Mode())
	}
}
