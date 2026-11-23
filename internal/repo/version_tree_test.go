package repo_test

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"sort"
	"testing"

	"github.com/chenyiwen/p37/internal/repo"
)

// TestVersionTreeAtDirectoryPath 钉住 version_tree 的 path 语义：
// path 指向目录时必须列出该目录的子项，而不是父目录的同级项。
func TestVersionTreeAtDirectoryPath(t *testing.T) {
	e := newEnv(t)
	e.seed("root.txt", "root\n")
	e.seed("a/b.txt", "B\n")
	e.seed("a/c/d.txt", "D\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	head, _, _ := e.r.Head()
	ref := "version:" + head.String()

	paths := func(items []repo.TreeEntry) []string {
		out := make([]string, 0, len(items))
		for _, it := range items {
			out = append(out, it.Path)
		}
		sort.Strings(out)
		return out
	}
	equal := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	// 根目录：只应有 a/ 和 root.txt。
	items, truncated, err := e.r.VersionTree(ref, "", false, 100)
	if err != nil || truncated {
		t.Fatalf("root tree err=%v truncated=%v", err, truncated)
	}
	if want := []string{"a", "root.txt"}; !equal(paths(items), want) {
		t.Fatalf("root paths=%v want=%v", paths(items), want)
	}

	// 目录 a：只应有 a/b.txt 和 a/c，不能再看到 root.txt。
	items, _, err = e.r.VersionTree(ref, "a", false, 100)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a/b.txt", "a/c"}; !equal(paths(items), want) {
		t.Fatalf("dir a paths=%v want=%v", paths(items), want)
	}
	for _, it := range items {
		if it.Path == "a/c" && it.Type != "dir" {
			t.Fatalf("a/c type=%q want dir", it.Type)
		}
		if it.Path == "a/b.txt" && it.Type != "file" {
			t.Fatalf("a/b.txt type=%q want file", it.Type)
		}
	}

	// 递归：a/c 之下也要展开。
	items, _, err = e.r.VersionTree(ref, "a", true, 100)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a/b.txt", "a/c", "a/c/d.txt"}; !equal(paths(items), want) {
		t.Fatalf("recursive a paths=%v want=%v", paths(items), want)
	}

	// 文件路径仍返回单条。
	items, _, err = e.r.VersionTree(ref, "a/b.txt", false, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Path != "a/b.txt" || items[0].Type != "file" {
		t.Fatalf("file tree=%+v", items)
	}

	// 不存在的路径必须报错，不能静默回退到根。
	if _, _, err := e.r.VersionTree(ref, "a/nope", false, 100); !errors.Is(err, repo.ErrPathNotFound) {
		t.Fatalf("missing path err=%v want ErrPathNotFound", err)
	}
}

// TestVersionArchiveAtDirectoryPath 归档目录时不能把父目录的同级文件带进去。
func TestVersionArchiveAtDirectoryPath(t *testing.T) {
	e := newEnv(t)
	e.seed("root.txt", "root\n")
	e.seed("a/b.txt", "B\n")
	e.seed("a/c/d.txt", "D\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	head, _, _ := e.r.Head()
	ref := "version:" + head.String()

	res, err := e.r.VersionArchive(ref, "a", "tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	sort.Strings(names)
	if want := []string{"a/b.txt", "a/c/", "a/c/d.txt"}; !equalStrings(names, want) {
		t.Fatalf("archive names=%v want=%v", names, want)
	}
	for _, n := range names {
		if n == "root.txt" || n == "./root.txt" {
			t.Fatalf("archive 泄漏了父目录文件: %v", names)
		}
	}
}
