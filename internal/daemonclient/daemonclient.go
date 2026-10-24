// Package daemonclient 通过 unix socket 连接运行中的 p37d MCP 服务。
//
// 只用于 CLI 的运维子命令；业务调用仍应走 MCP 客户端（p37-mcp/stdio）。
package daemonclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chenyiwen/p37/internal/buildinfo"
)

// ToolError 是 MCP 工具信封里的结构化错误。
type ToolError struct {
	Code    string
	Message string
	Detail  any
}

func (e *ToolError) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

// Client 是一个已建立的 MCP 会话。
type Client struct {
	cs *mcp.ClientSession
	hc *http.Client
}

// Connect 连接 unix socket 上的 p37d MCP 服务。
func Connect(ctx context.Context, socket string) (*Client, error) {
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	_ = conn.Close()

	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", socket)
		},
		ForceAttemptHTTP2: false,
	}}
	client := mcp.NewClient(&mcp.Implementation{
		Name: "p37-cli", Version: buildinfo.Version,
	}, nil)
	attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cs, err := client.Connect(attemptCtx, &mcp.StreamableClientTransport{
		Endpoint:             "http://p37d/mcp",
		HTTPClient:           hc,
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		hc.CloseIdleConnections()
		return nil, err
	}
	return &Client{cs: cs, hc: hc}, nil
}

// Call 调用一个 MCP 工具，返回 data 或 *ToolError。
func (c *Client) Call(ctx context.Context, name string, args any) (json.RawMessage, error) {
	if args == nil {
		args = map[string]any{}
	}
	res, err := c.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, err
	}
	var text string
	for _, content := range res.Content {
		if tc, ok := content.(*mcp.TextContent); ok {
			text = tc.Text
			break
		}
	}
	if text == "" {
		return nil, fmt.Errorf("daemonclient: %s 返回空内容", name)
	}
	var env struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Detail  any    `json:"detail"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		return nil, fmt.Errorf("daemonclient: %s 返回非法 JSON: %w", name, err)
	}
	if !env.OK {
		if env.Error == nil {
			return nil, fmt.Errorf("daemonclient: %s 失败（无错误对象）", name)
		}
		return nil, &ToolError{Code: env.Error.Code, Message: env.Error.Message, Detail: env.Error.Detail}
	}
	return env.Data, nil
}

// Close 关闭 MCP 会话与 HTTP 连接。
func (c *Client) Close() {
	if c == nil {
		return
	}
	if c.cs != nil {
		_ = c.cs.Close()
	}
	if c.hc != nil {
		c.hc.CloseIdleConnections()
	}
}
