// Package mcpsrv 把仓库能力暴露为 MCP 工具（§11.3、附录 K）。
//
// 统一信封：{ok, data, error{code, message, detail}}（附录 K.0）。
// 工具只做参数解析与错误码映射，业务逻辑全在 repo / workspace 层。
package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/op"
	"github.com/chenyiwen/p37/internal/repo"
	"github.com/chenyiwen/p37/internal/workspace"
)

// Version 是实现的版本号。
const Version = "0.1.0"

// Envelope 是统一返回信封。
type Envelope struct {
	OK    bool    `json:"ok"`
	Data  any     `json:"data"`
	Error *ErrObj `json:"error"`
}

// ErrObj 是错误对象。
type ErrObj struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`
}

// CodedError 携带一个 MCP 错误码。
type CodedError struct {
	Code   string
	Msg    string
	Detail any
	Err    error
}

func (e *CodedError) Error() string {
	if e.Err != nil {
		return e.Code + ": " + e.Msg + ": " + e.Err.Error()
	}
	return e.Code + ": " + e.Msg
}

func (e *CodedError) Unwrap() error { return e.Err }

// coded 构造一个带错误码的错误。
func coded(code, format string, args ...any) *CodedError {
	return &CodedError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// Server 包装 MCP server 与仓库。
type Server struct {
	R   *repo.Repo
	Srv *mcp.Server
}

// New 构造并注册全部工具。
func New(r *repo.Repo) *Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:        "p37",
		Title:       "p37 AI 原生版本控制",
		Description: "以整合事件为版本原子、以对象存储为唯一真相源的版本控制系统",
		Version:     Version,
	}, nil)

	s := &Server{R: r, Srv: srv}
	s.registerAll()
	return s
}

// Handler 返回可直接挂到 HTTP/unix socket 上的 Streamable HTTP handler。
func (s *Server) Handler() *mcp.StreamableHTTPHandler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.Srv }, nil)
}

// toolFunc 实现一个工具。
type toolFunc func(ctx context.Context, args json.RawMessage) (any, error)

// register 注册一个工具。
func (s *Server) register(name, desc, schema string, fn toolFunc) {
	t := &mcp.Tool{
		Name:        name,
		Description: desc,
		InputSchema: json.RawMessage(schema),
	}
	s.Srv.AddTool(t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// CallToolRequest.Params.Arguments 已经是 json.RawMessage。
		raw := req.Params.Arguments
		if len(raw) == 0 {
			raw = json.RawMessage("{}")
		}

		data, err := fn(ctx, raw)
		if err != nil {
			return envelopeErr(err), nil
		}
		return envelopeOK(data), nil
	})
}

func envelopeOK(data any) *mcp.CallToolResult {
	env := Envelope{OK: true, Data: data}
	return jsonResult(env, false)
}

func envelopeErr(err error) *mcp.CallToolResult {
	code, msg, detail := classify(err)
	env := Envelope{OK: false, Data: nil, Error: &ErrObj{Code: code, Message: msg, Detail: detail}}
	return jsonResult(env, true)
}

// classify 把内部错误映射为附录 E 的错误码。
func classify(err error) (code, msg string, detail any) {
	var ce *CodedError
	if errors.As(err, &ce) {
		return ce.Code, ce.Msg, ce.Detail
	}

	switch {
	case errors.Is(err, workspace.ErrNotFound):
		return "WS_NOT_FOUND", err.Error(), nil
	case errors.Is(err, workspace.ErrLimitExceeded):
		return "WS_LIMIT_EXCEEDED", err.Error(), nil
	case errors.Is(err, workspace.ErrHasChildren):
		return "WS_HAS_CHILDREN", err.Error(), nil
	case errors.Is(err, workspace.ErrDirMissing):
		return "WS_DIR_MISSING", err.Error(), nil
	case errors.Is(err, workspace.ErrNotMounted):
		return "WS_NOT_MOUNTED", err.Error(), nil
	case errors.Is(err, workspace.ErrWrongState):
		return "WS_NOT_ACTIVE", err.Error(), nil
	case errors.Is(err, mountmgr.ErrUnavailable):
		return "OVERLAY_UNAVAILABLE", err.Error(), nil
	case errors.Is(err, mountmgr.ErrCaseConflict):
		return "PATH_CASE_CONFLICT", err.Error(), nil
	case errors.Is(err, mountmgr.ErrDirExists):
		return "WS_DIR_EXISTS", err.Error(), nil
	case errors.Is(err, repo.ErrVersionNotFound):
		return "VERSION_NOT_FOUND", err.Error(), nil
	case errors.Is(err, repo.ErrLandConflict):
		return "LAND_CONFLICT", err.Error(), nil
	case errors.Is(err, repo.ErrNotDirty):
		return "NOT_DIRTY", err.Error(), nil
	case errors.Is(err, repo.ErrLandQueueFull):
		return "LAND_QUEUE_FULL", err.Error(), nil
	case errors.Is(err, repo.ErrNotInitialized):
		return "INTERNAL", err.Error(), nil
	case errors.Is(err, meta.ErrNotExist):
		return "WS_NOT_FOUND", err.Error(), nil
	case errors.Is(err, op.ErrNoContent):
		return "CORRUPT_OBJECT", err.Error(), nil
	case strings.Contains(err.Error(), "对象损坏"):
		return "CORRUPT_OBJECT", err.Error(), nil
	case strings.Contains(err.Error(), "超过上限"):
		return "FILE_TOO_LARGE", err.Error(), nil
	}
	return "INTERNAL", err.Error(), nil
}

func jsonResult(v any, isErr bool) *mcp.CallToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(`{"ok":false,"data":null,"error":{"code":"INTERNAL","message":"结果序列化失败"}}`)
		isErr = true
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(b)}},
		StructuredContent: v,
		IsError:           isErr,
	}
}

// --- 参数解析辅助 -------------------------------------------------------

func decode[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) == 0 {
		return v, nil
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, coded("INTERNAL", "参数解析失败: %v", err)
	}
	return v, nil
}

func nowMS() int64 { return time.Now().UnixMilli() }

// parseKey 解析冲突 key。
func parseKey(s string) (hash.ID, error) {
	if s == "" {
		return hash.ID{}, coded("INTERNAL", "缺少 key")
	}
	id, err := hash.Parse(s)
	if err != nil {
		return hash.ID{}, coded("INTERNAL", "非法 key %q", s)
	}
	return id, nil
}
