package mcpsrv_test

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSyncToolsOverMCP 覆盖 sync_prepare / sync_list / sync_show / sync_status。
func TestSyncToolsOverMCP(t *testing.T) {
	h := newHarness(t)
	h.seed("a.txt", "v0\n")
	if _, err := h.r.Import(h.srcDir); err != nil {
		t.Fatal(err)
	}

	// 造一个版本。
	w := h.mustOK("ws_open", map[string]any{"task_desc": "同步测试"})
	wsID, _ := w["ws_id"].(string)
	dir, _ := w["dir"].(string)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.mustOK("ws_land", map[string]any{"ws_id": wsID})

	// 生成包（只暂存）。
	pr := h.mustOK("sync_prepare", map[string]any{"force": true})
	if created, _ := pr["created"].(bool); !created {
		t.Fatalf("未生成包: %v", pr)
	}
	m, _ := pr["manifest"].(map[string]any)
	if m == nil {
		t.Fatal("缺少 manifest")
	}
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatal("manifest 缺少 id")
	}
	if m["kind"] != "checkpoint" {
		t.Fatalf("首次应为 checkpoint，得到 %v", m["kind"])
	}

	// 列出。
	lst := h.mustOK("sync_list", nil)
	arr, _ := lst["_array"].([]any)
	if len(arr) != 1 {
		t.Fatalf("sync_list = %v", lst)
	}
	row, _ := arr[0].(map[string]any)
	if row["id"] != id || row["uploaded"] != false {
		t.Fatalf("列表项异常: %v", row)
	}

	// 查看 + 校验。
	sh := h.mustOK("sync_show", map[string]any{"id": "last", "verify": true})
	if v, _ := sh["verified"].(bool); !v {
		t.Fatalf("verify 未通过: %v", sh)
	}

	// 状态。
	st := h.mustOK("sync_status", nil)
	if st["pending_packages"].(float64) != 1 {
		t.Fatalf("pending_packages = %v", st["pending_packages"])
	}
	if st["local_head"] == "" {
		t.Fatal("缺少 local_head")
	}
}
