package mcpsrv_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/repo"
)

// TestQuotaVisibleAndDetailed 覆盖 B：ws_quota 可查，quota_exceeded 的
// detail 带 scope/used/limit。
func TestQuotaVisibleAndDetailed(t *testing.T) {
	h := newHarnessOpt(t, repo.Options{Materialize: mountmgr.ModeAuto, MaxActive: 1})
	h.seed("a.txt", "A\n")
	if _, err := h.r.Import(h.srcDir); err != nil {
		t.Fatal(err)
	}

	q := h.mustOK("ws_quota", map[string]any{})
	if got := q["max_active"]; got != float64(1) {
		t.Fatalf("max_active = %v", got)
	}
	if got := q["active_available"]; got != float64(1) {
		t.Fatalf("active_available = %v", got)
	}

	if w := h.mustOK("ws_open", map[string]any{"task": "one"}); w["ws_id"] == nil {
		t.Fatalf("ws_open = %v", w)
	}
	q = h.mustOK("ws_quota", map[string]any{})
	if got := q["active_used"]; got != float64(1) {
		t.Fatalf("active_used = %v", got)
	}
	if got := q["active_available"]; got != float64(0) {
		t.Fatalf("active_available = %v", got)
	}

	e := h.mustFail("ws_open", "quota_exceeded", map[string]any{"task": "two"})
	detail, _ := e["detail"].(map[string]any)
	if detail == nil {
		t.Fatalf("quota_exceeded 没有 detail: %v", e)
	}
	if detail["scope"] != "workspace_active" ||
		detail["used"] != float64(1) || detail["limit"] != float64(1) {
		t.Fatalf("detail = %v", detail)
	}
}

// TestLandIntoConflictAPI 覆盖 C/D 的 MCP 层接线：冲突返回到子工作区上、
// 父 land 的 blockers 结构化、解决后带 preview_id 重试成功。
func TestLandIntoConflictAPI(t *testing.T) {
	h := newHarness(t)
	h.seed("a.txt", "A\n")
	if _, err := h.r.Import(h.srcDir); err != nil {
		t.Fatal(err)
	}
	parent := h.mustOK("ws_open", map[string]any{"task": "parent"})
	parentID, _ := parent["ws_id"].(string)
	child := h.mustOK("ws_open", map[string]any{
		"parent": map[string]any{"kind": "workspace", "id": parentID}, "task": "child",
	})
	childID, _ := child["ws_id"].(string)
	if err := os.WriteFile(filepath.Join(h.r.WS.Dir(childID), "a.txt"), []byte("CHILD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(h.r.WS.Dir(parentID), "a.txt")); err != nil {
		t.Fatal(err)
	}

	res := h.mustOK("ws_land_into", map[string]any{
		"ws_id": childID, "parent_ws_id": parentID,
	})
	if res["state"] != "conflict" || res["can_land"] != false {
		t.Fatalf("ws_land_into = %v", res)
	}
	if res["preview_id"] != childID {
		t.Fatalf("preview_id = %v，期望子工作区 id", res["preview_id"])
	}
	rows := h.mustOK("conflict_list", map[string]any{"ws_id": childID})["_array"].([]any)
	if len(rows) == 0 {
		t.Fatal("conflict_list(child) 为空")
	}

	e := h.mustFail("ws_land", "ws_has_active_children", map[string]any{"ws_id": parentID})
	detail, _ := e["detail"].(map[string]any)
	blocked, _ := detail["blocked_by_children"].([]any)
	if len(blocked) != 1 {
		t.Fatalf("blocked_by_children = %v", detail)
	}
	first, _ := blocked[0].(map[string]any)
	if first["ws_id"] != childID || first["state"] != "conflicted" {
		t.Fatalf("blocked child = %v", first)
	}

	h.mustOK("conflict_resolve_all", map[string]any{
		"ws_id": childID, "policy": "prefer_upper",
	})
	res2 := h.mustOK("ws_land_into", map[string]any{
		"ws_id": childID, "parent_ws_id": parentID,
		"expected_parent_tree": res["target_head"],
		"preview_id":           res["preview_id"],
	})
	if res2["can_land"] != true || res2["state"] != "done" {
		t.Fatalf("重试结果 = %v", res2)
	}
	q := h.mustOK("ws_quota", map[string]any{})
	if got := q["active_used"]; got != float64(1) {
		t.Fatalf("land_into 成功后 active_used = %v，期望 1", got)
	}
}
