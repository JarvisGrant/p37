package mcpstdio_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chenyiwen/p37/internal/mcpsrv"
	"github.com/chenyiwen/p37/internal/mcpstdio"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/repo"
)

func startDaemon(t *testing.T) (root, sock string) {
	t.Helper()
	root = t.TempDir()
	r, err := repo.Init(root, repo.Options{Materialize: mountmgr.ModeCopy})
	if err != nil {
		t.Fatal(err)
	}
	// macOS 的 unix socket 路径上限约 104 字节，t.TempDir() 通常超限；
	// 用一个短路径专门放 socket。
	sockDir, err := os.MkdirTemp("/tmp", "p37sock-")
	if err != nil {
		_ = r.Close()
		t.Fatal(err)
	}
	sock = filepath.Join(sockDir, "p37d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		_ = r.Close()
		t.Fatal(err)
	}
	hs := &http.Server{Handler: mcpsrv.New(r).Handler()}
	go func() { _ = hs.Serve(ln) }()
	t.Cleanup(func() {
		_ = hs.Close()
		_ = ln.Close()
		_ = r.Close()
		_ = os.RemoveAll(sockDir)
	})
	return root, sock
}

func startProxy(t *testing.T, sock string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := mcpstdio.Run(ctx, sock, inR, outW, os.Stderr, mcpstdio.Options{
			ConnectTimeout: 5 * time.Second,
			RetryInterval:  50 * time.Millisecond,
		})
		_ = outW.Close()
		done <- err
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "stdio-test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		cancel()
		t.Fatalf("连接 stdio 代理失败: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		cancel()
		_ = inW.Close()
		_ = inR.Close()
		_ = outR.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})
	return cs
}

func callTool(ctx context.Context, cs *mcp.ClientSession, name string, args any) (map[string]any, error) {
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, err
	}
	if len(res.Content) == 0 {
		return nil, fmt.Errorf("%s 返回空内容", name)
	}
	txt, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		return nil, fmt.Errorf("%s 返回非文本内容", name)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(txt.Text), &env); err != nil {
		return nil, fmt.Errorf("%s 返回非法 JSON: %w", name, err)
	}
	return env, nil
}

func mustOK(t *testing.T, ctx context.Context, cs *mcp.ClientSession, name string, args any) map[string]any {
	t.Helper()
	env, err := callTool(ctx, cs, name, args)
	if err != nil {
		t.Fatalf("调用 %s 失败: %v", name, err)
	}
	if ok, _ := env["ok"].(bool); !ok {
		t.Fatalf("%s 失败: %v", name, env["error"])
	}
	data, _ := env["data"].(map[string]any)
	return data
}

// TestProxyTwoClientsIndependent 覆盖两个 stdio 客户端并发走完整
// ws_open → ws_sync → ws_land，并确认它们拿到不同工作区。
func TestProxyTwoClientsIndependent(t *testing.T) {
	_, sock := startDaemon(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	csA := startProxy(t, sock)
	csB := startProxy(t, sock)

	run := func(cs *mcp.ClientSession, task, file string) (string, error) {
		env, err := callTool(ctx, cs, "ws_open", map[string]any{"task_desc": task})
		if err != nil {
			return "", err
		}
		if ok, _ := env["ok"].(bool); !ok {
			return "", fmt.Errorf("ws_open %s: %v", task, env["error"])
		}
		data, _ := env["data"].(map[string]any)
		wsID, _ := data["ws_id"].(string)
		dir, _ := data["dir"].(string)
		if wsID == "" || dir == "" {
			return "", fmt.Errorf("ws_open %s 返回不完整: %v", task, data)
		}
		if err := os.WriteFile(filepath.Join(dir, file), []byte(task+"\n"), 0o644); err != nil {
			return "", err
		}
		for _, step := range []string{"ws_sync", "ws_land"} {
			e, err := callTool(ctx, cs, step, map[string]any{"ws_id": wsID})
			if err != nil {
				return "", err
			}
			if ok, _ := e["ok"].(bool); !ok {
				return "", fmt.Errorf("%s %s: %v", step, wsID, e["error"])
			}
		}
		return wsID, nil
	}

	type result struct {
		id  string
		err error
	}
	ch := make(chan result, 2)
	go func() {
		id, err := run(csA, "agent-a", "a.txt")
		ch <- result{id, err}
	}()
	go func() {
		id, err := run(csB, "agent-b", "b.txt")
		ch <- result{id, err}
	}()

	var ids []string
	for i := 0; i < 2; i++ {
		r := <-ch
		if r.err != nil {
			t.Fatalf("客户端失败: %v", r.err)
		}
		ids = append(ids, r.id)
	}
	if ids[0] == ids[1] {
		t.Fatalf("两个客户端拿到了同一个 session/工作区: %s", ids[0])
	}

	env, err := callTool(ctx, csA, "version_log", map[string]any{"limit": 10})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := env["ok"].(bool); !ok {
		t.Fatalf("version_log 失败: %v", env["error"])
	}
	versions, _ := env["data"].([]any)
	if len(versions) < 3 {
		t.Fatalf("版本数 = %d，期望至少 3（init + 两次 land）", len(versions))
	}
}

// TestProxyToolNamesMatchDaemon 确认代理没有改名/漏工具。
func TestProxyToolNamesMatchDaemon(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, sock := startDaemon(t)
	cs := startProxy(t, sock)

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tool := range res.Tools {
		got = append(got, tool.Name)
	}
	sort.Strings(got)

	// 与 internal/mcpsrv 的工具面保持一致；这里只钉住关键工具，避免重复
	// 维护完整清单。
	has := map[string]bool{}
	for _, name := range got {
		has[name] = true
	}
	for _, want := range []string{
		"ws_open", "ws_sync", "ws_land", "version_log", "repo_status", "sync_status",
	} {
		if !has[want] {
			t.Errorf("代理缺少工具 %s（实际 %v）", want, got)
		}
	}
}

// TestProxyDaemonAbsent 确认 daemon 不在时不会无限等待、不会污染 stdout。
func TestProxyDaemonAbsent(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "missing.sock")
	var stdout strings.Builder
	var stderr strings.Builder

	start := time.Now()
	err := mcpstdio.Run(context.Background(), sock, strings.NewReader(""),
		&stdout, &stderr, mcpstdio.Options{
			ConnectTimeout: 300 * time.Millisecond,
			RetryInterval:  50 * time.Millisecond,
		})
	took := time.Since(start)

	if err == nil {
		t.Fatal("daemon 不在时应返回错误")
	}
	if !strings.Contains(err.Error(), "不会自动启动 daemon") {
		t.Fatalf("错误信息不清楚: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout 不应有输出，得到 %q", stdout.String())
	}
	if took > 3*time.Second {
		t.Fatalf("重试耗时 %s，超过合理上限", took)
	}
}
