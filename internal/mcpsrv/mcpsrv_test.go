package mcpsrv_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/mcpsrv"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/repo"
)

// harness 起一个进程内 MCP 客户端-服务器对。
type harness struct {
	t      *testing.T
	cs     *mcp.ClientSession
	r      *repo.Repo
	root   string
	srcDir string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessOpt(t, repo.Options{Materialize: mountmgr.ModeAuto})
}

// newHarnessOpt 允许测试自定义仓库上限（例如把 max_active 调到 1 来验证配额 detail）。
func newHarnessOpt(t *testing.T, opts repo.Options) *harness {
	t.Helper()
	root := t.TempDir()
	src := t.TempDir()
	r, err := repo.Init(root, opts)
	if err != nil {
		t.Fatal(err)
	}

	srv := mcpsrv.New(r)
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
	t.Cleanup(func() {
		_ = cs.Close()
		_ = r.Close()
	})
	return &harness{t: t, cs: cs, r: r, root: root, srcDir: src}
}

// call 调用一个工具并返回解码后的信封。
func (h *harness) call(name string, args any) map[string]any {
	h.t.Helper()
	res, err := h.cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		h.t.Fatalf("调用 %s 失败: %v", name, err)
	}
	if len(res.Content) == 0 {
		h.t.Fatalf("%s 返回空内容", name)
	}
	txt, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		h.t.Fatalf("%s 返回非文本内容", name)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(txt.Text), &env); err != nil {
		h.t.Fatalf("%s 返回非法 JSON: %v\n%s", name, err, txt.Text)
	}
	return env
}

// mustOK 断言调用成功并返回 data。
func (h *harness) mustOK(name string, args any) map[string]any {
	h.t.Helper()
	env := h.call(name, args)
	if ok, _ := env["ok"].(bool); !ok {
		h.t.Fatalf("%s 失败: %v", name, env["error"])
	}
	data, _ := env["data"].(map[string]any)
	if data == nil {
		if arr, ok := env["data"].([]any); ok {
			return map[string]any{"_array": arr}
		}
	}
	return data
}

func (h *harness) mustFail(name, code string, args any) map[string]any {
	h.t.Helper()
	env := h.call(name, args)
	if ok, _ := env["ok"].(bool); ok {
		h.t.Fatalf("%s 本应失败，却成功了", name)
	}
	e, _ := env["error"].(map[string]any)
	got, _ := e["code"].(string)
	if got != code {
		h.t.Fatalf("%s 错误码 = %q，期望 %q（%v）", name, got, code, e)
	}
	return e
}

func (h *harness) seed(rel, content string) {
	h.t.Helper()
	p := filepath.Join(h.srcDir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// TestToolsListed 断言工具面被正确暴露。
func TestToolsListed(t *testing.T) {
	h := newHarness(t)
	res, err := h.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"repo_status", "version_log", "version_show", "version_diff",
		"lease_query", "blame",
		"ws_open", "ws_list", "ws_info", "ws_lease", "ws_sync", "ws_drop", "ws_rebase",
		"ws_quota",
		"conflict_list", "conflict_show", "conflict_resolve", "conflict_resolve_all",
		"land_preview", "ws_land", "version_revert",
		"sync_status", "sync_prepare", "sync_list", "sync_show",
	}
	got := map[string]bool{}
	for _, tl := range res.Tools {
		got[tl.Name] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("工具 %s 未注册", w)
		}
	}
	if len(res.Tools) < len(want) {
		t.Errorf("工具数 = %d，少于期望 %d", len(res.Tools), len(want))
	}
}

// TestEndToEnd 走一遍 MCP 层的完整闭环。
func TestEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.seed("a.txt", "l0\nl1\nl2\n")
	h.seed("src/x.go", "package x\n")
	if _, err := h.r.Import(h.srcDir); err != nil {
		t.Fatal(err)
	}

	// blame（§12.2）：导入版本的每一行都必须能归因。
	bl := h.mustOK("blame", map[string]any{"path": "a.txt"})
	blRecs, _ := bl["records"].([]any)
	if len(blRecs) == 0 {
		t.Fatalf("blame 记录为空: %v", bl)
	}
	blFirst, _ := blRecs[0].(map[string]any)
	if blFirst["version_id"] == nil || blFirst["range"] == nil {
		t.Fatalf("blame 记录缺少 version_id/range: %v", blFirst)
	}

	// repo_status
	st := h.mustOK("repo_status", map[string]any{})
	repoPart, _ := st["repo"].(map[string]any)
	if repoPart["version_count"].(float64) != 1 {
		t.Fatalf("version_count = %v", repoPart["version_count"])
	}

	// ws_open
	wo := h.mustOK("ws_open", map[string]any{"task_desc": "实现登录接口"})
	wsID, _ := wo["ws_id"].(string)
	dir, _ := wo["dir"].(string)
	if wsID == "" || dir == "" {
		t.Fatalf("ws_open 返回不完整: %v", wo)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("工作区目录不存在: %v", err)
	}

	// 在目录里直接改文件（agent 的正常工作方式）
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("l0\nL1\nl2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("brand new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// ws_sync
	sy := h.mustOK("ws_sync", map[string]any{"ws_id": wsID})
	if sy["captured_ops"].(float64) < 2 {
		t.Fatalf("captured_ops = %v", sy["captured_ops"])
	}

	// land_preview 必须可落地
	pv := h.mustOK("land_preview", map[string]any{"ws_id": wsID})
	if !pv["can_land"].(bool) {
		t.Fatalf("can_land = false: %v", pv["conflicts"])
	}

	// ws_land
	lr := h.mustOK("ws_land", map[string]any{"ws_id": wsID})
	vid, _ := lr["version_id"].(string)
	if vid == "" {
		t.Fatalf("ws_land 未返回 version_id: %v", lr)
	}

	// 版本应出现在 log 中
	env := h.call("version_log", map[string]any{"limit": 10})
	arr, _ := env["data"].([]any)
	if len(arr) < 2 {
		t.Fatalf("version_log 只有 %d 条", len(arr))
	}

	// ws_info 应报不存在
	h.mustFail("ws_info", "ws_not_found", map[string]any{"ws_id": wsID})
}

// TestConflictOverMCP 覆盖冲突经 MCP 暴露与解决（§14.3.2）。
func TestConflictOverMCP(t *testing.T) {
	h := newHarness(t)
	h.seed("a.txt", "l0\nl1\nl2\nl3\nl4\nl5\n")
	if _, err := h.r.Import(h.srcDir); err != nil {
		t.Fatal(err)
	}
	baseHead, _, _ := h.r.Head()

	// A 落地。
	wa := h.mustOK("ws_open", map[string]any{
		"task_desc": "任务A", "parent": baseHead.String()})
	waID, _ := wa["ws_id"].(string)
	dirA, _ := wa["dir"].(string)
	if err := os.WriteFile(filepath.Join(dirA, "a.txt"), []byte("l0\nAAA\nl2\nl3\nl4\nl5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.mustOK("ws_land", map[string]any{"ws_id": waID})

	// B 从同一基线改同一行。
	wb := h.mustOK("ws_open", map[string]any{
		"task_desc": "任务B", "parent": baseHead.String()})
	wbID, _ := wb["ws_id"].(string)
	dirB, _ := wb["dir"].(string)
	if err := os.WriteFile(filepath.Join(dirB, "a.txt"), []byte("l0\nBBB\nl2\nl3\nl4\nl5\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 预演应给出冲突（且带 key）。
	pv := h.mustOK("land_preview", map[string]any{"ws_id": wbID})
	if pv["can_land"].(bool) {
		t.Fatal("应检测到冲突")
	}
	confs, _ := pv["conflicts"].([]any)
	if len(confs) == 0 {
		t.Fatal("conflicts 为空")
	}
	c0, _ := confs[0].(map[string]any)
	key, _ := c0["key"].(string)
	if key == "" {
		t.Fatalf("冲突缺少 key: %v", c0)
	}
	lower, _ := c0["lower"].(map[string]any)
	if lower == nil {
		t.Fatal("冲突缺少 lower 侧")
	}

	// 直接 resolve（不需要先失败一次，D22）。
	h.mustOK("conflict_resolve", map[string]any{
		"key": key, "resolution": map[string]any{"kind": "take_upper"}})

	// 现在应能落地。
	lr := h.mustOK("ws_land", map[string]any{"ws_id": wbID})
	if lr["version_id"] == nil {
		t.Fatalf("解决后仍无法落地: %v", lr)
	}

	// 内容应是 B 的版本。
	v, _ := h.r.DB.GetVersion(mustParse(t, lr["version_id"].(string)))
	tr, _ := h.r.Store.GetTree(v.Tree)
	fc, _ := tr.Get("a.txt")
	data, _ := h.r.Store.ReadFileData(fc.ID)
	if string(data) != "l0\nBBB\nl2\nl3\nl4\nl5\n" {
		t.Fatalf("take_upper 后内容 = %q", data)
	}
}

// TestRevertOverMCP 覆盖 version_revert。
func TestRevertOverMCP(t *testing.T) {
	h := newHarness(t)
	h.seed("a.txt", "v0\n")
	if _, err := h.r.Import(h.srcDir); err != nil {
		t.Fatal(err)
	}
	head0, _, _ := h.r.Head()

	w := h.mustOK("ws_open", map[string]any{"task_desc": "改一版"})
	wsID, _ := w["ws_id"].(string)
	dir, _ := w["dir"].(string)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lr := h.mustOK("ws_land", map[string]any{"ws_id": wsID})
	v1, _ := lr["version_id"].(string)

	rv := h.mustOK("version_revert", map[string]any{"version_id": v1})
	if rv["state"] != "done" {
		t.Fatalf("revert 状态 = %v", rv)
	}

	// 被 revert 的版本仍在历史中。
	head, _, _ := h.r.Head()
	if head.String() == v1 {
		t.Fatal("head 未推进")
	}
	env := h.call("version_show", map[string]any{"version_id": head0.String()})
	if ok, _ := env["ok"].(bool); !ok {
		t.Fatalf("原版本应仍可读取: %v", env["error"])
	}
}

// TestToolsErrorPaths 覆盖错误码映射（附录 E）。
func TestToolsErrorPaths(t *testing.T) {
	h := newHarness(t)

	// 不存在的工作区。
	e := h.mustFail("ws_info", "ws_not_found", map[string]any{"ws_id": "ws_nope"})
	if msg, _ := e["message"].(string); !strings.Contains(msg, "ws_nope") {
		t.Errorf("错误消息未包含 id: %q", msg)
	}

	// 不存在的版本。
	h.mustFail("version_show", "version_not_found",
		map[string]any{"version_id": "0000000000000000000000000000000000000000000000000000000000000000"})

	// 非法 key。
	h.mustFail("conflict_show", "internal", map[string]any{"key": "not-a-key"})

	// blame 对不存在的路径必须返回结构化错误码而不是 panic。
	env := h.call("blame", map[string]any{"path": "definitely-missing.txt"})
	if ok, _ := env["ok"].(bool); ok {
		t.Error("blame 不存在的路径应失败")
	}
	if e, _ := env["error"].(map[string]any); e == nil || e["code"] != "path_not_found" {
		t.Errorf("blame 错误码 = %v，期望 PATH_NOT_FOUND", env["error"])
	}

	// 未知参数应被忽略而不是报错（附录 K.3）。
	env = h.call("sync_status", map[string]any{"unknown_param": 1})
	if ok, _ := env["ok"].(bool); !ok {
		t.Errorf("未知参数应被忽略: %v", env["error"])
	}
}

func mustParse(t *testing.T, s string) hash.ID {
	t.Helper()
	id, err := hash.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
