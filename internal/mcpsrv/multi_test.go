package mcpsrv_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/hub"
	"github.com/chenyiwen/p37/internal/mcpsrv"
)

func newMultiHarness(t *testing.T) (*harness, *hub.Hub) {
	t.Helper()
	root := t.TempDir()
	cfg := config.Default()
	h, err := hub.New(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.SetAllowNonPersistent(true)
	srv := mcpsrv.NewWithResolver(nil, h.Open)
	srv.Hub = h
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close(); _ = h.Close() })
	return &harness{t: t, cs: cs, root: root}, h
}

func TestMultiRepoMCPLineLoop(t *testing.T) {
	hh, _ := newMultiHarness(t)
	repo := hh.mustOK("repo_create", map[string]any{"name": "app"})
	repoID, _ := repo["id"].(string)
	if repoID == "" {
		t.Fatal("repo_create 没有 id")
	}

	ls := hh.mustOK("line_list", map[string]any{"repo": "app"})
	arr, _ := ls["_array"].([]any)
	if len(arr) != 1 {
		t.Fatalf("lines=%v", ls)
	}

	hh.mustOK("line_create", map[string]any{"repo": "app", "name": "p34/session/s1", "from": "mainline"})
	w := hh.mustOK("ws_open", map[string]any{
		"repo": "app", "task": "turn 1",
		"parent": map[string]any{"kind": "line", "id": "p34/session/s1"},
	})
	wsID, _ := w["ws_id"].(string)
	dir, _ := w["dir"].(string)
	if wsID == "" || dir == "" {
		t.Fatalf("ws_open=%v", w)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hh.mustOK("ws_sync", map[string]any{"repo": "app", "ws_id": wsID})
	preview := hh.mustOK("land_preview", map[string]any{
		"repo": "app", "ws_id": wsID,
		"target": map[string]any{"kind": "line", "id": "p34/session/s1"},
	})
	if ok, _ := preview["can_land"].(bool); !ok {
		t.Fatalf("preview=%v", preview)
	}
	land := hh.mustOK("ws_land", map[string]any{
		"repo": "app", "ws_id": wsID,
		"target":        map[string]any{"kind": "line", "id": "p34/session/s1"},
		"expected_head": preview["target_head"], "expected_ws_tree": preview["ws_tree"],
	})
	if land["version_id"] == nil {
		t.Fatalf("land=%v", land)
	}

	w2 := hh.mustOK("ws_open", map[string]any{
		"repo": "app", "task": "turn 2",
		"parent": map[string]any{"kind": "line", "id": "p34/session/s1"},
	})
	dir2, _ := w2["dir"].(string)
	b, err := os.ReadFile(filepath.Join(dir2, "a.txt"))
	if err != nil || string(b) != "hello\n" {
		t.Fatalf("reopen content=%q err=%v", b, err)
	}

	tree := hh.mustOK("version_tree", map[string]any{
		"repo": "app", "ref": "line:p34/session/s1", "recursive": true,
	})
	items, _ := tree["items"].([]any)
	if len(items) == 0 {
		t.Fatalf("tree=%v", tree)
	}

	info := hh.mustOK("repo_info", map[string]any{"repo": "app"})
	if info["id"] != repoID {
		t.Fatalf("info=%v", info)
	}
}

func TestMultiRepoIsolation(t *testing.T) {
	hh, _ := newMultiHarness(t)
	hh.mustOK("repo_create", map[string]any{"name": "a"})
	hh.mustOK("repo_create", map[string]any{"name": "b"})
	list := hh.mustOK("repo_list", nil)
	raw, _ := json.Marshal(list)
	if len(raw) == 0 {
		t.Fatal("repo_list empty")
	}
	if _, err := hh.call("line_create", map[string]any{"repo": "b", "name": "x", "from": "mainline"}), error(nil); err != nil {
		t.Fatal(err)
	}
	hh.mustOK("repo_delete", map[string]any{"repo": "a", "force": true})
	if _, err := hh.call("repo_info", map[string]any{"repo": "a"}), error(nil); err != nil {
		t.Fatal(err)
	}
	hh.mustOK("repo_info", map[string]any{"repo": "b"})
}
