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
