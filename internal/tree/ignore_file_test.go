package tree_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyiwen/p37/internal/tree"
)

func TestLoadIgnoreFileMissingIsNotAnError(t *testing.T) {
	lines, err := tree.LoadIgnoreFile(t.TempDir())
	if err != nil {
		t.Fatalf("缺少 .p37ignore 不应报错: %v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("期望空，得到 %v", lines)
	}
}

func TestLoadIgnoreFileCombinedWithDefaults(t *testing.T) {
	root := t.TempDir()
	content := "# comment\n*.log\n!keep.log\n\nbuild/\n"
	if err := os.WriteFile(filepath.Join(root, tree.IgnoreFileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := tree.LoadIgnoreFile(root)
	if err != nil {
		t.Fatal(err)
	}
	ig, err := tree.NewIgnore(true, lines)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path string
		dir  bool
		want bool
	}{
		{"a.log", false, true},
		{"sub/a.log", false, true},
		{"keep.log", false, false}, // '!' 取反
		{"sub/keep.log", false, false},
		{"build/x", true, true},
		{"node_modules/pkg/x.js", false, true}, // 内置集
		{"keep.txt", false, false},
	}
	for _, c := range cases {
		if got := ig.Match(c.path, c.dir); got != c.want {
			t.Errorf("Match(%q, dir=%v) = %v，期望 %v", c.path, c.dir, got, c.want)
		}
	}
}

func TestLoadIgnoreFileBadSyntaxReportsLine(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, tree.IgnoreFileName), []byte("ok\na//b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := tree.LoadIgnoreFile(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tree.NewIgnore(false, lines); err == nil {
		t.Fatal("非法模式应报语法错误")
	}
}
