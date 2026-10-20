//go:build p36acceptance

package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type toolEnvelope struct {
	OK    bool           `json:"ok"`
	Data  map[string]any `json:"data"`
	Error any            `json:"error"`
}

type agentResult struct {
	Task        string `json:"task"`
	WSID        string `json:"ws_id"`
	Dir         string `json:"dir"`
	Land        string `json:"land"`
	Materialize string `json:"materialize"`
}

func repoRoot() string {
	if v := os.Getenv("P37_REPO"); v != "" {
		return v
	}
	return "/srv/repo"
}

func repoName() string { return os.Getenv("P37_REPO_NAME") }

func mcpPath() string {
	if v := os.Getenv("P37_MCP"); v != "" {
		return v
	}
	return "/usr/local/bin/p37-mcp"
}

func startClient(t *testing.T, ctx context.Context) *mcp.ClientSession {
	t.Helper()
	cmd := exec.Command(mcpPath())
	env := append(os.Environ(),
		"P37_REPO="+repoRoot(),
		"TMPDIR="+filepath.Join(repoRoot(), ".p37", "tmp"),
	)
	if v := os.Getenv("P37_ROOT"); v != "" {
		env = append(env, "P37_ROOT="+v)
	}
	cmd.Env = env
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 %s: %v", mcpPath(), err)
	}
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "p36-acceptance",
		Version: "0",
	}, nil)
	cs, err := client.Connect(ctx, &mcp.IOTransport{Reader: stdout, Writer: stdin}, nil)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("连接 %s: %v", mcpPath(), err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cs
}

func callTool(ctx context.Context, cs *mcp.ClientSession, name string, args any) (*toolEnvelope, error) {
	if r := repoName(); r != "" {
		if m, ok := args.(map[string]any); ok {
			if _, exists := m["repo"]; !exists {
				m["repo"] = r
			}
		}
	}
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
	var env toolEnvelope
	if err := json.Unmarshal([]byte(txt.Text), &env); err != nil {
		return nil, fmt.Errorf("%s 返回非法 JSON: %w", name, err)
	}
	if !env.OK {
		return nil, fmt.Errorf("%s 失败: %v", name, env.Error)
	}
	return &env, nil
}

func runAgent(ctx context.Context, cs *mcp.ClientSession, task, file string) (agentResult, error) {
	env, err := callTool(ctx, cs, "ws_open", map[string]any{"task_desc": task})
	if err != nil {
		return agentResult{}, err
	}
	wsID, _ := env.Data["ws_id"].(string)
	dir, _ := env.Data["dir"].(string)
	materialize, _ := env.Data["materialize"].(string)
	if wsID == "" || dir == "" {
		return agentResult{}, fmt.Errorf("ws_open %s 返回不完整: %v", task, env.Data)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(task+"\n"), 0o644); err != nil {
		return agentResult{}, err
	}
	if _, err := callTool(ctx, cs, "ws_sync", map[string]any{"ws_id": wsID}); err != nil {
		return agentResult{}, err
	}
	land, err := callTool(ctx, cs, "ws_land", map[string]any{"ws_id": wsID})
	if err != nil {
		return agentResult{}, err
	}
	version, _ := land.Data["version_id"].(string)
	return agentResult{Task: task, WSID: wsID, Dir: dir, Land: version, Materialize: materialize}, nil
}

func assertFindmnt(t *testing.T, target, want string) {
	t.Helper()
	out, err := exec.Command("findmnt", "-T", target).CombinedOutput()
	if err != nil {
		t.Fatalf("findmnt -T %s 失败: %v\n%s", target, err, out)
	}
	t.Logf("findmnt -T %s:\n%s", target, out)
	if !strings.Contains(string(out), want) {
		t.Fatalf("findmnt -T %s 不包含 %q:\n%s", target, want, out)
	}
}

// assertWorkspaceMaterialize 接受 auto 的两种结果：
//
//	overlay -> findmnt 必须显示 overlay
//	copy    -> 工作区是 /srv/repo 下的普通目录，findmnt 应显示 ext4
func assertWorkspaceMaterialize(t *testing.T, dir, materialize string) {
	t.Helper()
	switch materialize {
	case "overlay":
		assertFindmnt(t, dir, "overlay")
	case "copy":
		assertFindmnt(t, dir, "ext4")
	default:
		t.Fatalf("未知 materialize=%q（dir=%s）", materialize, dir)
	}
}

func TestP36TwoAgents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	payload := os.Getenv("P37_PAYLOAD_MOUNT")
	if payload == "" {
		if _, err := os.Stat("/usr/local/lib/p37/p37d"); err == nil {
			payload = "/usr/local/lib/p37"
		} else {
			payload = "/run/p37-payload"
		}
	}
	assertFindmnt(t, payload, "ext4")

	csA := startClient(t, ctx)
	csB := startClient(t, ctx)

	type outcome struct {
		res agentResult
		err error
	}
	ch := make(chan outcome, 2)
	go func() {
		r, err := runAgent(ctx, csA, "agent-a", "a.txt")
		ch <- outcome{r, err}
	}()
	go func() {
		r, err := runAgent(ctx, csB, "agent-b", "b.txt")
		ch <- outcome{r, err}
	}()

	var results []agentResult
	for i := 0; i < 2; i++ {
		o := <-ch
		if o.err != nil {
			t.Fatalf("agent 失败: %v", o.err)
		}
		results = append(results, o.res)
	}
	if results[0].WSID == results[1].WSID {
		t.Fatalf("两个 agent 拿到同一个 ws_id: %s", results[0].WSID)
	}

	// ws_land 会焚毁折叠集合并删除工作区目录，所以落地后的目录不能再用来
	// 检查 overlay。额外开一个不落地的 keeper 工作区，供服务外 findmnt 和
	// 重启恢复验收使用。
	keeperEnv, err := callTool(ctx, csA, "ws_open", map[string]any{"task_desc": "acceptance-keeper"})
	if err != nil {
		t.Fatal(err)
	}
	keeperID, _ := keeperEnv.Data["ws_id"].(string)
	keeperDir, _ := keeperEnv.Data["dir"].(string)
	keeperMaterialize, _ := keeperEnv.Data["materialize"].(string)
	if keeperID == "" || keeperDir == "" {
		t.Fatalf("keeper ws_open 返回不完整: %v", keeperEnv.Data)
	}
	assertWorkspaceMaterialize(t, keeperDir, keeperMaterialize)

	// version_log 的 data 是数组，而 toolEnvelope.Data 只适合 object；
	// 这里直接解析原始信封。
	logArgs := map[string]any{"limit": 10}
	if r := repoName(); r != "" {
		logArgs["repo"] = r
	}
	res, err := csA.CallTool(ctx, &mcp.CallToolParams{
		Name:      "version_log",
		Arguments: logArgs,
	})
	if err != nil {
		t.Fatal(err)
	}
	txt := res.Content[0].(*mcp.TextContent).Text
	var raw struct {
		OK   bool  `json:"ok"`
		Data []any `json:"data"`
	}
	if err := json.Unmarshal([]byte(txt), &raw); err != nil {
		t.Fatalf("version_log JSON: %v", err)
	}
	if !raw.OK || len(raw.Data) < 3 {
		t.Fatalf("version_log 版本数 = %d，期望至少 3", len(raw.Data))
	}

	report := struct {
		Agents            []agentResult `json:"agents"`
		Keeper            string        `json:"keeper_ws"`
		KeeperMaterialize string        `json:"keeper_materialize"`
		Version           int           `json:"version_count"`
	}{
		Agents:            results,
		Keeper:            keeperID,
		KeeperMaterialize: keeperMaterialize,
		Version:           len(raw.Data),
	}
	out := os.Getenv("P37_ACCEPTANCE_OUT")
	if out == "" {
		out = filepath.Join(repoRoot(), ".p37", "acceptance.json")
	}
	b, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatalf("写验收结果 %s: %v", out, err)
	}
	wsFile := os.Getenv("P37_ACCEPTANCE_WS")
	if wsFile == "" {
		wsFile = filepath.Join(repoRoot(), ".p37", "acceptance-ws.txt")
	}
	if err := os.WriteFile(wsFile,
		[]byte(keeperID+"\n"+results[0].WSID+"\n"+results[1].WSID+"\n"), 0o644); err != nil {
		t.Fatalf("写 ws 列表: %v", err)
	}
	t.Logf("acceptance: MATERIALIZE=%s keeper=%s agents=%v versions=%d", keeperMaterialize, keeperID, results, len(raw.Data))
}

// TestP36MultiRepoSession 在 P37_ROOT 模式下验证多仓库 + 会话 line 闭环。
// 运行前设置 P37_ROOT=/srv/repos（或测试用临时 root）。
func TestP36MultiRepoSession(t *testing.T) {
	if os.Getenv("P37_ROOT") == "" {
		t.Skip("P37_ROOT 未设置")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cs := startClient(t, ctx)
	name := fmt.Sprintf("accept-%d", time.Now().UnixNano())
	if _, err := callTool(ctx, cs, "repo_create", map[string]any{"name": name}); err != nil {
		t.Fatal(err)
	}
	if _, err := callTool(ctx, cs, "line_create", map[string]any{
		"repo": name, "name": "p34/session/accept", "from": "mainline"}); err != nil {
		t.Fatal(err)
	}
	env, err := callTool(ctx, cs, "ws_open", map[string]any{
		"repo": name, "task": "accept",
		"parent": map[string]any{"kind": "line", "id": "p34/session/accept"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wsID, _ := env.Data["ws_id"].(string)
	dir, _ := env.Data["dir"].(string)
	if wsID == "" || dir == "" {
		t.Fatalf("ws_open: %v", env.Data)
	}
	if err := os.WriteFile(filepath.Join(dir, "accept.txt"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := callTool(ctx, cs, "ws_sync", map[string]any{"repo": name, "ws_id": wsID}); err != nil {
		t.Fatal(err)
	}
	if _, err := callTool(ctx, cs, "ws_land", map[string]any{
		"repo": name, "ws_id": wsID,
		"target": map[string]any{"kind": "line", "id": "p34/session/accept"},
	}); err != nil {
		t.Fatal(err)
	}
	env, err = callTool(ctx, cs, "version_tree", map[string]any{
		"repo": name, "ref": "line:p34/session/accept", "recursive": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	items, _ := env.Data["items"].([]any)
	if len(items) == 0 {
		t.Fatalf("version_tree 为空: %v", env.Data)
	}
}
