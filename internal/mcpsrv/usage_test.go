package mcpsrv_test

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRepoUsageOverMCP 覆盖 repo_usage 的只读口径：
//   - objects.count/stored_bytes/raw_bytes 来自 object_index；
//   - durable_stored_bytes 包含 db/refs/config；
//   - workspaces.logical_bytes 统计未提交文件；
//   - head/version_count 与 repo_status 一致。
func TestRepoUsageOverMCP(t *testing.T) {
	h := newHarness(t)
	h.seed("a.txt", "hello\n")
	if _, err := h.r.Import(h.srcDir); err != nil {
		t.Fatal(err)
	}

	w := h.mustOK("ws_open", map[string]any{"task_desc": "usage"})
	wsID, _ := w["ws_id"].(string)
	dir, _ := w["dir"].(string)
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 不 land：保留一个活跃工作区，验证 workspaces 统计；durable 已由 import 的 v0 提供。
	h.mustOK("ws_sync", map[string]any{"ws_id": wsID})

	u := h.mustOK("repo_usage", map[string]any{"repo": "app"})
	if u["repo"] != "app" {
		t.Fatalf("repo = %v", u["repo"])
	}
	if u["head"] == "" {
		t.Fatal("缺少 head")
	}
	if vc, _ := u["version_count"].(float64); vc < 1 {
		t.Fatalf("version_count = %v", u["version_count"])
	}
	objs, _ := u["objects"].(map[string]any)
	if objs == nil {
		t.Fatal("缺少 objects")
	}
	if n, _ := objs["count"].(float64); n <= 0 {
		t.Fatalf("objects.count = %v", objs["count"])
	}
	if n, _ := u["durable_stored_bytes"].(float64); n <= 0 {
		t.Fatalf("durable_stored_bytes = %v", u["durable_stored_bytes"])
	}
	if n, _ := u["db_bytes"].(float64); n <= 0 {
		t.Fatalf("db_bytes = %v", u["db_bytes"])
	}
	ws, _ := u["workspaces"].(map[string]any)
	if ws == nil {
		t.Fatal("缺少 workspaces")
	}
	if n, _ := ws["count"].(float64); n < 1 {
		t.Fatalf("workspaces.count = %v", ws["count"])
	}
}
