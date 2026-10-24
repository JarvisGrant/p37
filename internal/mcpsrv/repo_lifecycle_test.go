package mcpsrv_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/hub"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/repo"
	"github.com/chenyiwen/p37/internal/workspace"
)

func prepareExternalRepo(t *testing.T, path, name string, withMeta bool) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Workspace.Materialize = string(mountmgr.ModeCopy)
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(path, cfg.RepoOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	id, err := hub.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if withMeta {
		if err := os.MkdirAll(filepath.Join(path, ".p37"), 0o700); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(map[string]string{"id": id, "name": name, "default_line": "main"})
		if err := os.WriteFile(filepath.Join(path, ".p37", "repo.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func TestRepoLifecycleToolsListed(t *testing.T) {
	hh, _ := newMultiHarness(t)
	res, err := hh.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"repo_register": false, "repo_unregister": false, "repo_scan": false}
	for _, tool := range res.Tools {
		if _, ok := want[tool.Name]; !ok {
			continue
		}
		want[tool.Name] = true
		b, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Required   []string       `json:"required"`
			Properties map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(b, &schema); err != nil {
			t.Fatalf("%s schema: %v", tool.Name, err)
		}
		for _, k := range []string{"repo", "path"} {
			if tool.Name == "repo_register" && schema.Properties[k] == nil {
				t.Fatalf("%s 缺少 %s: %s", tool.Name, k, b)
			}
		}
		for _, k := range []string{"repo"} {
			if schema.Properties[k] == nil {
				t.Fatalf("%s 缺少 %s: %s", tool.Name, k, b)
			}
		}
	}
	for k, ok := range want {
		if !ok {
			t.Fatalf("工具 %s 未注册", k)
		}
	}
}

func TestRepoRegisterMCP(t *testing.T) {
	hh, _ := newMultiHarness(t)
	path := filepath.Join(hh.root, "app")
	// 未初始化 -> repo_not_initialized
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatal(err)
	}
	hh.mustFail("repo_register", "repo_not_initialized", map[string]any{"repo": "app", "path": path})
	e := hh.mustOK("repo_register", map[string]any{
		"repo": "app", "path": path, "init_if_missing": true, "default_line": "trunk",
	})
	if e["name"] != "app" || e["available"] != true || e["default_line"] != "trunk" {
		t.Fatalf("register=%v", e)
	}
	// 同名幂等
	e2 := hh.mustOK("repo_register", map[string]any{"repo": "app", "path": path})
	if e2["id"] != e["id"] {
		t.Fatalf("idempotent=%v", e2)
	}
	// 同 path 不同 name 与 repo.json 冲突
	hh.mustFail("repo_register", "repo_name_mismatch",
		map[string]any{"repo": "other", "path": path})
	// root 外
	hh.mustFail("repo_register", "invalid_path",
		map[string]any{"repo": "x", "path": t.TempDir()})
	// symlink 逃逸
	outside := t.TempDir()
	link := filepath.Join(hh.root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	hh.mustFail("repo_register", "invalid_path",
		map[string]any{"repo": "link", "path": link})

	list := hh.mustOK("repo_list", nil)
	arr, _ := list["_array"].([]any)
	if len(arr) != 1 {
		t.Fatalf("list=%v", list)
	}
	item, _ := arr[0].(map[string]any)
	if item["path"] != path || item["available"] != true {
		t.Fatalf("list item=%v", item)
	}

	info := hh.mustOK("repo_info", map[string]any{"repo": "app"})
	if info["mounted"] != false || info["available"] != true {
		t.Fatalf("info=%v", info)
	}

	// 卷不可用：repo_info/ list 不 Open，仍可观测。
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	info = hh.mustOK("repo_info", map[string]any{"repo": "app"})
	if info["available"] != false || info["degraded"] != true {
		t.Fatalf("unavailable info=%v", info)
	}
}

func TestRepoUnregisterBusyAndForce(t *testing.T) {
	hh, h := newMultiHarness(t)
	path := filepath.Join(hh.root, "app")
	prepareExternalRepo(t, path, "app", true)
	hh.mustOK("repo_register", map[string]any{"repo": "app", "path": path})
	r, err := h.Open("app")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WS.Open(workspace.OpenRequest{Parent: "mainline", TaskDesc: "busy"}); err != nil {
		t.Fatal(err)
	}
	e := hh.mustFail("repo_unregister", "repo_busy", map[string]any{"repo": "app"})
	detail, _ := e["detail"].(map[string]any)
	if detail == nil {
		t.Fatalf("busy detail=%v", e)
	}
	if ws, _ := detail["workspaces"].([]any); len(ws) == 0 {
		t.Fatalf("busy detail=%v", detail)
	}
	if list := hh.mustOK("repo_list", nil); len(list["_array"].([]any)) != 1 {
		t.Fatal("busy 后不应注销")
	}
	res := hh.mustOK("repo_unregister", map[string]any{"repo": "app", "force": true})
	if res["unregistered"] != true || res["already_unregistered"] != false {
		t.Fatalf("unregister=%v", res)
	}
	if list := hh.mustOK("repo_list", nil); len(list["_array"].([]any)) != 0 {
		t.Fatal("force 后应注销")
	}
	res = hh.mustOK("repo_unregister", map[string]any{"repo": "app"})
	if res["already_unregistered"] != true {
		t.Fatalf("重复 unregister=%v", res)
	}
	if _, err := os.Stat(filepath.Join(path, ".p37", "p37.db")); err != nil {
		t.Fatalf("数据应保留: %v", err)
	}
}

func TestRepoScanMCPNonEmptyAndDryRun(t *testing.T) {
	hh, _ := newMultiHarness(t)
	hh.mustOK("repo_create", map[string]any{"name": "existing"})
	path := filepath.Join(hh.root, "app")
	prepareExternalRepo(t, path, "app", true)

	res := hh.mustOK("repo_scan", map[string]any{"dry_run": true})
	if added, _ := res["added"].([]any); len(added) != 1 {
		t.Fatalf("dry scan=%v", res)
	}
	if list := hh.mustOK("repo_list", nil); len(list["_array"].([]any)) != 1 {
		t.Fatal("dry_run 不应写 registry")
	}
	res = hh.mustOK("repo_scan", nil)
	if added, _ := res["added"].([]any); len(added) != 1 {
		t.Fatalf("scan=%v", res)
	}
	if list := hh.mustOK("repo_list", nil); len(list["_array"].([]any)) != 2 {
		t.Fatal("scan 后应有 2 个 repo")
	}
	res = hh.mustOK("repo_scan", nil)
	if added, _ := res["added"].([]any); len(added) != 0 {
		t.Fatalf("幂等 scan=%v", res)
	}
}

func TestRepoDeleteMountpointMCP(t *testing.T) {
	hh, h := newMultiHarness(t)
	path := filepath.Join(hh.root, "app")
	prepareExternalRepo(t, path, "app", true)
	hh.mustOK("repo_register", map[string]any{"repo": "app", "path": path})
	h.SetMountPointProbe(func(p string) bool { return filepath.Clean(p) == filepath.Clean(path) })
	hh.mustFail("repo_delete", "repo_is_mountpoint", map[string]any{"repo": "app", "force": true})
	if _, err := os.Stat(filepath.Join(path, ".p37", "p37.db")); err != nil {
		t.Fatalf("挂载点拒绝后数据应完整: %v", err)
	}
}
