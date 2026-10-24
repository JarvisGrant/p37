//go:build linux

package hub_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/hub"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/repo"
	"github.com/chenyiwen/p37/internal/workspace"
)

func TestUnregisterForceUnmountsOverlay(t *testing.T) {
	if err := mountmgr.ProbeOverlay(t.TempDir()); err != nil {
		t.Skipf("OverlayFS 不可用: %v", err)
	}
	root := t.TempDir()
	h, err := hub.New(root, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()

	path := filepath.Join(root, "app")
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Workspace.Materialize = string(mountmgr.ModeOverlay)
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(path, cfg.RepoOptions())
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	if _, err := h.RegisterPath("app", path, hub.RegisterOptions{}); err != nil {
		t.Fatal(err)
	}
	r, err = h.Open("app")
	if err != nil {
		t.Fatal(err)
	}
	w, err := r.WS.Open(workspace.OpenRequest{Parent: "mainline", TaskDesc: "overlay", Mode: mountmgr.ModeOverlay})
	if errors.Is(err, mountmgr.ErrUnavailable) {
		t.Skipf("OverlayFS 不可用: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	wsDir := filepath.Join(path, "workspaces", w.ID)
	if !mountmgr.IsMountPoint(wsDir) {
		t.Fatalf("工作区未挂载: %s", wsDir)
	}
	if _, err := h.Unregister("app", true); err != nil {
		t.Fatal(err)
	}
	if mountmgr.IsMountPoint(wsDir) {
		t.Fatalf("unregister force 后 overlay 仍挂载: %s", wsDir)
	}
}
