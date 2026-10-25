package mcpsrv_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/repo"
)

func (h *harness) callSafe(name string, args any) (map[string]any, error) {
	res, err := h.cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: name, Arguments: args,
	})
	if err != nil {
		return nil, err
	}
	txt := res.Content[0].(*mcp.TextContent)
	var env map[string]any
	if err := json.Unmarshal([]byte(txt.Text), &env); err != nil {
		return nil, err
	}
	return env, nil
}

func TestWSEnsureConcurrent(t *testing.T) {
	h := newHarness(t)
	h.seed("a.txt", "A\n")
	if _, err := h.r.Import(h.srcDir); err != nil {
		t.Fatal(err)
	}

	const n = 5
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			env, err := h.callSafe("ws_ensure", map[string]any{
				"name": "root-turn-1", "task": "one",
			})
			if err != nil {
				errs[i] = err
				return
			}
			if ok, _ := env["ok"].(bool); !ok {
				t.Logf("ensure %d error: %v", i, env["error"])
				return
			}
			data, _ := env["data"].(map[string]any)
			ids[i], _ = data["ws_id"].(string)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("ensure %d: %v", i, err)
		}
		if ids[i] == "" {
			t.Fatalf("ensure %d 没有 ws_id", i)
		}
		if ids[i] != ids[0] {
			t.Fatalf("ws_id 不一致: %v", ids)
		}
	}
	list := h.mustOK("ws_list", map[string]any{})["_array"].([]any)
	if len(list) != 1 {
		t.Fatalf("ws_list = %d，期望 1", len(list))
	}

	// 再并发 ensure，仍是同一个 ws_id。
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			env, err := h.callSafe("ws_ensure", map[string]any{"name": "root-turn-1"})
			if err != nil || env["ok"] != true {
				t.Logf("again %d: %v %v", i, err, env["error"])
				return
			}
			data, _ := env["data"].(map[string]any)
			if data["ws_id"] != ids[0] {
				t.Errorf("again ws_id=%v want %s", data["ws_id"], ids[0])
			}
		}(i)
	}
	wg.Wait()
}

func TestWSEnsureIdempotencyKeyConcurrent(t *testing.T) {
	h := newHarness(t)
	h.seed("a.txt", "A\n")
	if _, err := h.r.Import(h.srcDir); err != nil {
		t.Fatal(err)
	}
	const n = 5
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			env, err := h.callSafe("ws_ensure", map[string]any{
				"name": "same-key-ensure", "idempotency_key": "ensure-key",
			})
			if err != nil || env["ok"] != true {
				t.Logf("ensure %d: %v %v", i, err, env["error"])
				return
			}
			data, _ := env["data"].(map[string]any)
			ids[i], _ = data["ws_id"].(string)
		}(i)
	}
	wg.Wait()
	for i := range ids {
		if ids[i] == "" || ids[i] != ids[0] {
			t.Fatalf("ensure idem ids=%v", ids)
		}
	}
}

func TestWSOpenIdempotencyConcurrent(t *testing.T) {
	h := newHarness(t)
	h.seed("a.txt", "A\n")
	if _, err := h.r.Import(h.srcDir); err != nil {
		t.Fatal(err)
	}
	const n = 5
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			env, err := h.callSafe("ws_open", map[string]any{
				"name": "idem-open", "task": "t", "idempotency_key": "same-key",
			})
			if err != nil || env["ok"] != true {
				t.Logf("open %d: %v %v", i, err, env["error"])
				return
			}
			data, _ := env["data"].(map[string]any)
			ids[i], _ = data["ws_id"].(string)
		}(i)
	}
	wg.Wait()
	for i := range ids {
		if ids[i] == "" || ids[i] != ids[0] {
			t.Fatalf("ws_open idempotency ids=%v", ids)
		}
	}
	list := h.mustOK("ws_list", map[string]any{})["_array"].([]any)
	if len(list) != 1 {
		t.Fatalf("ws_list = %d，期望 1", len(list))
	}
	if got := h.mustOK("ws_open", map[string]any{
		"name": "idem-open", "idempotency_key": "same-key",
	})["ws_id"]; got != ids[0] {
		t.Fatalf("replay ws_id=%v want %s", got, ids[0])
	}
}

func TestWSGoneLandReplay(t *testing.T) {
	h := newHarness(t)
	h.seed("a.txt", "A\n")
	if _, err := h.r.Import(h.srcDir); err != nil {
		t.Fatal(err)
	}
	w := h.mustOK("ws_open", map[string]any{"name": "turn-1", "task": "t"})
	wsID := w["ws_id"].(string)
	dir := w["dir"].(string)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lr := h.mustOK("ws_land", map[string]any{
		"ws_id": wsID, "drop_workspace": true, "idempotency_key": "land-1",
	})
	vid := lr["version_id"].(string)
	if vid == "" {
		t.Fatalf("land=%v", lr)
	}

	info := h.mustOK("ws_info", map[string]any{"ws_id": wsID})
	if info["already_landed"] != true || info["version_id"] != vid {
		t.Fatalf("ws_info=%v", info)
	}
	sy := h.mustOK("ws_sync", map[string]any{"ws_id": wsID})
	if sy["already_landed"] != true || sy["state"] != "already_landed" {
		t.Fatalf("ws_sync=%v", sy)
	}
	pv := h.mustOK("land_preview", map[string]any{"ws_id": wsID})
	if pv["already_landed"] != true {
		t.Fatalf("land_preview=%v", pv)
	}
	again := h.mustOK("ws_land", map[string]any{
		"ws_id": wsID, "drop_workspace": true, "idempotency_key": "land-1",
	})
	if again["already_landed"] != true || again["version_id"] != vid {
		t.Fatalf("ws_land replay=%v", again)
	}
	if again["durable"] != false {
		t.Fatalf("ws_land 默认不应声明远端 durable: %v", again["durability"])
	}
	if d, _ := again["durability"].(map[string]any); d == nil || d["local_committed"] != true {
		t.Fatalf("durability=%v", again["durability"])
	}

	// 同一 key 换 ws_id 必须显式冲突。
	w2 := h.mustOK("ws_open", map[string]any{"name": "turn-2", "task": "t2"})
	h.mustFail("ws_land", "idempotency_conflict", map[string]any{
		"ws_id": w2["ws_id"].(string), "idempotency_key": "land-1",
	})

	// land + drop 会释放 name claim；同名 ensure 用新 key 应创建新一代工作区，
	// 旧 ws_id 仍返回 already_landed。
	w3 := h.mustOK("ws_ensure", map[string]any{
		"name": "turn-1", "idempotency_key": "land-1/next-turn",
	})
	newID := w3["ws_id"].(string)
	if newID == wsID || w3["created"] != true {
		t.Fatalf("land 后同名 ensure=%v old=%s", w3, wsID)
	}
}

func TestWSGoneAbandonedAndTombstoneList(t *testing.T) {
	h := newHarnessOpt(t, repo.Options{Materialize: mountmgr.ModeAuto,
		TombstoneTTL: 2 * time.Second})
	h.seed("a.txt", "A\n")
	if _, err := h.r.Import(h.srcDir); err != nil {
		t.Fatal(err)
	}
	w := h.mustOK("ws_ensure", map[string]any{"name": "gone-1"})
	wsID := w["ws_id"].(string)
	h.mustOK("ws_drop", map[string]any{"ws_id": wsID})

	e := h.mustFail("ws_info", "ws_gone", map[string]any{"ws_id": wsID})
	detail := e["detail"].(map[string]any)
	for _, k := range []string{"repo", "ws_id", "name", "reason", "dropped_at"} {
		if detail[k] == nil || detail[k] == "" {
			t.Fatalf("ws_gone detail 缺少 %s: %v", k, detail)
		}
	}
	if detail["reason"] != "abandoned" {
		t.Fatalf("reason=%v", detail["reason"])
	}
	h.mustFail("ws_sync", "ws_gone", map[string]any{"ws_id": wsID})
	h.mustFail("land_preview", "ws_gone", map[string]any{"ws_id": wsID})

	list := h.mustOK("ws_list", map[string]any{"include_dropped": true})["_array"].([]any)
	found := false
	for _, it := range list {
		m := it.(map[string]any)
		if m["ws_id"] == wsID {
			found = true
			if m["state"] != "tombstone" || m["reason"] != "abandoned" {
				t.Fatalf("tombstone=%v", m)
			}
		}
	}
	if !found {
		t.Fatalf("include_dropped 未返回 tombstone: %v", list)
	}
	time.Sleep(2200 * time.Millisecond)
	list = h.mustOK("ws_list", map[string]any{"include_dropped": true})["_array"].([]any)
	for _, it := range list {
		if it.(map[string]any)["ws_id"] == wsID {
			t.Fatalf("过期 tombstone 仍然出现: %v", it)
		}
	}
}

func TestWSDiffAlreadyLandedAndRepoHealth(t *testing.T) {
	h := newHarness(t)
	h.seed("a.txt", "A\n")
	if _, err := h.r.Import(h.srcDir); err != nil {
		t.Fatal(err)
	}
	w := h.mustOK("ws_ensure", map[string]any{"name": "diff-health"})
	dir := w["dir"].(string)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.mustOK("ws_land", map[string]any{"ws_id": w["ws_id"].(string)})

	d := h.mustOK("ws_diff", map[string]any{"ws_id": w["ws_id"].(string)})
	if d["already_landed"] != true || d["state"] != "already_landed" {
		t.Fatalf("ws_diff=%v", d)
	}
	if items, _ := d["items"].([]any); items == nil || len(items) != 0 {
		t.Fatalf("ws_diff items=%v", d["items"])
	}

	hh := h.mustOK("repo_health", map[string]any{})
	for _, k := range []string{"repo", "p37_version", "format_ver", "persistence",
		"quota", "workspaces", "lines", "warnings"} {
		if hh[k] == nil {
			t.Fatalf("repo_health 缺少 %s: %v", k, hh)
		}
	}
	if hh["p37_version"] == "" {
		t.Fatalf("p37_version 为空")
	}
	if _, ok := hh["persistence"].(map[string]any)["pending_packages"]; !ok {
		t.Fatalf("persistence 缺少 pending_packages: %v", hh["persistence"])
	}
}

func TestWSLandDurability(t *testing.T) {
	h := newHarness(t)
	h.seed("a.txt", "A\n")
	if _, err := h.r.Import(h.srcDir); err != nil {
		t.Fatal(err)
	}
	w := h.mustOK("ws_open", map[string]any{"task": "d"})
	dir := w["dir"].(string)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := h.mustOK("ws_land", map[string]any{"ws_id": w["ws_id"].(string)})
	if res["durable"] != false {
		t.Fatalf("durable=%v", res["durable"])
	}
	d, _ := res["durability"].(map[string]any)
	if d == nil || d["truth_source"] != "local_repo" || d["local_committed"] != true ||
		d["offsite_durable"] != false {
		t.Fatalf("durability=%v", res["durability"])
	}
}

func TestWSPersistenceReopen(t *testing.T) {
	h := newHarness(t)
	h.seed("a.txt", "A\n")
	if _, err := h.r.Import(h.srcDir); err != nil {
		t.Fatal(err)
	}
	w := h.mustOK("ws_ensure", map[string]any{"name": "persist-1"})
	dir := w["dir"].(string)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lr := h.mustOK("ws_land", map[string]any{"ws_id": w["ws_id"].(string)})
	vid := lr["version_id"].(string)
	if err := h.r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := repo.Open(h.root, repo.Options{Materialize: mountmgr.ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r2.Close() }()
	if _, err := r2.DB.GetVersion(mustParse(t, vid)); err != nil {
		t.Fatalf("reopen 后版本丢失: %v", err)
	}
	lines, err := r2.Lines()
	if err != nil || len(lines) == 0 {
		t.Fatalf("reopen 后 line 丢失: %v %v", lines, err)
	}
	if head, err := r2.DB.LineHead("main"); err != nil || head.String() != vid {
		t.Fatalf("reopen 后 head=%v err=%v want %s", head, err, vid)
	}
}

func TestWSQuotaAndReapObservability(t *testing.T) {
	h := newHarness(t)
	h.seed("a.txt", "A\n")
	if _, err := h.r.Import(h.srcDir); err != nil {
		t.Fatal(err)
	}
	w := h.mustOK("ws_ensure", map[string]any{"name": "quota-1"})
	wsID := w["ws_id"].(string)
	q := h.mustOK("ws_quota", map[string]any{})
	items, _ := q["workspaces"].([]any)
	if len(items) != 1 {
		t.Fatalf("quota workspaces=%v", q)
	}
	item := items[0].(map[string]any)
	if item["ws_id"] != wsID || item["name"] != "quota-1" || item["dirty"] != false {
		t.Fatalf("quota item=%v", item)
	}
	// 空 active 工作区不应被自动回收。
	reap := h.mustOK("ws_reap", map[string]any{"dry_run": true})
	if reap["count"].(float64) != 0 {
		t.Fatalf("reap count=%v", reap)
	}
	// 落地但保留只读工作区，dry-run 应给出可解释的 candidate。
	if err := os.WriteFile(filepath.Join(w["dir"].(string), "a.txt"), []byte("B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.mustOK("ws_sync", map[string]any{"ws_id": wsID})
	h.mustOK("ws_land", map[string]any{"ws_id": wsID, "drop_workspace": false})
	reap = h.mustOK("ws_reap", map[string]any{"dry_run": true})
	cands, _ := reap["candidates"].([]any)
	if len(cands) != 1 {
		t.Fatalf("reap candidates=%v", reap)
	}
	c0 := cands[0].(map[string]any)
	if c0["ws_id"] != wsID || c0["reason"] != "landed_and_dropped" ||
		c0["line"] != "main" || c0["line_head"] == "" {
		t.Fatalf("candidate=%v", c0)
	}
	// 显式 drop 后，landed/dropped 记录可被解释。
	h.mustOK("ws_drop", map[string]any{"ws_id": wsID})
	list := h.mustOK("ws_list", map[string]any{"include_dropped": true})["_array"].([]any)
	var found bool
	for _, it := range list {
		if it.(map[string]any)["ws_id"] == wsID {
			found = true
			if it.(map[string]any)["reason"] != "landed_and_dropped" {
				t.Fatalf("tombstone=%v", it)
			}
		}
	}
	if !found {
		t.Fatal("drop 后 tombstone 不可见")
	}
}

func TestWSReapNamePrefix(t *testing.T) {
	h := newHarness(t)
	h.seed("a.txt", "A\n")
	if _, err := h.r.Import(h.srcDir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"p34s-a", "p34s-b"} {
		w := h.mustOK("ws_ensure", map[string]any{"name": name})
		dir := w["dir"].(string)
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		h.mustOK("ws_sync", map[string]any{"ws_id": w["ws_id"].(string)})
		h.mustOK("ws_land", map[string]any{"ws_id": w["ws_id"].(string), "drop_workspace": false})
	}
	res := h.mustOK("ws_reap", map[string]any{"dry_run": true, "name_prefix": "p34s-a"})
	cands, _ := res["candidates"].([]any)
	if len(cands) != 1 || cands[0].(map[string]any)["name"] != "p34s-a" {
		t.Fatalf("name_prefix candidates=%v", res)
	}
}

func TestWSErrorContext(t *testing.T) {
	h := newHarnessOpt(t, repo.Options{Materialize: mountmgr.ModeAuto, MaxActive: 1})
	h.seed("a.txt", "A\n")
	if _, err := h.r.Import(h.srcDir); err != nil {
		t.Fatal(err)
	}
	e := h.mustFail("ws_info", "ws_not_found", map[string]any{"ws_id": "ws_nope"})
	detail := e["detail"].(map[string]any)
	for _, k := range []string{"repo", "ws_id", "reason"} {
		if detail[k] == nil || detail[k] == "" {
			t.Fatalf("ws_not_found detail 缺少 %s: %v", k, detail)
		}
	}
	e = h.mustFail("line_info", "line_not_found", map[string]any{"name": "no-such-line"})
	detail = e["detail"].(map[string]any)
	for _, k := range []string{"repo", "name", "reason"} {
		if detail[k] == nil || detail[k] == "" {
			t.Fatalf("line_not_found detail 缺少 %s: %v", k, detail)
		}
	}
	h.mustOK("ws_open", map[string]any{"task": "one"})
	e = h.mustFail("ws_open", "quota_exceeded", map[string]any{"task": "two"})
	detail = e["detail"].(map[string]any)
	for _, k := range []string{"repo", "reason", "scope", "used", "limit",
		"retryable", "retry_after_ms", "next_action"} {
		if detail[k] == nil {
			t.Fatalf("quota detail 缺少 %s: %v", k, detail)
		}
	}
	if detail["retryable"] != true || detail["next_action"] != "retry_or_reclaim" {
		t.Fatalf("quota retry 语义=%v", detail)
	}
}
