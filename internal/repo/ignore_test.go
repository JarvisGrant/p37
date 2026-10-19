package repo_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyiwen/p37/internal/repo"
	"github.com/chenyiwen/p37/internal/tree"
	"github.com/chenyiwen/p37/internal/workspace"
)

// TestP37IgnoreFileIsApplied 回归：仓库根的 .p37ignore 必须真正生效。
func TestP37IgnoreFileIsApplied(t *testing.T) {
	e := newEnv(t)
	e.seed("keep.txt", "keep\n")
	e.seed("junk.log", "log\n")
	e.seed("sub/nested.log", "log\n")

	// 忽略文件位于仓库容器根（与 .p37 同级）。
	if err := os.WriteFile(filepath.Join(e.root, tree.IgnoreFileName),
		[]byte("*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 忽略规则在打开仓库时载入一次；这里重新打开以生效（与 CLI/daemon 一致）。
	if err := e.r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := repo.Open(e.root, repo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	e.r = r2
	t.Cleanup(func() { _ = r2.Close() })

	vid, err := e.r.Import(e.src)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := e.r.DB.GetVersion(vid)
	tr, _ := e.r.Store.GetTree(v.Tree)
	if _, ok := tr.Get("junk.log"); ok {
		t.Fatal(".p37ignore 未生效：junk.log 被跟踪")
	}
	if _, ok := tr.Get("keep.txt"); !ok {
		t.Fatal("keep.txt 应被跟踪")
	}

	// 工作区里也不应出现被忽略的文件。
	w, err := e.r.WS.Open(workspace.OpenRequest{TaskDesc: "忽略校验"})
	if err != nil {
		t.Fatal(err)
	}
	dir := e.r.WS.Dir(w.ID)
	if _, err := os.Stat(filepath.Join(dir, "junk.log")); !os.IsNotExist(err) {
		t.Fatal("工作区物化了被忽略的文件")
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "nested.log")); !os.IsNotExist(err) {
		t.Fatal("工作区物化了被忽略的嵌套文件")
	}
}
