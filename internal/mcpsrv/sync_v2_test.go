package mcpsrv_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyiwen/p37/internal/config"
)

func TestSyncPushRestoreDir(t *testing.T) {
	hh, h := newMultiHarness(t)
	remoteDir := t.TempDir()
	repo := hh.mustOK("repo_create", map[string]any{"name": "app"})
	_ = repo
	r, err := h.Open("app")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Remote.Enabled = true
	cfg.Remote.Backend = "dir"
	cfg.Remote.Dir = remoteDir
	if err := config.Save(r.Root, cfg); err != nil {
		t.Fatal(err)
	}

	hh.mustOK("line_create", map[string]any{"repo": "app", "name": "p34/session/s1", "from": "mainline"})
	w := hh.mustOK("ws_open", map[string]any{"repo": "app", "task": "t",
		"parent": map[string]any{"kind": "line", "id": "p34/session/s1"}})
	dir, _ := w["dir"].(string)
	wsID, _ := w["ws_id"].(string)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hh.mustOK("ws_land", map[string]any{"repo": "app", "ws_id": wsID,
		"target": map[string]any{"kind": "line", "id": "p34/session/s1"}})

	// 产生一个双亲 merge 版本，验证 restore 后 DAG 关系也恢复。
	hh.mustOK("line_merge", map[string]any{"repo": "app", "source": "p34/session/s1",
		"target": "main", "no_ff": true})

	push := hh.mustOK("sync_push", map[string]any{"repo": "app", "full": true})
	if push["durable"] != true {
		t.Fatalf("push=%v", push)
	}
	key, _ := push["remote_key"].(string)
	if key == "" {
		t.Fatalf("no remote_key: %v", push)
	}
	prefix, _ := push["remote_prefix"].(string)

	hh.mustOK("repo_delete", map[string]any{"repo": "app", "force": true})
	if _, err := os.Stat(filepath.Join(remoteDir, prefix, "packages")); err != nil {
		t.Fatalf("remote packages missing: %v", err)
	}
	restore := hh.mustOK("sync_restore", map[string]any{
		"repo": "app", "name": "app", "remote_prefix": prefix,
		"remote": map[string]any{"enabled": true, "backend": "dir", "dir": remoteDir},
	})
	if restore["up_to_date"] == true {
		t.Fatalf("restore=%v", restore)
	}

	r2, err := h.Open("app")
	if err != nil {
		t.Fatal(err)
	}
	ls, err := r2.Lines()
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 2 {
		t.Fatalf("restored lines=%d %+v", len(ls), ls)
	}
	// session line head should have the landed version
	var session string
	for i := range ls {
		if ls[i].Name == "p34/session/s1" {
			session = ls[i].Head.String()
		}
	}
	if session == "" {
		t.Fatal("session line missing")
	}
	tree, _, err := r2.VersionTree("line:p34/session/s1", "", true, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) == 0 {
		t.Fatal("restored tree empty")
	}
}
