package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/hub"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/repo"
	"github.com/chenyiwen/p37/internal/workspace"
)

// registerRepoLineRead 注册多仓库、命名线、历史读取与新的工作区/land 工具。
func (s *Server) registerRepoLineRead() {
	s.register("repo_list", "列出 daemon 管理的全部仓库。",
		`{"type":"object","properties":{},"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			if s.Hub == nil {
				if s.R == nil {
					return []any{}, nil
				}
				st, err := s.R.Status()
				if err != nil {
					return nil, err
				}
				return []any{map[string]any{"id": "default", "name": "default",
					"path": st.Root, "default_line": "main", "version_count": st.VersionCount}}, nil
			}
			es, err := s.Hub.List()
			if err != nil {
				return nil, err
			}
			return es, nil
		})

	s.register("repo_create", "创建一个新仓库（含空 v0 与默认 line）。传 path 时等价于 repo_init(mode=empty)；不传 path 是 L2 scratch，不能用于应用卷。",
		`{"type":"object","properties":{"name":{"type":"string"},"path":{"type":"string","description":"可选；应用卷必须传持久挂载路径"},"default_line":{"type":"string"},"remote_key":{"type":"string"},"remote_prefix":{"type":"string"},"remote":{"type":"object"},"idempotency_key":{"type":"string"}},"required":["name"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			if s.Hub == nil {
				return nil, coded("repo_exists", "单仓库模式不支持 repo_create")
			}
			var a struct {
				Name           string          `json:"name"`
				Path           string          `json:"path"`
				DefaultLine    string          `json:"default_line"`
				RemoteKey      string          `json:"remote_key"`
				RemotePrefix   string          `json:"remote_prefix"`
				Remote         json.RawMessage `json:"remote"`
				IdempotencyKey string          `json:"idempotency_key"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			var e *hub.Entry
			var err error
			if a.Path != "" {
				if _, err = s.Hub.InitRepo(a.Name, a.Path, hub.InitOptions{
					Mode: repo.InitEmpty, DefaultLine: a.DefaultLine,
					IdempotencyKey: a.IdempotencyKey,
				}); err != nil {
					return nil, err
				}
				e, err = s.Hub.Get(a.Name)
			} else {
				e, err = s.Hub.CreateEx(a.Name, a.DefaultLine, a.RemoteKey, a.RemotePrefix)
			}
			if err != nil {
				return nil, err
			}
			if len(a.Remote) > 0 && string(a.Remote) != "null" {
				rr, err := s.Hub.Open(e.ID)
				if err != nil {
					return nil, err
				}
				cfg, err := config.Load(rr.Root)
				if err != nil {
					return nil, err
				}
				if err := json.Unmarshal(a.Remote, &cfg.Remote); err != nil {
					return nil, coded("invalid_argument", "remote 解析失败: %v", err)
				}
				if a.RemoteKey != "" {
					cfg.Remote.RemoteKey = a.RemoteKey
				}
				if err := config.Save(rr.Root, cfg); err != nil {
					return nil, err
				}
			}
			return e, nil
		})

	s.register("repo_info", "返回仓库详情。",
		`{"type":"object","properties":{"repo":{"type":"string"}},"required":["repo"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			var entry *hub.Entry
			if s.Hub != nil {
				e, err := s.Hub.Get(repoArg(raw))
				if err != nil {
					return nil, coded("repo_not_found", "仓库 %q 不存在", repoArg(raw))
				}
				entry = e
				// 卷未挂载时返回 registry-only 信息，不 Open repo。
				if !e.Available {
					out := entryInfo(e)
					out["root"] = e.Path
					out["status"] = "unavailable"
					out["initialized"] = false
					out["repo_db_ok"] = false
					out["default_line_ready"] = false
					out["next_action"] = "repo_init"
					out["p37_version"] = Version
					out["persistence"] = map[string]any{
						"truth_source": "local_repo",
						"local":        map[string]any{"root": e.Path, "available": false},
						"durable":      false, "durable_scope": "local_only",
					}
					return out, nil
				}
				// 半初始化仓库不能走 repoFor（Open 会尝试自愈并可能写库）；
				// 先用只读完整性探测，未初始化就返回 registry-only 视图。
				if in, _ := s.Hub.Health(repoArg(raw), "", false); in != nil && !in.Initialized {
					out := entryInfo(e)
					out["root"] = e.Path
					out["status"] = "uninitialized"
					out["initialized"] = false
					out["reason"] = in.Reason
					out["next_action"] = in.NextAction
					out["default_line"] = in.DefaultLine
					out["default_line_ready"] = false
					out["mainline_head"] = in.MainlineHead
					out["version_count"] = in.VersionCount
					out["repo_db_ok"] = in.RepoDBOK
					out["repo_json_ok"] = in.RepoJSONOK
					out["refs_ok"] = in.RefsOK
					out["root_ok"] = in.RootOK
					out["p37_version"] = Version
					out["lines"] = []any{}
					out["persistence"] = map[string]any{
						"truth_source": "local_repo",
						"local":        map[string]any{"root": e.Path, "available": true, "mount_point": e.Mounted},
						"durable":      false, "durable_scope": "local_only",
					}
					return out, nil
				}
			}
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			st, err := rr.Status()
			if err != nil {
				return nil, err
			}
			in := repo.IntegrityOpen(rr)
			rawLines, _ := rr.Lines()
			lines := make([]meta.LineView, 0, len(rawLines))
			for i := range rawLines {
				lines = append(lines, rawLines[i].View())
			}
			ws, _ := rr.WS.List()
			visible := 0
			for i := range ws {
				if !ws[i].Hidden {
					visible++
				}
			}
			def, _ := rr.DB.DefaultLine()
			cfg, _ := config.Load(rr.Root)
			rp := cfg.Remote.Prefix
			if cfg.Remote.RemoteKey != "" {
				if rp == "" {
					rp = cfg.Remote.RemoteKey
				} else {
					rp = strings.Trim(rp, "/") + "/" + cfg.Remote.RemoteKey
				}
			}
			out := map[string]any{
				"root": st.Root, "default_line": def, "mainline_head": st.MainlineHead.String(),
				"version_count": st.VersionCount, "epoch": st.Epoch,
				"workspace_count": visible, "format_ver": st.FormatVer,
				"initialized": in.Initialized, "default_line_ready": in.DefaultLineReady,
				"repo_db_ok": in.RepoDBOK, "repo_json_ok": in.RepoJSONOK,
				"root_ok": in.RootOK, "refs_ok": in.RefsOK,
				"next_action": in.NextAction, "reason": in.Reason,
				"p37_version": Version,
				"lines":       lines,
				"persistence": persistenceView(rr),
				"remote": map[string]any{"enabled": cfg.Remote.Enabled,
					"backend": cfg.Remote.Backend, "remote_key": cfg.Remote.RemoteKey,
					"prefix": cfg.Remote.Prefix, "remote_prefix": rp},
			}
			if entry != nil {
				for k, v := range entryInfo(entry) {
					out[k] = v
				}
				if pm, ok := out["persistence"].(map[string]any); ok {
					if lm, ok := pm["local"].(map[string]any); ok {
						lm["mount_point"] = entry.Mounted
						lm["available"] = entry.Available
						lm["degraded"] = entry.Degraded
					}
				}
			}
			return out, nil
		})

	s.register("repo_delete", "删除仓库；默认拒绝有活跃工作区/租约的仓库。",
		`{"type":"object","properties":{"repo":{"type":"string"},"force":{"type":"boolean"},"delete_remote":{"type":"boolean"},"idempotency_key":{"type":"string"}},"required":["repo"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			var a struct {
				Repo           string `json:"repo"`
				Force          bool   `json:"force"`
				DeleteRemote   bool   `json:"delete_remote"`
				IdempotencyKey string `json:"idempotency_key"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			if s.Hub == nil {
				return nil, coded("repo_not_found", "单仓库模式不支持 repo_delete")
			}
			e, err := s.Hub.Delete(a.Repo, a.Force)
			if errors.Is(err, hub.ErrRepoNotFound) {
				return map[string]any{"deleted": true, "already_deleted": true}, nil
			}
			if errors.Is(err, hub.ErrRepoBusy) {
				return nil, coded("repo_busy", "%v", err)
			}
			if err != nil {
				return nil, err
			}
			return map[string]any{"deleted": true, "id": e.ID, "name": e.Name}, nil
		})

	s.register("repo_disk_usage", "返回仓库磁盘占用（实际块）。",
		`{"type":"object","properties":{"repo":{"type":"string"}},"required":["repo"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			bytes, err := dirSize(rr.Root)
			if err != nil {
				return nil, err
			}
			return map[string]any{"repo": repoArg(raw), "root": rr.Root, "disk_bytes": bytes}, nil
		})

	s.register("repo_import", "把目录导入空仓库作为 v0。未初始化的已注册仓库等价于 repo_init(mode=import)。",
		`{"type":"object","properties":{"repo":{"type":"string"},"dir":{"type":"string"},"task":{"type":"string"},"force":{"type":"boolean","default":false},"idempotency_key":{"type":"string"}},"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			var a struct {
				Dir   string `json:"dir"`
				Task  string `json:"task"`
				Force bool   `json:"force"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			// 未初始化的已注册仓库：走统一 InitRepo(mode=import)。
			if s.Hub != nil {
				if e, gerr := s.Hub.Get(repoArg(raw)); gerr == nil {
					if in, _ := s.Hub.Health(e.Name, "", false); in != nil && !in.Initialized {
						dir := a.Dir
						if dir == "" {
							dir = e.Path
						}
						if filepath.Clean(dir) != filepath.Clean(e.Path) {
							return nil, codedDetail("invalid_argument", map[string]any{
								"path": e.Path, "next_action": "repo_init_import",
								"retryable": false,
							}, "未初始化仓库只能原地导入；dir 必须等于 %s", e.Path)
						}
						res, err := s.Hub.InitRepo(e.Name, e.Path, hub.InitOptions{
							Mode: repo.InitImport, DefaultLine: e.DefaultLine,
						})
						if err != nil {
							return nil, err
						}
						return map[string]any{
							"version_id": res.Version.ID, "dir": e.Path,
							"initialized": true, "imported": true,
						}, nil
					}
				}
			}
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			dir := a.Dir
			if dir == "" {
				dir = rr.Root
			}
			id, err := rr.ImportWithOptions(dir, repo.ImportOptions{Force: a.Force})
			if err != nil {
				return nil, err
			}
			return map[string]any{"version_id": id.String(), "dir": dir}, nil
		})

	s.register("repo_set_remote", "设置/更新仓库远端配置（凭证仍只从环境变量读）。",
		`{"type":"object","properties":{"repo":{"type":"string"},"remote":{"type":"object"}},"required":["repo","remote"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				Remote json.RawMessage `json:"remote"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			cfg, err := config.Load(rr.Root)
			if err != nil {
				return nil, err
			}
			if err := json.Unmarshal(a.Remote, &cfg.Remote); err != nil {
				return nil, coded("invalid_argument", "remote 解析失败: %v", err)
			}
			// 凭证不落库：RemoteConfig 结构里没有凭证字段，只有环境变量。
			if err := config.Save(rr.Root, cfg); err != nil {
				return nil, err
			}
			return map[string]any{"enabled": cfg.Remote.Enabled, "backend": cfg.Remote.Backend,
				"remote_key": cfg.Remote.RemoteKey, "prefix": cfg.Remote.Prefix}, nil
		})

	// --- line ---------------------------------------------------------
	s.register("line_create", "创建命名版本线。",
		`{"type":"object","properties":{"repo":{"type":"string"},"name":{"type":"string"},"from":{"type":"string"},"protected":{"type":"boolean"},"idempotency_key":{"type":"string"}},"required":["repo","name"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				Name      string `json:"name"`
				From      string `json:"from"`
				Protected bool   `json:"protected"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			l, err := rr.CreateLine(a.Name, a.From, a.Protected)
			if err != nil {
				if errors.Is(err, repo.ErrLineExists) {
					return nil, coded("line_exists", "%v", err)
				}
				return nil, err
			}
			return l.View(), nil
		})

	s.register("line_list", "列出全部命名线。",
		`{"type":"object","properties":{"repo":{"type":"string"}},"required":["repo"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			ls, err := rr.Lines()
			if err != nil {
				return nil, err
			}
			wss, _ := rr.WS.List()
			out := make([]map[string]any, 0, len(ls))
			for i := range ls {
				n := 0
				for j := range wss {
					if !wss[j].Hidden && wss[j].BaseLine == ls[i].Name && wss[j].LandedTS == 0 {
						n++
					}
				}
				v := ls[i].View()
				out = append(out, map[string]any{
					"name": v.Name, "head": v.Head, "protected": v.Protected,
					"created_from": v.CreatedFrom, "created_ts": v.CreatedTS,
					"updated_ts": v.UpdatedTS, "workspace_count": n,
				})
			}
			return out, nil
		})

	s.register("line_info", "返回一条命名线的详情。",
		`{"type":"object","properties":{"repo":{"type":"string"},"name":{"type":"string"}},"required":["repo","name"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			return rr.LineInfo(a.Name)
		})

	s.register("line_delete", "删除命名线（不删版本）。",
		`{"type":"object","properties":{"repo":{"type":"string"},"name":{"type":"string"},"force":{"type":"boolean"},"idempotency_key":{"type":"string"}},"required":["repo","name"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				Name  string `json:"name"`
				Force bool   `json:"force"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			if err := rr.DeleteLine(a.Name, a.Force); err != nil {
				if errors.Is(err, repo.ErrLineNotFound) {
					return map[string]any{"deleted": true, "already_deleted": true, "name": a.Name}, nil
				}
				if errors.Is(err, repo.ErrLineProtected) {
					return nil, coded("line_protected", "%v", err)
				}
				return nil, err
			}
			return map[string]any{"deleted": true, "name": a.Name}, nil
		})

	s.register("line_protect", "设置/取消命名线保护。",
		`{"type":"object","properties":{"repo":{"type":"string"},"name":{"type":"string"},"protected":{"type":"boolean"},"force":{"type":"boolean"}},"required":["repo","name","protected"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				Name      string `json:"name"`
				Protected bool   `json:"protected"`
				Force     bool   `json:"force"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			if err := rr.ProtectLine(a.Name, a.Protected, a.Force); err != nil {
				return nil, err
			}
			return map[string]any{"name": a.Name, "protected": a.Protected}, nil
		})

	s.register("line_reset", "把 line head 移动到历史版本。",
		`{"type":"object","properties":{"repo":{"type":"string"},"name":{"type":"string"},"version":{"type":"string"},"force":{"type":"boolean"},"reason":{"type":"string"},"expected_head":{"type":"string"},"idempotency_key":{"type":"string"}},"required":["repo","name","version"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				Name         string `json:"name"`
				Version      string `json:"version"`
				Force        bool   `json:"force"`
				Reason       string `json:"reason"`
				ExpectedHead string `json:"expected_head"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			l, err := rr.ResetLine(a.Name, a.Version, a.Force, a.Reason, a.ExpectedHead)
			if err != nil {
				if errors.Is(err, repo.ErrTargetNotAncestor) {
					return nil, coded("target_not_ancestor", "%v", err)
				}
				if errors.Is(err, repo.ErrTargetMoved) {
					return nil, coded("target_moved", "%v", err)
				}
				if errors.Is(err, repo.ErrLineProtected) {
					return nil, coded("line_protected", "%v", err)
				}
				return nil, err
			}
			return l.View(), nil
		})

	s.register("line_log", "返回命名线的祖先历史。",
		`{"type":"object","properties":{"repo":{"type":"string"},"name":{"type":"string"},"limit":{"type":"integer"},"before_ts":{"type":"integer"},"before_id":{"type":"string"}},"required":["repo","name"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				Name     string `json:"name"`
				Limit    int    `json:"limit"`
				BeforeTS int64  `json:"before_ts"`
				BeforeID string `json:"before_id"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			vs, err := rr.LineLog(a.Name, a.Limit, a.BeforeTS, a.BeforeID)
			if err != nil {
				return nil, err
			}
			return versionSummaries(vs), nil
		})

	s.register("line_compare", "比较两条命名线。",
		`{"type":"object","properties":{"repo":{"type":"string"},"a":{"type":"string"},"b":{"type":"string"}},"required":["repo","a","b"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				A string `json:"a"`
				B string `json:"b"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			c, err := rr.CompareLines(a.A, a.B)
			if err != nil {
				return nil, err
			}
			return c, nil
		})

	s.register("line_graph", "返回版本 DAG 节点与边。",
		`{"type":"object","properties":{"repo":{"type":"string"},"lines":{"type":"array","items":{"type":"string"}},"limit":{"type":"integer"}},"required":["repo"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				Lines []string `json:"lines"`
				Limit int      `json:"limit"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			return rr.LineGraph(a.Lines, a.Limit)
		})

	s.register("line_merge", "把 source line 的成果并入 target line。",
		`{"type":"object","properties":{"repo":{"type":"string"},"source":{"type":"string"},"target":{"type":"string"},"preview":{"type":"boolean"},"no_ff":{"type":"boolean"},"merge_id":{"type":"string"},"expected_head":{"type":"string"},"expected_source_head":{"type":"string"},"task":{"type":"string"},"idempotency_key":{"type":"string"}},"required":["repo","source","target"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				Source             string `json:"source"`
				Target             string `json:"target"`
				Preview            bool   `json:"preview"`
				NoFF               bool   `json:"no_ff"`
				MergeID            string `json:"merge_id"`
				ExpectedHead       string `json:"expected_head"`
				ExpectedSourceHead string `json:"expected_source_head"`
				Task               string `json:"task"`
				IdempotencyKey     string `json:"idempotency_key"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			res, err := rr.MergeLines(a.Source, a.Target, repo.MergeOptions{
				Preview: a.Preview, NoFF: a.NoFF, MergeID: a.MergeID,
				ExpectedHead: a.ExpectedHead, ExpectedSourceHead: a.ExpectedSourceHead,
				IdempotencyKey: a.IdempotencyKey, TaskDesc: a.Task,
			})
			if err != nil {
				if errors.Is(err, repo.ErrLandConflict) && res != nil {
					return mergeView(res), nil
				}
				if errors.Is(err, repo.ErrTargetMoved) {
					return nil, coded("target_moved", "%v", err)
				}
				if errors.Is(err, repo.ErrSourceMoved) {
					return nil, coded("source_moved", "%v", err)
				}
				if errors.Is(err, repo.ErrMergeBaseAmbiguous) {
					return nil, coded("merge_base_ambiguous", "%v", err)
				}
				return nil, err
			}
			if cfg, err := config.Load(rr.Root); err == nil && cfg.Remote.AutoPush {
				go func() { _, _ = pushRepo(context.Background(), rr, s.Hub, true, 0) }()
			}
			return mergeView(res), nil
		})

	// --- 历史读取 -----------------------------------------------------
	s.register("version_tree", "列出历史版本/命名线的目录树。",
		`{"type":"object","properties":{"repo":{"type":"string"},"ref":{"type":"string"},"path":{"type":"string"},"recursive":{"type":"boolean"},"limit":{"type":"integer"}},"required":["repo","ref"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				Ref       string `json:"ref"`
				Path      string `json:"path"`
				Recursive bool   `json:"recursive"`
				Limit     int    `json:"limit"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			items, truncated, err := rr.VersionTree(a.Ref, a.Path, a.Recursive, a.Limit)
			if err != nil {
				return nil, err
			}
			return map[string]any{"items": items, "truncated": truncated}, nil
		})

	s.register("version_file", "读取历史版本中的单个文件（base64）。",
		`{"type":"object","properties":{"repo":{"type":"string"},"ref":{"type":"string"},"path":{"type":"string"},"max_bytes":{"type":"integer"}},"required":["repo","ref","path"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				Ref      string `json:"ref"`
				Path     string `json:"path"`
				MaxBytes int64  `json:"max_bytes"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			return rr.VersionFile(a.Ref, a.Path, a.MaxBytes)
		})

	s.register("version_archive", "把历史版本的子树打包到 VM 内临时文件。",
		`{"type":"object","properties":{"repo":{"type":"string"},"ref":{"type":"string"},"path":{"type":"string"},"format":{"type":"string","enum":["tar.gz","zip"]}},"required":["repo","ref"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				Ref    string `json:"ref"`
				Path   string `json:"path"`
				Format string `json:"format"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			return rr.VersionArchive(a.Ref, a.Path, a.Format)
		})

	s.register("ws_diff", "返回工作区相对 base 的文件级差异。",
		`{"type":"object","properties":{"repo":{"type":"string"},"ws_id":{"type":"string"}},"required":["repo","ws_id"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				WsID string `json:"ws_id"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			_, view, err := wsLifecycleOrError(rr, a.WsID, true)
			if err != nil {
				return nil, err
			}
			if view != nil {
				if m, ok := view.(map[string]any); ok {
					m["items"] = []any{}
					m["files"] = []any{}
				}
				return view, nil
			}
			return rr.WSDiff(a.WsID)
		})

	s.register("ws_reap", "清理过期/孤儿工作区，默认 dry-run。",
		`{"type":"object","properties":{"repo":{"type":"string"},"older_than":{"type":"string"},"state":{"type":"string"},"name_prefix":{"type":"string","description":"只过滤已符合回收条件的候选，不扩大回收范围"},"include_orphan_dirs":{"type":"boolean"},"dry_run":{"type":"boolean"}},"required":["repo"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				OlderThan        string `json:"older_than"`
				State            string `json:"state"`
				NamePrefix       string `json:"name_prefix"`
				IncludeOrphanDir bool   `json:"include_orphan_dirs"`
				DryRun           *bool  `json:"dry_run"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			dry := true
			if a.DryRun != nil {
				dry = *a.DryRun
			}
			res, err := reapWorkspaces(rr, a.OlderThan, a.State, a.NamePrefix, dry)
			if err != nil {
				return nil, err
			}
			if a.IncludeOrphanDir && !dry {
				reaped, err := rr.WS.ReapOrphanDirs()
				if err != nil {
					res["orphan_errors"] = err.Error()
				}
				res["orphan_dirs"] = reaped
			}
			return res, nil
		})
}

func entryInfo(e *hub.Entry) map[string]any {
	out := map[string]any{
		"id": e.ID, "name": e.Name, "path": e.Path,
		"default_line": e.DefaultLine, "mounted": e.Mounted,
		"available": e.Available, "degraded": e.Degraded,
	}
	if e.DegradedMsg != "" {
		out["degraded_msg"] = e.DegradedMsg
	}
	if e.RemoteKey != "" {
		out["remote_key"] = e.RemoteKey
	}
	if e.RemotePrefix != "" {
		out["remote_prefix"] = e.RemotePrefix
	}
	return out
}

func versionSummaries(vs []repo.VersionSummary) []map[string]any {
	out := make([]map[string]any, 0, len(vs))
	for _, v := range vs {
		out = append(out, map[string]any{
			"id": v.ID.String(), "parent": v.Parent.String(), "ts": v.TS,
			"agent": v.Agent, "task": v.Task, "epoch": v.Epoch,
		})
	}
	return out
}

func mergeView(res *repo.MergeResult) map[string]any {
	conflicts := make([]map[string]any, 0, len(res.Conflicts))
	for i := range res.Conflicts {
		c := &res.Conflicts[i]
		conflicts = append(conflicts, map[string]any{
			"key": c.Key.String(), "path": c.Path, "kind": string(c.Kind),
		})
	}
	out := map[string]any{
		"merge_id": res.MergeID, "source": res.Source, "target": res.Target,
		"source_head": res.SourceHead.String(), "target_head": res.TargetHead.String(),
		"base_version": res.BaseVersion.String(), "fast_forward": res.FastForward,
		"already_merged": res.AlreadyMerged, "no_op": res.NoOp,
		"can_merge": res.CanMerge, "conflicts": conflicts, "files": res.Files,
		"auto_merged": res.AutoMerged,
	}
	if !res.VersionID.IsZero() {
		out["version_id"] = res.VersionID.String()
		out["state"] = "done"
	}
	return out
}

func landView2(rr *repo.Repo, res *repo.LandResult) map[string]any {
	conflicts := make([]map[string]any, 0, len(res.Conflicts))
	for i := range res.Conflicts {
		c := &res.Conflicts[i]
		cands := make([]map[string]string, 0, len(c.Candidat))
		for _, cd := range c.Candidat {
			cands = append(cands, map[string]string{"kind": string(cd.Kind), "note": cd.Note})
		}
		conflicts = append(conflicts, map[string]any{
			"key": c.Key.String(), "path": c.Path, "kind": string(c.Kind),
			"state": "open", "base": toStrLines(c.Base),
			"lower":      map[string]any{"op_id": c.Lower.OpID, "hunks": c.Lower.Hunks},
			"upper":      map[string]any{"op_id": c.Upper.OpID, "hunks": c.Upper.Hunks},
			"candidates": cands,
		})
	}
	out := map[string]any{
		"ops_count": res.OpsCount, "files": res.Files, "can_land": res.CanLand,
		"auto_merged": res.AutoMerged, "target_head": res.TargetHead.String(),
		"ws_tree": res.WSTree.String(), "ws_ops_epoch": res.WSOpsEpoch,
		"preview_id": res.PreviewID, "already_landed": res.AlreadyLanded,
		"conflicts": conflicts,
	}
	// 版本坐标与路径级分类：全部为增量字段，旧调用方可以忽略。
	if !res.BaseTree.IsZero() {
		out["base_tree"] = res.BaseTree.String()
	}
	if !res.ChildTree.IsZero() {
		out["child_tree"] = res.ChildTree.String()
	}
	if !res.ParentTreeBefore.IsZero() {
		out["parent_tree_before"] = res.ParentTreeBefore.String()
	}
	if !res.ParentTreeAfter.IsZero() {
		out["parent_tree_after"] = res.ParentTreeAfter.String()
	}
	if res.AppliedPaths != nil {
		out["applied_paths"] = res.AppliedPaths
	}
	if res.ChildOnlyChangedPaths != nil {
		out["child_only_changed_paths"] = res.ChildOnlyChangedPaths
	}
	if res.ParentOnlyChangedPaths != nil {
		out["parent_only_changed_paths"] = res.ParentOnlyChangedPaths
	}
	if res.BothChangedPaths != nil {
		out["both_changed_paths"] = res.BothChangedPaths
	}
	if len(res.AutoMergedPaths) > 0 {
		out["auto_merged_paths"] = res.AutoMergedPaths
	}
	if len(res.ConflictPaths) > 0 {
		out["conflict_paths"] = res.ConflictPaths
	}
	if len(res.OutOfScopePaths) > 0 {
		out["out_of_scope_paths"] = res.OutOfScopePaths
	}
	if len(res.Paths) > 0 {
		out["paths"] = res.Paths
	}
	if len(res.PostApplyMismatch) > 0 {
		out["post_apply_mismatch"] = res.PostApplyMismatch
	}
	if !res.VersionID.IsZero() {
		out["version_id"] = res.VersionID.String()
	}
	if res.State != "" {
		out["state"] = res.State
	}
	if len(res.Warnings) > 0 {
		out["warnings"] = res.Warnings
	}
	if res.Line != "" {
		out["line"] = res.Line
	}
	if res.ParentWSID != "" {
		out["parent_ws_id"] = res.ParentWSID
	}
	if !res.LandedVersion.IsZero() {
		out["landed_version"] = res.LandedVersion.String()
	}
	if !res.CurrentLineHead.IsZero() {
		out["current_line_head"] = res.CurrentLineHead.String()
	}
	if res.Replayed {
		out["replayed"] = true
	}
	if res.Reason != "" {
		out["reason"] = res.Reason
	}
	if res.DroppedAt != 0 {
		out["dropped_at"] = res.DroppedAt
	}
	if rr != nil {
		out["durable"] = false
		out["durability"] = durabilityView(rr)
	}
	return out
}

func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Blocks > 0 {
				total += st.Blocks * 512
			} else {
				total += info.Size()
			}
		}
		return nil
	})
	return total, err
}

func reapWorkspaces(r *repo.Repo, olderThan, state, namePrefix string, dry bool) (map[string]any, error) {
	all, err := r.WS.List()
	if err != nil {
		return nil, err
	}
	leases, _ := r.DB.ListLeases()
	leaseCount := map[string]int{}
	for _, l := range leases {
		leaseCount[l.WsID]++
	}
	now := time.Now()
	cutoff := int64(0)
	if olderThan != "" {
		d, err := time.ParseDuration(olderThan)
		if err != nil {
			return nil, coded("invalid_argument", "older_than 解析失败: %v", err)
		}
		cutoff = now.Add(-d).UnixMilli()
	}
	var out []string
	var candidates []map[string]any
	var skipped []map[string]any
	failed := map[string]string{}
	for i := range all {
		w := &all[i]
		if state != "" && w.State != state {
			continue
		}
		if namePrefix != "" && !strings.HasPrefix(w.Name, namePrefix) {
			continue
		}
		// 默认只回收已落地/隐藏的工作区；broken 是显式 state=broken 时的例外，
		// 否则「挂载丢失留下的死工作区」永远占着目录。
		explicitBroken := state == meta.StateBroken && w.State == meta.StateBroken
		if w.LandedTS == 0 && !w.Hidden && !explicitBroken {
			continue // 用户活跃工作区绝不自动回收
		}
		reason, why := "unknown", "reap"
		if w.LandedTS != 0 {
			reason, why = "landed_and_dropped", "landed"
		} else if w.Hidden {
			reason, why = "expired", "hidden_ttl"
		} else if explicitBroken {
			reason, why = "unknown", "broken"
		}
		effectiveCutoff := cutoff
		if w.Hidden && effectiveCutoff == 0 {
			effectiveCutoff = now.Add(-24 * time.Hour).UnixMilli()
		}
		if effectiveCutoff > 0 && w.UpdatedTS > effectiveCutoff {
			skipped = append(skipped, map[string]any{
				"ws_id": w.ID, "name": w.Name, "reason": "not_old_enough",
				"age_ms": now.UnixMilli() - w.CreatedTS,
			})
			continue
		}
		if leaseCount[w.ID] > 0 {
			skipped = append(skipped, map[string]any{
				"ws_id": w.ID, "name": w.Name, "reason": "lease_held",
				"leases": leaseCount[w.ID],
			})
			continue
		}
		ops, _ := r.DB.CountOps(w.ID)
		line, lineHead := "", ""
		if info, ok, _ := r.DB.GetLandedWSInfo(w.ID); ok {
			if info.TargetKind == "line" {
				line = info.TargetID
				lineHead = info.VersionID
			}
			if lineHead == "" {
				lineHead = info.VersionID
			}
		}
		candidates = append(candidates, map[string]any{
			"ws_id": w.ID, "name": w.Name, "state": w.State,
			"reason": reason, "why": why,
			"age_ms": now.UnixMilli() - w.CreatedTS, "ops": ops,
			"dirty": ops > 0, "landed": w.LandedTS != 0,
			"line": line, "line_head": lineHead,
			"leases": leaseCount[w.ID], "will_drop": true,
		})
		out = append(out, w.ID)
		if !dry {
			if _, _, err := r.WS.DropWithOptions(w.ID, workspace.DropOptions{
				Cascade: true, Reason: reason, Why: why,
				Line: line, LineHead: lineHead,
			}); err != nil && !errors.Is(err, workspace.ErrNotFound) {
				failed[w.ID] = err.Error()
			}
		}
	}
	res := map[string]any{"dry_run": dry, "workspaces": out, "count": len(out),
		"candidates": candidates, "skipped": skipped}
	if len(failed) > 0 {
		res["failed"] = failed
	}
	return res, nil
}

var _ = fmt.Sprintf

// parseParentArg 接受 {kind,id} 或旧字符串。
func parseParentArg(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", coded("invalid_argument", "parent 解析失败: %v", err)
		}
		return s, nil
	}
	var o struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return "", coded("invalid_argument", "parent 解析失败: %v", err)
	}
	switch o.Kind {
	case "line":
		return "line:" + o.ID, nil
	case "version":
		return "version:" + o.ID, nil
	case "workspace":
		return "workspace:" + o.ID, nil
	case "mainline":
		return "", nil
	}
	return "", coded("target_invalid", "未知 parent.kind %q", o.Kind)
}

// parseTargetArg 解析 target；省略时默认 mainline。
func parseTargetArg(raw json.RawMessage) (string, string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "mainline", "", nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", "", coded("invalid_argument", "target 解析失败: %v", err)
		}
		if s == "" || s == "mainline" {
			return "mainline", "", nil
		}
		if len(s) > 5 && s[:5] == "line:" {
			return "line", s[5:], nil
		}
		if len(s) > 8 && s[:8] == "version:" {
			return "version", s[8:], nil
		}
		if len(s) > 10 && s[:10] == "workspace:" {
			return "workspace", s[10:], nil
		}
		return "line", s, nil
	}
	var o struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return "", "", coded("invalid_argument", "target 解析失败: %v", err)
	}
	if o.Kind == "mainline" {
		return "mainline", "", nil
	}
	switch o.Kind {
	case "line", "version", "workspace":
		if o.ID == "" {
			return "", "", coded("target_invalid", "target.id 不能为空")
		}
		return o.Kind, o.ID, nil
	}
	return "", "", coded("target_invalid", "未知 target.kind %q", o.Kind)
}

func (s *Server) registerVersionGraph() {
	s.register("version_graph", "返回版本 DAG（line_graph 的别名）。",
		`{"type":"object","properties":{"repo":{"type":"string"},"lines":{"type":"array","items":{"type":"string"}},"limit":{"type":"integer"}},"required":["repo"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				Lines []string `json:"lines"`
				Limit int      `json:"limit"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			return rr.LineGraph(a.Lines, a.Limit)
		})
	s.register("version_compare", "比较两个 ref（version/line/mainline）。",
		`{"type":"object","properties":{"repo":{"type":"string"},"a":{"type":"string"},"b":{"type":"string"}},"required":["repo","a","b"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				A string `json:"a"`
				B string `json:"b"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			la, err := refAsLine(rr, a.A)
			if err != nil {
				return nil, err
			}
			lb, err := refAsLine(rr, a.B)
			if err != nil {
				return nil, err
			}
			return rr.CompareLines(la, lb)
		})
}

func refAsLine(r *repo.Repo, ref string) (string, error) {
	if ref == "" || ref == "mainline" {
		return r.DB.DefaultLine()
	}
	if len(ref) > 5 && ref[:5] == "line:" {
		return ref[5:], nil
	}
	if len(ref) > 8 && ref[:8] == "version:" {
		ref = ref[8:]
	}
	// version ref: 找到唯一包含它的 line；否则报错。
	id, err := r.ResolveVersionRef(ref)
	if err == nil {
		ls, _ := r.Lines()
		for i := range ls {
			ok, _ := r.DB.IsAncestor(id, ls[i].Head)
			if ok {
				return ls[i].Name, nil
			}
		}
		return "", coded("line_not_found", "版本 %s 不属于任何 line", ref)
	}
	return ref, nil
}

func (s *Server) registerRepoExport() {
	s.register("repo_export",
		"把 line/version 物化到目录（迁移/部署导出）。async=true 时返回 job_id，用 repo_export_status 轮询。",
		`{"type":"object","properties":{"repo":{"type":"string"},"ref":{"type":"string"},"dest_dir":{"type":"string"},"async":{"type":"boolean"}},"required":["repo","ref","dest_dir"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				Ref     string `json:"ref"`
				DestDir string `json:"dest_dir"`
				Async   bool   `json:"async"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			if strings.TrimSpace(a.Ref) == "" {
				return nil, coded("invalid_argument", "ref 不能为空")
			}
			if strings.TrimSpace(a.DestDir) == "" {
				return nil, coded("invalid_argument", "dest_dir 不能为空")
			}
			if a.Async {
				id, state := s.startExportJob(rr, repoArg(raw), a.Ref, a.DestDir)
				return map[string]any{
					"job_id": id, "state": state, "dest_dir": a.DestDir,
				}, nil
			}
			start := nowMS()
			res, err := rr.Export(a.Ref, a.DestDir)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"exported": true, "ref": a.Ref, "dest_dir": res.DestDir,
				"version_id": res.VersionID.String(), "files": res.Files,
				"bytes": res.Bytes, "duration_ms": nowMS() - start,
			}, nil
		})

	s.register("repo_export_status", "查询异步 repo_export 任务状态。",
		`{"type":"object","properties":{"job_id":{"type":"string"}},"required":["job_id"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			var a struct {
				JobID string `json:"job_id"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			return s.exportJobStatus(a.JobID)
		})
}

func (s *Server) registerWSDiskUsage() {
	s.register("ws_disk_usage", "按工作区返回磁盘占用。",
		`{"type":"object","properties":{"repo":{"type":"string"}},"required":["repo"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			all, err := rr.WS.List()
			if err != nil {
				return nil, err
			}
			out := make([]map[string]any, 0, len(all))
			var total int64
			for i := range all {
				st, err := rr.WS.Info(all[i].ID)
				if err != nil {
					continue
				}
				total += st.DiskBytes
				out = append(out, map[string]any{"ws_id": st.ID, "disk_bytes": st.DiskBytes,
					"materialize": st.Materialize, "hidden": st.Hidden})
			}
			return map[string]any{"workspaces": out, "total_bytes": total}, nil
		})
}
