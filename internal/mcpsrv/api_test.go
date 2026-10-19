package mcpsrv

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/workspace"
)

// TestWSInfoUsesSnakeCase 是 API 一致性回归（附录 K）。
//
// 曾经 ws_info 直接返回 workspace.Status，而该结构体没有 json tag，
// 于是字段名变成 PascalCase（ID / ParentKind / Dir / ...），与其它工具的
// snake_case 约定不一致，按 schema 实现的客户端会解析失败。
func TestWSInfoUsesSnakeCase(t *testing.T) {
	st := workspace.Status{
		ID: "ws_0123456789abcdef", ParentKind: meta.ParentVersion, ParentID: "v1",
		Dir: "/tmp/x", Dirty: true, CapturedOps: 3, ConflictCount: 1,
		State: meta.StateActive, Materialize: meta.MaterializeOverlay,
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"ws_id", "parent_kind", "parent_id", "base_version", "dir",
		"dirty", "captured_ops", "conflict_count", "state", "materialize",
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("缺少 snake_case 字段 %q（实际: %s）", k, b)
		}
	}
	// 不应出现 PascalCase 残留
	for k := range m {
		if k != "" && strings.ToUpper(k[:1]) == k[:1] {
			t.Errorf("出现 PascalCase 字段 %q", k)
		}
	}
}

// TestErrorCodesCoverNewStates 断言新错误码已登记（附录 E）。
func TestErrorCodesCoverNewStates(t *testing.T) {
	// 通过 classify 走一遍
	code, _, _ := classify(workspace.ErrNotMounted)
	if code != "WS_NOT_MOUNTED" {
		t.Fatalf("ErrNotMounted -> %q，期望 WS_NOT_MOUNTED", code)
	}
	code, _, _ = classify(workspace.ErrDirMissing)
	if code != "WS_DIR_MISSING" {
		t.Fatalf("ErrDirMissing -> %q", code)
	}
}
