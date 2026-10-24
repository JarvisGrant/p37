package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/hub"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/repo"
)

func makeExternalRepo(t *testing.T, path, name string) string {
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
	b, _ := json.Marshal(map[string]string{"id": id, "name": name, "default_line": "main"})
	if err := os.WriteFile(filepath.Join(path, ".p37", "repo.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCLIRepoLifecycleDirect(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "app")
	if err := os.MkdirAll(app, 0o750); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runCLI("--root", root, "repo", "register", "app", app,
		"--init-if-missing", "--json")
	if code != 0 {
		t.Fatalf("register: %d %s", code, errs)
	}
	var e hub.Entry
	if err := json.Unmarshal([]byte(out), &e); err != nil {
		t.Fatalf("register json: %v\n%s", err, out)
	}
	if e.Name != "app" || e.ID == "" || !e.Available {
		t.Fatalf("register=%+v", e)
	}
	if _, err := os.Stat(filepath.Join(app, ".p37", "repo.json")); err != nil {
		t.Fatal(err)
	}

	// registry 非空时 scan 仍能发现新 repo。
	app2 := filepath.Join(root, "app2")
	prepareExternalForCLI(t, app2, "app2")
	code, out, errs = runCLI("--root", root, "repo", "scan", "--json")
	if code != 0 {
		t.Fatalf("scan: %d %s", code, errs)
	}
	var sr hub.ScanResult
	if err := json.Unmarshal([]byte(out), &sr); err != nil {
		t.Fatalf("scan json: %v\n%s", err, out)
	}
	if len(sr.Added) != 1 || sr.Added[0].Name != "app2" {
		t.Fatalf("scan=%+v", sr)
	}
	code, out, errs = runCLI("--root", root, "repo", "scan", "--json")
	if code != 0 || !strings.Contains(out, `"added": []`) {
		t.Fatalf("scan 幂等: %d %s %s", code, out, errs)
	}

	code, out, errs = runCLI("--root", root, "repo", "list", "--json")
	if code != 0 || !strings.Contains(out, `"app2"`) {
		t.Fatalf("list: %d %s %s", code, out, errs)
	}
	code, out, errs = runCLI("--root", root, "repo", "unregister", "app", "--json")
	if code != 0 || !strings.Contains(out, `"unregistered": true`) {
		t.Fatalf("unregister: %d %s %s", code, out, errs)
	}
	if _, err := os.Stat(filepath.Join(app, ".p37", "p37.db")); err != nil {
		t.Fatalf("unregister 不应删除数据: %v", err)
	}
	code, out, errs = runCLI("--root", root, "repo", "list", "--json")
	if code != 0 || strings.Contains(out, `"name": "app"`) {
		t.Fatalf("unregister 后列表: %d %s %s", code, out, errs)
	}

	// 不存在时幂等。
	code, out, errs = runCLI("--root", root, "repo", "unregister", "missing", "--json")
	if code != 0 || !strings.Contains(out, `"already_unregistered": true`) {
		t.Fatalf("idempotent unregister: %d %s %s", code, out, errs)
	}
}

func prepareExternalForCLI(t *testing.T, path, name string) {
	t.Helper()
	_ = makeExternalRepo(t, path, name)
}

func TestCLIRepoLifecycleRoutesToDaemon(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "p37cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.MkdirAll(filepath.Join(root, ".p37"), 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(root, ".p37", "p37d.sock")
	stop := startCannedDaemon(t, sock)
	defer stop()

	path := filepath.Join(root, "app")
	code, out, errs := runCLI("--root", root, "repo", "register", "app", path, "--json")
	if code != 0 {
		t.Fatalf("routed register: %d %s", code, errs)
	}
	if !strings.Contains(out, "routed-name") {
		t.Fatalf("register 未走 daemon: %s", out)
	}
	code, out, errs = runCLI("--root", root, "repo", "scan", "--json")
	if code != 0 || !strings.Contains(out, `"routed_scan": true`) {
		t.Fatalf("scan 未走 daemon: %d %s %s", code, out, errs)
	}
	code, out, errs = runCLI("--root", root, "repo", "unregister", "app", "--json")
	if code != 0 || !strings.Contains(out, `"routed_unregister": true`) {
		t.Fatalf("unregister 未走 daemon: %d %s %s", code, out, errs)
	}

	// daemon 没有写本地 registry；如果 CLI 绕过 daemon 就会有行。
	h, err := hub.New(root, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	if es, _ := h.List(); len(es) != 0 {
		t.Fatalf("CLI 不应绕过 daemon 写 registry: %+v", es)
	}
}

func startCannedDaemon(t *testing.T, sock string) func() {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "canned", Version: "0"}, nil)
	ok := func(data any) *mcp.CallToolResult {
		b, _ := json.Marshal(map[string]any{"ok": true, "data": data, "error": nil})
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(b)}},
		}
	}
	srv.AddTool(&mcp.Tool{Name: "repo_register", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return ok(map[string]any{
				"id": "repo_routed", "name": "routed-name", "path": "/routed", "default_line": "main",
				"mounted": true, "available": true, "degraded": false,
			}), nil
		})
	srv.AddTool(&mcp.Tool{Name: "repo_unregister", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return ok(map[string]any{"unregistered": true, "already_unregistered": false,
				"routed_unregister": true, "id": "repo_routed", "name": "routed-name"}), nil
		})
	srv.AddTool(&mcp.Tool{Name: "repo_scan", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return ok(map[string]any{"added": []any{}, "removed": []any{}, "degraded": []any{},
				"skipped": []any{}, "reconciled": true, "dry_run": false, "routed_scan": true}), nil
		})
	hs := &http.Server{Handler: mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)}
	go func() { _ = hs.Serve(ln) }()
	return func() {
		_ = hs.Close()
		_ = ln.Close()
	}
}
