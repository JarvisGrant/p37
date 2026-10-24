package mcpsrv

import (
	"context"
	"encoding/json"

	"github.com/chenyiwen/p37/internal/hub"
)

// registerRepoLifecycle 注册应用卷热插拔所需的三个 registry 生命周期工具。
func (s *Server) registerRepoLifecycle() {
	s.register("repo_register",
		"把 root 下已存在（已挂载）的目录注册为 repo；不移动/复制数据。",
		`{"type":"object","properties":{
		  "repo":{"type":"string","description":"repo 名（不是 id）"},
		  "path":{"type":"string","description":"必须位于 root 下的绝对路径"},
		  "default_line":{"type":"string","default":"main"},
		  "init_if_missing":{"type":"boolean","default":false},
		  "force_rename":{"type":"boolean","default":false},
		  "idempotency_key":{"type":"string"}
		},"required":["repo","path"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			if s.Hub == nil {
				return nil, coded("invalid_argument", "单仓库模式不支持 repo_register")
			}
			var a struct {
				Repo           string `json:"repo"`
				Path           string `json:"path"`
				DefaultLine    string `json:"default_line"`
				InitIfMissing  bool   `json:"init_if_missing"`
				ForceRename    bool   `json:"force_rename"`
				IdempotencyKey string `json:"idempotency_key"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			if a.Repo == "" || a.Path == "" {
				return nil, coded("invalid_argument", "repo 和 path 不能为空")
			}
			return s.Hub.RegisterPath(a.Repo, a.Path, hub.RegisterOptions{
				DefaultLine:   a.DefaultLine,
				InitIfMissing: a.InitIfMissing,
				ForceRename:   a.ForceRename,
			})
		})

	s.register("repo_unregister",
		"注销 repo（保留全部数据）：卸载 workspace、关闭 SQLite、释放锁后从 registry 隐藏。",
		`{"type":"object","properties":{
		  "repo":{"type":"string","description":"repo id 或 name"},
		  "force":{"type":"boolean","default":false},
		  "idempotency_key":{"type":"string"}
		},"required":["repo"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			if s.Hub == nil {
				return nil, coded("invalid_argument", "单仓库模式不支持 repo_unregister")
			}
			var a struct {
				Repo           string `json:"repo"`
				Force          bool   `json:"force"`
				IdempotencyKey string `json:"idempotency_key"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			if a.Repo == "" {
				return nil, coded("invalid_argument", "repo 不能为空")
			}
			res, err := s.Hub.Unregister(a.Repo, a.Force)
			if err != nil {
				return nil, err
			}
			out := map[string]any{
				"unregistered":         true,
				"already_unregistered": res.Already,
				"repo":                 a.Repo,
			}
			if res.Entry != nil {
				out["id"] = res.Entry.ID
				out["name"] = res.Entry.Name
				out["path"] = res.Entry.Path
			}
			return out, nil
		})

	s.register("repo_scan",
		"扫描 root 下的直接子目录，把已挂载/已初始化的 repo 与 registry 做 reconcile。",
		`{"type":"object","properties":{
		  "reconcile":{"type":"boolean","default":true},
		  "prune_missing":{"type":"boolean","default":false},
		  "dry_run":{"type":"boolean","default":false}
		},"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			if s.Hub == nil {
				return nil, coded("invalid_argument", "单仓库模式不支持 repo_scan")
			}
			var a struct {
				Reconcile    *bool `json:"reconcile"`
				PruneMissing bool  `json:"prune_missing"`
				DryRun       bool  `json:"dry_run"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			reconcile := true
			if a.Reconcile != nil {
				reconcile = *a.Reconcile
			}
			return s.Hub.Scan(hub.ScanOptions{
				Reconcile:    reconcile,
				PruneMissing: a.PruneMissing,
				DryRun:       a.DryRun,
			})
		})
}
