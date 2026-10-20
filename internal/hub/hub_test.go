package hub_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/hub"
	"github.com/chenyiwen/p37/internal/workspace"
)

func TestMultiRepoLifecycle(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	h, err := hub.New(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	a, err := h.Create("app-a", "", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Create("app-b", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatal("repo id 重复")
	}
	if _, err := h.Create("app-a", "", ""); err != nil {
		t.Fatalf("重复 create 应幂等: %v", err)
	}

	ra, err := h.Open("app-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ra.CreateLine("p34/session/1", "mainline", false); err != nil {
		t.Fatal(err)
	}
	if _, err := ra.WS.Open(workspace.OpenRequest{Parent: "line:p34/session/1", TaskDesc: "t"}); err != nil {
		t.Fatal(err)
	}

	rb, err := h.Open(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	ls, _ := rb.Lines()
	if len(ls) != 1 {
		t.Fatalf("repo b lines=%d", len(ls))
	}

	// 删除 a 不影响 b
	if _, err := h.Delete("app-a", true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Open("app-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.Path); !os.IsNotExist(err) {
		t.Fatalf("repo a 目录应删除: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".p37", "registry.db")); err != nil {
		t.Fatal(err)
	}
}

func TestMultiRepoRestart(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	h, err := hub.New(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b", "c"} {
		if _, err := h.Create(n, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	ra, err := h.Open("a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ra.CreateLine("s", "mainline", false); err != nil {
		t.Fatal(err)
	}
	if _, err := ra.WS.Open(workspace.OpenRequest{Parent: "line:s", TaskDesc: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	h2, err := hub.New(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()
	es, err := h2.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 3 {
		t.Fatalf("repos=%d", len(es))
	}
	ra2, err := h2.Open("a")
	if err != nil {
		t.Fatal(err)
	}
	ls, err := ra2.Lines()
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 2 {
		t.Fatalf("lines=%d", len(ls))
	}
	ws, err := ra2.WS.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 {
		t.Fatalf("workspaces=%d", len(ws))
	}
}
