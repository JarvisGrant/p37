package repo_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyiwen/p37/internal/cas"
	"github.com/chenyiwen/p37/internal/workspace"
)

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return n
}

// TestPackEndToEnd 覆盖 §9 P2 的离线打包在真实仓库闭环里的行为：
// 打包后对象一条不少、内容可读、GC 不误删、索引可从 pack 重建。
func TestPackEndToEnd(t *testing.T) {
	e := newEnv(t)
	want := map[string]string{}
	for i := 0; i < 30; i++ {
		rel := fmt.Sprintf("d%02d/f%02d.txt", i%5, i)
		content := fmt.Sprintf("file %d\nline\n", i)
		e.seed(rel, content)
		want[rel] = content
	}
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}

	looseBefore := countFiles(t, filepath.Join(e.root, ".p37", "objects"))
	if looseBefore == 0 {
		t.Fatal("导入后应有松散对象")
	}
	objsBefore, err := e.r.Store.ListObjects()
	if err != nil {
		t.Fatal(err)
	}

	// 打包。
	res, err := e.r.Pack(cas.DefaultPackOptions())
	if err != nil {
		t.Fatal(err)
	}
	if res.ObjectsPacked == 0 || res.PacksCreated == 0 {
		t.Fatalf("应打包若干对象: %+v", res)
	}
	looseAfter := countFiles(t, filepath.Join(e.root, ".p37", "objects"))
	if looseAfter != 0 {
		t.Fatalf("打包后仍有 %d 个松散对象", looseAfter)
	}

	// 对象数量与可读性不变。
	objsAfter, err := e.r.Store.ListObjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(objsAfter) != len(objsBefore) {
		t.Fatalf("对象数 %d -> %d", len(objsBefore), len(objsAfter))
	}
	for _, o := range objsAfter {
		if _, _, err := e.r.Store.Get(o.ID); err != nil {
			t.Fatalf("打包后对象 %s 不可读: %v", o.ID, err)
		}
	}

	// 版本内容仍完整。
	w, err := e.r.WS.Open(workspace.OpenRequest{TaskDesc: "校验打包"})
	if err != nil {
		t.Fatal(err)
	}
	for rel, content := range want {
		b, err := os.ReadFile(filepath.Join(e.r.WS.Dir(w.ID), rel))
		if err != nil {
			t.Fatalf("读取 %s: %v", rel, err)
		}
		if string(b) != content {
			t.Fatalf("%s 内容 = %q，期望 %q", rel, b, content)
		}
	}

	// GC（真删）不得误删可达对象；打包对象整包回收逻辑不误伤。
	rep, err := e.r.GC(false)
	if err != nil {
		t.Fatal(err)
	}
	_ = rep
	for _, o := range objsAfter {
		if _, _, err := e.r.Store.Get(o.ID); err != nil {
			t.Fatalf("GC 后对象 %s 不可读: %v", o.ID, err)
		}
	}

	// 索引可从 pack 重建（fsck 走的路径）。
	n, err := e.r.Store.RebuildIndex()
	if err != nil {
		t.Fatal(err)
	}
	if n != len(objsAfter) {
		t.Fatalf("RebuildIndex = %d，期望 %d", n, len(objsAfter))
	}
}
