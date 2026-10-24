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
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chenyiwen/p37/internal/buildinfo"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/hub"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/op"
	"github.com/chenyiwen/p37/internal/remotesync"
	"github.com/chenyiwen/p37/internal/repo"
	"github.com/chenyiwen/p37/internal/workspace"
)

// Version 是实现的版本号。必须保持为 var：发布构建通过 -X 注入。
var Version = buildinfo.Version

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
	// R 是单仓库模式下的默认仓库；多仓库模式下由 Resolve 按 repo 参数路由。
	R   *repo.Repo
	Srv *mcp.Server

	// Resolve 按 repo id/name 返回仓库；为 nil 时只能使用 R。
	Resolve func(string) (*repo.Repo, error)
	// Hub 是多仓库控制面；单仓库模式为 nil。
	Hub *hub.Hub

	// syncOpt / remoteConfigured 供 sync_* 工具使用（由 daemon 注入）。
	syncOpt          remotesync.Options
	remoteConfigured bool

	// exportMu 保护异步导出任务表。任务只在当前进程内有效；daemon 重启后
	// 调用方应重新发起（导出目录可能已部分写入，需自行清理）。
	exportMu   sync.Mutex
	exportJobs map[string]*exportJob
}

// SetSyncOptions 注入远端同步参数（p37d 在启动时调用）。
func (s *Server) SetSyncOptions(opt remotesync.Options, configured bool) {
	s.syncOpt = opt
	s.remoteConfigured = configured
}

// New 构造并注册全部工具。
// NewWithResolver 构造多仓库 MCP server。defaultRepo 可为 nil；此时所有业务
// 工具都必须带 repo 参数。
func NewWithResolver(defaultRepo *repo.Repo, resolve func(string) (*repo.Repo, error)) *Server {
	s := New(defaultRepo)
	s.Resolve = resolve
	return s
}

// New 构造并注册全部工具。
func New(r *repo.Repo) *Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:        "p37",
		Title:       "p37 AI 原生版本控制",
		Description: "以整合事件为版本原子、以对象存储为唯一真相源的版本控制系统",
		Version:     Version,
	}, nil)

	s := &Server{R: r, Srv: srv, exportJobs: map[string]*exportJob{}}
	s.registerAll()
	return s
}

// withRepoParam 给所有工具 schema 注入可选的 repo 参数。
func withRepoParam(raw json.RawMessage) json.RawMessage {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	if m["type"] == nil {
		m["type"] = "object"
	}
	props, _ := m["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
		m["properties"] = props
	}
	if _, ok := props["repo"]; !ok {
		props["repo"] = map[string]any{
			"type":        "string",
			"description": "仓库 id 或 name；单仓库模式可省略",
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return b
}

// repoArg 只从原始参数里提取 repo 字段，避免每个 handler 重复解析。
func repoArg(raw json.RawMessage) string {
	var a struct {
		Repo string `json:"repo"`
	}
	_ = json.Unmarshal(raw, &a)
	return a.Repo
}

// repoFor 解析工具调用里的 repo 参数。单仓库模式可省略。
func (s *Server) repoFor(ref string) (*repo.Repo, error) {
	if ref == "" {
		if s.R != nil {
			return s.R, nil
		}
		return nil, coded("repo_required", "多仓库模式必须提供 repo（id 或 name）")
	}
	if s.Resolve == nil {
		if s.R != nil {
			return s.R, nil
		}
		return nil, coded("repo_not_found", "仓库 %q 不存在", ref)
	}
	r, err := s.Resolve(ref)
	if err != nil {
		return nil, coded("repo_not_found", "仓库 %q 不存在: %v", ref, err)
	}
	return r, nil
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
		InputSchema: withRepoParam(json.RawMessage(schema)),
	}
	s.Srv.AddTool(t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// CallToolRequest.Params.Arguments 已经是 json.RawMessage。
		raw := req.Params.Arguments
		if len(raw) == 0 {
			raw = json.RawMessage("{}")
		}

		data, err := fn(ctx, raw)
		if err != nil {
			return envelopeErrCtx(s, raw, err), nil
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

// envelopeErrCtx 在统一出口补齐错误上下文：code + repo + 标识符 + reason。
// 这里不依赖各 handler 记得填 detail，避免 P34 只能退化成“操作失败”。
func envelopeErrCtx(s *Server, raw json.RawMessage, err error) *mcp.CallToolResult {
	code, msg, detail := classify(err)
	m := map[string]any{}
	if dm, ok := detail.(map[string]any); ok {
		for k, v := range dm {
			m[k] = v
		}
	}
	var args map[string]any
	if json.Unmarshal(raw, &args) == nil {
		for _, k := range []string{"repo", "ws_id", "name", "line", "ref", "version_id",
			"target", "parent", "parent_ws_id", "source", "idempotency_key"} {
			if v, ok := args[k]; ok && v != nil && v != "" {
				if _, exists := m[k]; !exists {
					m[k] = v
				}
			}
		}
	}
	if _, ok := m["repo"]; !ok {
		if ref, _ := args["repo"].(string); ref != "" {
			m["repo"] = ref
		} else if s != nil && s.R != nil {
			m["repo"] = repoLabel(s.R)
		}
	}
	if _, ok := m["reason"]; !ok {
		m["reason"] = reasonForCode(code)
	}
	switch code {
	case "ws_gone":
		if _, ok := m["legacy_code"]; !ok {
			m["legacy_code"] = "ws_not_found"
		}
	case "ws_open_in_progress":
		if _, ok := m["legacy_code"]; !ok {
			m["legacy_code"] = "ws_busy"
		}
	case "idempotency_conflict":
		if _, ok := m["legacy_code"]; !ok {
			m["legacy_code"] = "invalid_argument"
		}
	}
	if _, ok := m["retryable"]; !ok {
		m["retryable"] = code == "ws_open_in_progress" || code == "sync_in_progress" ||
			code == "land_queue_full"
	}
	env := Envelope{OK: false, Data: nil, Error: &ErrObj{Code: code, Message: msg, Detail: m}}
	return jsonResult(env, true)
}

func reasonForCode(code string) string {
	switch code {
	case "ws_not_found":
		return "never_opened_or_tombstone_expired"
	case "ws_gone":
		return "unknown"
	case "line_not_found":
		return "line_absent"
	case "repo_not_found":
		return "repo_absent_or_unregistered"
	case "repo_not_initialized":
		return "repo_db_missing_or_corrupt"
	case "quota_exceeded":
		return "quota_exceeded"
	case "repo_busy":
		return "active_workspaces_or_leases"
	case "ws_busy":
		return "state_not_allowed"
	case "ws_broken":
		return "dir_missing_or_io_error"
	case "ws_not_mounted":
		return "overlay_mount_lost"
	case "target_moved":
		return "expected_head_or_tree_mismatch"
	case "sync_in_progress":
		return "sync_running"
	case "land_queue_full":
		return "land_queue_full"
	case "ws_open_in_progress":
		return "claim_in_progress"
	case "idempotency_conflict":
		return "key_reused_with_different_request"
	default:
		return code
	}
}

// classify 把内部错误映射为附录 E 的错误码。
func classify(err error) (code, msg string, detail any) {
	var ce *CodedError
	if errors.As(err, &ce) {
		return ce.Code, ce.Msg, ce.Detail
	}

	switch {
	case errors.Is(err, repo.ErrLineNotFound):
		return "line_not_found", err.Error(), nil
	case errors.Is(err, repo.ErrLineExists):
		return "line_exists", err.Error(), nil
	case errors.Is(err, repo.ErrLineProtected):
		return "line_protected", err.Error(), nil
	case errors.Is(err, repo.ErrTargetInvalid):
		return "target_invalid", err.Error(), nil
	case errors.Is(err, repo.ErrTargetNotAncestor):
		return "target_not_ancestor", err.Error(), nil
	case errors.Is(err, repo.ErrTargetMoved):
		return "target_moved", err.Error(), nil
	case errors.Is(err, repo.ErrSourceMoved):
		return "source_moved", err.Error(), nil
	case errors.Is(err, repo.ErrMergeBaseAmbiguous), errors.Is(err, meta.ErrMergeBaseAmbiguous):
		return "merge_base_ambiguous", err.Error(), nil
	case errors.Is(err, repo.ErrMergeExpired):
		return "merge_expired", err.Error(), nil
	case errors.Is(err, repo.ErrPreviewExpired):
		return "preview_expired", err.Error(), nil
	case errors.Is(err, repo.ErrHasActiveChildren):
		var ace *repo.ActiveChildrenError
		if errors.As(err, &ace) {
			return "ws_has_active_children", ace.Error(), map[string]any{
				"ws_id": ace.WSID, "blocked_by_children": ace.Children,
			}
		}
		return "ws_has_active_children", err.Error(), nil
	case errors.Is(err, workspace.ErrNotFound):
		return "ws_not_found", err.Error(), nil
	case errors.Is(err, workspace.ErrOpenInProgress):
		var oe *workspace.OpenInProgressError
		if errors.As(err, &oe) {
			return "ws_open_in_progress", err.Error(), map[string]any{
				"ws_id": oe.WSID, "retry_after_ms": oe.RetryAfterMS,
			}
		}
		return "ws_open_in_progress", err.Error(), nil
	case errors.Is(err, workspace.ErrIdempotencyConflict), errors.Is(err, repo.ErrIdemConflict):
		var ic *workspace.IdemConflictError
		if errors.As(err, &ic) {
			return "idempotency_conflict", err.Error(), map[string]any{
				"idempotency_key": ic.Key, "ws_id": ic.WSID,
				"field": ic.Field, "existing": ic.Existing, "requested": ic.Requested,
			}
		}
		var rc *repo.IdemConflictError
		if errors.As(err, &rc) {
			return "idempotency_conflict", err.Error(), map[string]any{
				"idempotency_key": rc.Key, "field": rc.Field,
				"existing": rc.Existing, "requested": rc.Requested,
			}
		}
		return "idempotency_conflict", err.Error(), nil
	case errors.Is(err, workspace.ErrCleanupFailed):
		return "ws_cleanup_warning", err.Error(), nil
	case errors.Is(err, workspace.ErrLimitExceeded):
		var le *workspace.LimitError
		if errors.As(err, &le) {
			return "quota_exceeded", err.Error(), map[string]any{
				"scope": le.Scope, "used": le.Used, "limit": le.Limit,
			}
		}
		return "quota_exceeded", err.Error(), nil
	case errors.Is(err, workspace.ErrHasChildren):
		return "ws_has_active_children", err.Error(), nil
	case errors.Is(err, workspace.ErrDirMissing):
		return "ws_broken", err.Error(), nil
	case errors.Is(err, workspace.ErrNotMounted):
		return "ws_not_mounted", err.Error(), nil
	case errors.Is(err, workspace.ErrWrongState):
		return "ws_busy", err.Error(), nil
	case errors.Is(err, mountmgr.ErrUnavailable):
		return "overlay_unavailable", err.Error(), nil
	case errors.Is(err, mountmgr.ErrCaseConflict):
		return "path_case_conflict", err.Error(), nil
	case errors.Is(err, mountmgr.ErrDirExists):
		return "ws_exists", err.Error(), nil
	case errors.Is(err, repo.ErrVersionNotFound):
		return "version_not_found", err.Error(), nil
	case errors.Is(err, repo.ErrLandConflict):
		return "conflict", err.Error(), nil
	case errors.Is(err, repo.ErrNotDirty):
		return "not_dirty", err.Error(), nil
	case errors.Is(err, repo.ErrLandQueueFull):
		return "land_queue_full", err.Error(), nil
	case errors.Is(err, repo.ErrPathNotFound):
		return "path_not_found", err.Error(), nil
	case errors.Is(err, repo.ErrDestNotEmpty):
		return "destination_not_empty", err.Error(), nil
	case errors.Is(err, repo.ErrInvalidDest):
		return "invalid_argument", err.Error(), nil
	case errors.Is(err, hub.ErrInvalidPath):
		return "invalid_path", err.Error(), nil
	case errors.Is(err, hub.ErrRepoNotInitialized):
		return "repo_not_initialized", err.Error(), nil
	case errors.Is(err, hub.ErrRepoNameMismatch):
		return "repo_name_mismatch", err.Error(), nil
	case errors.Is(err, hub.ErrRepoPathExists):
		return "repo_path_exists", err.Error(), nil
	case errors.Is(err, hub.ErrRepoIsMountpoint):
		return "repo_is_mountpoint", err.Error(), nil
	case errors.Is(err, hub.ErrRepoBusy):
		var be *hub.BusyError
		if errors.As(err, &be) {
			return "repo_busy", be.Error(), map[string]any{
				"workspaces": be.Workspaces, "leases": be.Leases,
			}
		}
		return "repo_busy", err.Error(), nil
	case errors.Is(err, hub.ErrRegistryBusy):
		return "registry_busy", err.Error(), nil
	case errors.Is(err, hub.ErrRepoLocked):
		return "repo_busy", err.Error(), nil
	case errors.Is(err, repo.ErrNotInitialized):
		return "repo_not_initialized", err.Error(), nil
	case errors.Is(err, meta.ErrNotExist):
		return "ws_not_found", err.Error(), nil
	case errors.Is(err, op.ErrNoContent):
		return "corrupt_object", err.Error(), nil
	case strings.Contains(err.Error(), "对象损坏"):
		return "corrupt_object", err.Error(), nil
	case strings.Contains(err.Error(), "超过上限"):
		return "file_too_large", err.Error(), nil
	}
	return "internal", err.Error(), nil
}

func jsonResult(v any, isErr bool) *mcp.CallToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(`{"ok":false,"data":null,"error":{"code":"internal","message":"结果序列化失败"}}`)
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
		return v, coded("internal", "参数解析失败: %v", err)
	}
	return v, nil
}

func nowMS() int64 { return time.Now().UnixMilli() }

// parseKey 解析冲突 key。
func parseKey(s string) (hash.ID, error) {
	if s == "" {
		return hash.ID{}, coded("internal", "缺少 key")
	}
	id, err := hash.Parse(s)
	if err != nil {
		return hash.ID{}, coded("internal", "非法 key %q", s)
	}
	return id, nil
}
