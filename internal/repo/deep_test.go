package repo_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/tree"
	"github.com/chenyiwen/p37/internal/workspace"
)

// TestLandPreservesNestedFiles 是线上发现的严重 bug 的回归测试。
//
// 症状：仓库里只要有**两级以上**嵌套目录（如 pkg/mod5/f5.go），land 之后
// 未被触碰的文件会全部消失 —— 因为 op.unflatten 先收集目录键、再在循环里
// 创建父目录，中间层目录（pkg）不在初始键集合里，永远不会被构建，根树变空。
//
// 关键点是**深度**，不是文件数：之前所有测试的目录都只有一层（src/x.go）。
func TestLandPreservesNestedFiles(t *testing.T) {
	e := newEnv(t)

	// 构造 3 层嵌套：a/b/c/f*.txt
	const (
		lvl1  = 3
		lvl2  = 2
		files = 4
	)
	want := 0
	for i := 0; i < lvl1; i++ {
		for j := 0; j < lvl2; j++ {
			for k := 0; k < files; k++ {
				e.seed(fmt.Sprintf("a%d/b%d/c%d/f%d.txt", i, j, k, k),
					fmt.Sprintf("content %d-%d-%d\n", i, j, k))
				want++
			}
		}
	}
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	head, _, _ := e.r.Head()
	hv, _ := e.r.DB.GetVersion(head)
	if got := countTreeFiles(t, e, hv.Tree); got != want {
		t.Fatalf("导入树文件数 = %d，期望 %d", got, want)
	}

	// 只改一个深层文件
	w, err := e.r.WS.Open(workspace.OpenRequest{TaskDesc: "只改一个深层文件"})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(e.r.WS.Dir(w.ID), "a0", "b0", "c0", "f0.txt")
	if err := os.WriteFile(target, []byte("CHANGED\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := e.r.WS.Sync(w.ID); err != nil {
		t.Fatal(err)
	}
	if ops, _ := e.r.DB.ListOps(w.ID); len(ops) != 1 {
		t.Fatalf("期望 1 个 op，得到 %d", len(ops))
	}

	res, err := e.r.Land(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := e.r.DB.GetVersion(res.VersionID)
	if got := countTreeFiles(t, e, v.Tree); got != want {
		t.Fatalf("★ land 后文件数 = %d，期望 %d —— 嵌套目录里的文件被清空了", got, want)
	}

	// 改动本身应生效
	tr, _ := e.r.Store.GetTree(v.Tree)
	if _, ok := tr.Get("a0"); !ok {
		t.Fatal("顶层 a0 丢失")
	}
	data, err := readTreeFile(e, tr, "a0/b0/c0/f0.txt")
	if err != nil {
		t.Fatal(err)
	}
	if data != "CHANGED\n" {
		t.Fatalf("改动未生效: %q", data)
	}
	// 未改动的邻居必须还在
	other, err := readTreeFile(e, tr, "a0/b0/c1/f1.txt")
	if err != nil {
		t.Fatalf("未改动的邻居丢失: %v", err)
	}
	if other != "content 0-0-1\n" {
		t.Fatalf("邻居内容被改: %q", other)
	}
}

// TestLandPreservesDeepSingleChain 覆盖单链深目录（a/b/c/d/e/f.txt）。
func TestLandPreservesDeepSingleChain(t *testing.T) {
	e := newEnv(t)
	e.seed("a/b/c/d/e/f.txt", "deep\n")
	e.seed("a/b/g.txt", "sibling\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	head, _, _ := e.r.Head()
	hv, _ := e.r.DB.GetVersion(head)
	if got := countTreeFiles(t, e, hv.Tree); got != 2 {
		t.Fatalf("导入文件数 = %d，期望 2", got)
	}

	w, err := e.r.WS.Open(workspace.OpenRequest{TaskDesc: "改深层文件"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.r.WS.Dir(w.ID), "a", "b", "c", "d", "e", "f.txt"),
		[]byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.Land(w.ID); err != nil {
		t.Fatal(err)
	}
	head2, _, _ := e.r.Head()
	v, _ := e.r.DB.GetVersion(head2)
	if got := countTreeFiles(t, e, v.Tree); got != 2 {
		t.Fatalf("land 后文件数 = %d，期望 2", got)
	}
	tr, _ := e.r.Store.GetTree(v.Tree)
	if got, err := readTreeFile(e, tr, "a/b/c/d/e/f.txt"); err != nil || got != "changed\n" {
		t.Fatalf("深层文件内容 = %q err=%v", got, err)
	}
	if got, err := readTreeFile(e, tr, "a/b/g.txt"); err != nil || got != "sibling\n" {
		t.Fatalf("兄弟文件内容 = %q err=%v", got, err)
	}
}

// --- 辅助 ---------------------------------------------------------------

func countTreeFiles(t *testing.T, e *env, root hash.ID) int {
	t.Helper()
	n := 0
	var walk func(id hash.ID)
	walk = func(id hash.ID) {
		tr, err := e.r.Store.GetTree(id)
		if err != nil {
			t.Fatalf("GetTree(%s): %v", id, err)
		}
		for _, en := range tr.Entries {
			if en.Kind == 1 { // codec.EntryTree
				walk(en.ID)
				continue
			}
			n++
		}
	}
	walk(root)
	return n
}

// readTreeFile 按仓库相对路径读取树中的文件内容。
func readTreeFile(e *env, tr *tree.Tree, rel string) (string, error) {
	cur := tr
	parts := splitSlash(rel)
	for i, seg := range parts {
		en, ok := cur.Get(seg)
		if !ok {
			return "", fmt.Errorf("路径不存在: %s", rel)
		}
		if i == len(parts)-1 {
			b, err := e.r.Store.ReadFileData(en.ID)
			return string(b), err
		}
		sub, err := e.r.Store.GetTree(en.ID)
		if err != nil {
			return "", err
		}
		cur = sub
	}
	return "", fmt.Errorf("空路径")
}

func splitSlash(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
