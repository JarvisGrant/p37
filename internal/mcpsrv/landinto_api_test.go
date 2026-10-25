package mcpsrv_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/repo"
)

func writeWS(t *testing.T, h *harness, wsID, rel, content string) {
	t.Helper()
	p := filepath.Join(h.r.WS.Dir(wsID), rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLandIntoAPIRevisionFields 覆盖 MCP 层的 revision 暴露与结构化结果。
func TestLandIntoAPIRevisionFields(t *testing.T) {
	h := newHarnessOpt(t, repo.Options{Materialize: mountmgr.ModeCopy})
	h.seed("a.txt", "A\n")
	if _, err := h.r.Import(h.srcDir); err != nil {
		t.Fatal(err)
	}
	parent := h.mustOK("ws_open", map[string]any{"task": "parent"})
	parentID, _ := parent["ws_id"].(string)
	if parentID == "" || parent["base_tree"] == "" || parent["ops_epoch"] == nil {
		t.Fatalf("ws_open 缺少 revision 字段: %v", parent)
	}
	child := h.mustOK("ws_open", map[string]any{
		"task":   "child",
		"parent": map[string]any{"kind": "workspace", "id": parentID},
	})
	childID, _ := child["ws_id"].(string)
	if childID == "" {
		t.Fatalf("ws_open child = %v", child)
	}
	// 子工作区的 base_tree 必须等于父的当前有效树（不是 base_version）。
	infoChild := h.mustOK("ws_info", map[string]any{"ws_id": childID})
	if infoChild["base_tree"] == "" || infoChild["effective_tree"] == "" || infoChild["parent_tree"] == "" {
		t.Fatalf("ws_info 缺少 revision 字段: %v", infoChild)
	}
	infoParent := h.mustOK("ws_info", map[string]any{"ws_id": parentID})
	if infoChild["parent_tree"] != infoParent["effective_tree"] {
		t.Fatalf("child.parent_tree=%v 应等于 parent.effective_tree=%v",
			infoChild["parent_tree"], infoParent["effective_tree"])
	}

	writeWS(t, h, childID, "b.txt", "B\n")
	pv := h.mustOK("land_preview", map[string]any{
		"ws_id":  childID,
		"target": map[string]any{"kind": "workspace", "id": parentID},
	})
	if pv["base_tree"] == "" || pv["parent_tree_before"] == "" || pv["child_tree"] == "" {
		t.Fatalf("land_preview 缺少版本坐标: %v", pv)
	}
	if pv["can_land"] != true {
		t.Fatalf("预览应可落地: %v", pv)
	}
	applied, _ := pv["applied_paths"].([]any)
	if len(applied) != 1 || applied[0] != "b.txt" {
		t.Fatalf("applied_paths = %v，期望 [b.txt]", pv["applied_paths"])
	}
	childOnly, _ := pv["child_only_changed_paths"].([]any)
	if len(childOnly) != 1 || childOnly[0] != "b.txt" {
		t.Fatalf("child_only_changed_paths = %v", pv["child_only_changed_paths"])
	}

	// allowed_paths 越界：P37 直接拒绝。
	h.mustFail("ws_land_into", "path_not_allowed", map[string]any{
		"ws_id": childID, "parent_ws_id": parentID, "allowed_paths": []string{"a.txt"},
	})

	res := h.mustOK("ws_land_into", map[string]any{
		"ws_id": childID, "parent_ws_id": parentID,
		"expected_parent_tree": pv["parent_tree_before"],
		"idempotency_key":      "api-child-1",
	})
	if res["parent_tree_after"] == "" || res["state"] != "done" {
		t.Fatalf("land_into 结果不完整: %v", res)
	}
	if _, err := os.Stat(filepath.Join(h.r.WS.Dir(parentID), "b.txt")); err != nil {
		t.Fatalf("子改动未落盘: %v", err)
	}
	// 幂等重放。
	again := h.mustOK("ws_land_into", map[string]any{
		"ws_id": childID, "parent_ws_id": parentID, "idempotency_key": "api-child-1",
	})
	if again["already_landed"] != true || again["replayed"] != true {
		t.Fatalf("第二次 land 应幂等重放: %v", again)
	}
	// 审计记录可反查本次 land 的父树前后与写入集合。
	hist := h.mustOK("land_history", map[string]any{"ws_id": childID, "limit": 10})
	if hist["count"] == nil || hist["count"].(float64) < 1 {
		t.Fatalf("land_history 未记录本次 land: %v", hist)
	}
}

// TestLandIntoAPIParentMoved 覆盖 expected_parent_tree 失配的 error code/detail。
func TestLandIntoAPIParentMoved(t *testing.T) {
	h := newHarnessOpt(t, repo.Options{Materialize: mountmgr.ModeCopy})
	h.seed("a.txt", "A\n")
	if _, err := h.r.Import(h.srcDir); err != nil {
		t.Fatal(err)
	}
	parent := h.mustOK("ws_open", map[string]any{"task": "parent"})
	parentID, _ := parent["ws_id"].(string)
	child := h.mustOK("ws_open", map[string]any{
		"task":   "child",
		"parent": map[string]any{"kind": "workspace", "id": parentID},
	})
	childID, _ := child["ws_id"].(string)
	writeWS(t, h, childID, "b.txt", "B\n")

	pv := h.mustOK("land_preview", map[string]any{
		"ws_id":  childID,
		"target": map[string]any{"kind": "workspace", "id": parentID},
	})
	// 父在 preview 之后被改。
	writeWS(t, h, parentID, "a.txt", "A-new\n")
	if err := h.r.WS.Sync(parentID); err != nil {
		t.Fatal(err)
	}

	env := h.mustFail("ws_land_into", "target_moved", map[string]any{
		"ws_id": childID, "parent_ws_id": parentID,
		"expected_parent_tree": pv["parent_tree_before"],
	})
	detail, _ := env["detail"].(map[string]any)
	if detail == nil || detail["field"] != "parent_tree" {
		t.Fatalf("target_moved detail = %v", detail)
	}
	if got, _ := os.ReadFile(filepath.Join(h.r.WS.Dir(parentID), "a.txt")); string(got) != "A-new\n" {
		t.Fatalf("失配时不应写盘: %q", got)
	}
}
