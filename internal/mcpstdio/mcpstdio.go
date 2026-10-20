// Package mcpstdio 实现 p37d --stdio 的 MCP 代理。
//
// daemon 的 unix socket 上是 Streamable HTTP（不是裸 JSON-RPC），所以这里
// 不能做字节转发。正确层级是：
//
//	MCP client <--stdio JSON-RPC--> proxy <--HTTP/SSE over unix socket--> daemon
//
// 代理在启动时从 daemon 拉取工具清单，原样注册工具名与 schema；每个
// p37d --stdio 进程各自建立一个 daemon session，因此多客户端天然独立。
package mcpstdio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chenyiwen/p37/internal/buildinfo"
)

// Options 控制代理行为。
type Options struct {
	// ConnectTimeout 是等待 daemon socket 可用的总时长。
	ConnectTimeout time.Duration
	// RetryInterval 是两次连接尝试之间的间隔；0 表示 250ms。
	RetryInterval time.Duration
}

// Run 把 in/out 上的 MCP 流量代理到 socket。
//
// 它只连接、不启动 daemon、不取仓库锁。socket 在 ConnectTimeout 内不可用
// 时返回明确错误；stdout 始终只用于 MCP 协议帧。
func Run(ctx context.Context, socket string, in io.Reader, out io.Writer, errw io.Writer, opt Options) error {
	if socket == "" {
		return errors.New("mcpstdio: 缺少 daemon socket 路径")
	}
	timeout := opt.ConnectTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	retry := opt.RetryInterval
	if retry <= 0 {
		retry = 250 * time.Millisecond
	}

	deadline := time.Now().Add(timeout)
	var lastErr error
	var lastLog time.Time
	var session *mcp.ClientSession

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		sess, err := connectOnce(ctx, socket)
		if err == nil {
			session = sess
			break
		}
		lastErr = err
		if time.Now().After(deadline) {
			break
		}
		if errw != nil && (lastLog.IsZero() || time.Since(lastLog) >= 2*time.Second) {
			fmt.Fprintf(errw, "p37d --stdio: 等待 daemon socket %s（%v）\n", socket, err)
			lastLog = time.Now()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retry):
		}
	}
	if session == nil {
		return fmt.Errorf("mcpstdio: %s 在 %s 内不可用（不会自动启动 daemon）: %w",
			socket, timeout, lastErr)
	}
	defer func() { _ = session.Close() }()

	tools, err := listTools(ctx, session)
	if err != nil {
		return fmt.Errorf("mcpstdio: 读取 daemon 工具列表失败: %w", err)
	}

	srv := mcp.NewServer(&mcp.Implementation{
		Name:        "p37",
		Title:       "p37 AI 原生版本控制",
		Description: "通过 p37d --stdio 代理到同仓库 daemon",
		Version:     buildinfo.Version,
	}, nil)

	for _, t := range tools {
		tool := *t
		srv.AddTool(&tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args any = map[string]any{}
			if req.Params != nil && len(req.Params.Arguments) > 0 {
				// json.RawMessage 原样转发，避免代理层重新解释参数。
				args = req.Params.Arguments
			}
			return session.CallTool(ctx, &mcp.CallToolParams{
				Name:      req.Params.Name,
				Arguments: args,
			})
		})
	}

	transport := &mcp.IOTransport{
		Reader: io.NopCloser(in),
		Writer: nopWriteCloser{out},
	}
	ss, err := srv.Connect(ctx, transport, nil)
	if err != nil {
		return fmt.Errorf("mcpstdio: 建立 stdio 会话失败: %w", err)
	}
	defer func() { _ = ss.Close() }()
	return ss.Wait()
}

func connectOnce(ctx context.Context, socket string) (*mcp.ClientSession, error) {
	// 先用短超时探测 socket，能更快给出“daemon 不在”的可读错误。
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	_ = conn.Close()

	hc := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", socket)
			},
			ForceAttemptHTTP2: false,
		},
	}
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "p37-stdio-proxy",
		Version: buildinfo.Version,
	}, nil)

	attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cs, err := client.Connect(attemptCtx, &mcp.StreamableClientTransport{
		Endpoint:             "http://p37d/mcp",
		HTTPClient:           hc,
		MaxRetries:           -1, // 重试由本包控制，避免 SDK 内部长退避
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		hc.CloseIdleConnections()
		return nil, err
	}
	return cs, nil
}

func listTools(ctx context.Context, cs *mcp.ClientSession) ([]*mcp.Tool, error) {
	var out []*mcp.Tool
	params := &mcp.ListToolsParams{}
	for {
		res, err := cs.ListTools(ctx, params)
		if err != nil {
			return nil, err
		}
		out = append(out, res.Tools...)
		if res.NextCursor == "" {
			return out, nil
		}
		params.Cursor = res.NextCursor
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
