package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/hub"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/repo"
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

	s.register("repo_create", "创建一个新仓库（含空 v0 与默认 line）。",
		`{"type":"object","properties":{"name":{"type":"string"},"default_line":{"type":"string"},"remote_key":{"type":"string"},"remote_prefix":{"type":"string"},"remote":{"type":"object"},"idempotency_key":{"type":"string"}},"required":["name"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			if s.Hub == nil {
				return nil, coded("repo_exists", "单仓库模式不支持 repo_create")
			}
			var a struct {
				Name           string          `json:"name"`
				DefaultLine    string          `json:"default_line"`
				RemoteKey      string          `json:"remote_key"`
				RemotePrefix   string          `json:"remote_prefix"`
				Remote         json.RawMessage `json:"remote"`
				IdempotencyKey string          `json:"idempotency_key"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			e, err := s.Hub.CreateEx(a.Name, a.DefaultLine, a.RemoteKey, a.RemotePrefix)
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
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			st, err := rr.Status()
			if err != nil {
				return nil, err
			}
			lines, _ := rr.Lines()
			ws, _ := rr.WS.List()
			visible := 0
			for i := range ws {
				if !ws[i].Hidden {
					visible++
				}
			}
			def, _ := rr.DB.DefaultLine()
			cfg, _ := config.Load(rr.Root)
			out := map[string]any{
				"root": st.Root, "default_line": def, "mainline_head": st.MainlineHead.String(),
				"version_count": st.VersionCount, "epoch": st.Epoch,
				"workspace_count": visible, "format_ver": st.FormatVer,
				"lines": lines,
				"remote": map[string]any{"enabled": cfg.Remote.Enabled,
					"backend": cfg.Remote.Backend, "remote_key": cfg.Remote.RemoteKey,
					"prefix": cfg.Remote.Prefix},
			}
			if s.Hub != nil {
				if e, err := s.Hub.Get(repoArg(raw)); err == nil {
					out["id"] = e.ID
					out["name"] = e.Name
					out["path"] = e.Path
					out["remote_key"] = e.RemoteKey
					out["remote_prefix"] = e.RemotePrefix
					out["degraded"] = e.Degraded
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

	s.register("repo_import", "把目录导入空仓库作为默认 line 的 v0。",
		`{"type":"object","properties":{"repo":{"type":"string"},"dir":{"type":"string"},"task":{"type":"string"},"idempotency_key":{"type":"string"}},"required":["repo","dir"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				Dir  string `json:"dir"`
				Task string `json:"task"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			id, err := rr.Import(a.Dir)
			if err != nil {
				return nil, err
			}
			return map[string]any{"version_id": id.String(), "dir": a.Dir}, nil
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
			out := make([]meta.LineView, 0, len(ls))
			for i := range ls {
				out = append(out, ls[i].View())
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
			return rr.WSDiff(a.WsID)
		})

	s.register("ws_reap", "清理过期/孤儿工作区，默认 dry-run。",
		`{"type":"object","properties":{"repo":{"type":"string"},"older_than":{"type":"string"},"state":{"type":"string"},"dry_run":{"type":"boolean"}},"required":["repo"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				OlderThan string `json:"older_than"`
				State     string `json:"state"`
				DryRun    *bool  `json:"dry_run"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			dry := true
			if a.DryRun != nil {
				dry = *a.DryRun
			}
			return reapWorkspaces(rr, a.OlderThan, a.State, dry)
		})
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

func landView2(res *repo.LandResult) map[string]any {
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
	if !res.VersionID.IsZero() {
		out["version_id"] = res.VersionID.String()
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
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func reapWorkspaces(r *repo.Repo, olderThan, state string, dry bool) (map[string]any, error) {
	all, err := r.WS.List()
	if err != nil {
		return nil, err
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
	for i := range all {
		w := &all[i]
		if state != "" && w.State != state {
			continue
		}
		if w.LandedTS == 0 && !w.Hidden {
			continue // 用户活跃工作区绝不自动回收
		}
		if w.Hidden && cutoff == 0 {
			cutoff = now.Add(-24 * time.Hour).UnixMilli()
		}
		if cutoff > 0 && w.UpdatedTS > cutoff {
			continue
		}
		leases, _ := r.DB.ListLeases()
		busy := false
		for _, l := range leases {
			if l.WsID == w.ID {
				busy = true
			}
		}
		if busy {
			continue
		}
		out = append(out, w.ID)
		if !dry {
			_, _, _ = r.WS.Drop(w.ID, true, false)
		}
	}
	return map[string]any{"dry_run": dry, "workspaces": out, "count": len(out)}, nil
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
	s.register("repo_export", "把 line/version 物化到目录（迁移/导出）。",
		`{"type":"object","properties":{"repo":{"type":"string"},"ref":{"type":"string"},"dest_dir":{"type":"string"}},"required":["repo","ref","dest_dir"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				Ref     string `json:"ref"`
				DestDir string `json:"dest_dir"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			if err := rr.Export(a.Ref, a.DestDir); err != nil {
				return nil, err
			}
			return map[string]any{"exported": true, "ref": a.Ref, "dest_dir": a.DestDir}, nil
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
