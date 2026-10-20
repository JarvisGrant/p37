package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/op"
	"github.com/chenyiwen/p37/internal/remotesync"
	"github.com/chenyiwen/p37/internal/repo"
	"github.com/chenyiwen/p37/internal/workspace"
)

// registerAll 注册全部工具（§11.3）。
func (s *Server) registerAll() {
	s.registerRead()
	s.registerWorkspace()
	s.registerConflict()
	s.registerLand()
	s.registerSync()
	s.registerRepoLineRead()
	s.registerVersionGraph()
	s.registerSyncV2()
	s.registerRepoExport()
	s.registerWSDiskUsage()
}

// --- 只读 ---------------------------------------------------------------

func (s *Server) registerRead() {
	s.register("repo_status",
		"返回仓库与（可选）工作区的当前状态，含租约列表。",
		`{"type":"object","properties":{"ws":{"type":"string","description":"工作区 id"}},"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, rerr := s.repoFor(repoArg(raw))
			if rerr != nil {
				return nil, rerr
			}
			a, err := decode[struct {
				Ws string `json:"ws"`
			}](raw)
			if err != nil {
				return nil, err
			}
			st, err := rr.Status()
			if err != nil {
				return nil, err
			}
			leases, err := rr.Leases()
			if err != nil {
				return nil, err
			}
			out := map[string]any{
				"repo": map[string]any{
					"root":          st.Root,
					"mainline_head": st.MainlineHead.String(),
					"version_count": st.VersionCount,
					"epoch":         st.Epoch,
					"last_land_ts":  st.LastLandTS,
					"format_ver":    st.FormatVer,
					"has_head":      st.HasHead,
				},
				"leases": leases,
			}
			if a.Ws != "" {
				info, err := rr.WS.Info(a.Ws)
				if err != nil {
					return nil, err
				}
				violations, _ := rr.LeaseViolations(a.Ws)
				out["ws"] = map[string]any{
					"id": info.ID, "parent_kind": info.ParentKind, "parent_id": info.ParentID,
					"base_version": info.BaseVersion.String(), "dir": info.Dir,
					"dirty": info.Dirty, "captured_ops": info.CapturedOps,
					"conflict_count": info.ConflictCount, "state": info.State,
					"materialize":      info.Materialize,
					"lease_violations": violations,
				}
			} else {
				out["ws"] = nil
			}
			return out, nil
		})

	s.register("version_log",
		"按 ref（line/mainline/version）返回版本列表；默认 default line。",
		`{"type":"object","properties":{"repo":{"type":"string"},"ref":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":500},"before_ts":{"type":"integer"},"before_id":{"type":"string"}},"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				Ref      string `json:"ref"`
				Limit    int    `json:"limit"`
				BeforeTS int64  `json:"before_ts"`
				BeforeID string `json:"before_id"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			if a.Limit <= 0 {
				a.Limit = 50
			}
			if a.Limit > 500 {
				a.Limit = 500
			}
			if a.Ref == "" {
				a.Ref = "mainline"
			}
			line := strings.TrimPrefix(a.Ref, "line:")
			if a.Ref == "mainline" {
				line, _ = rr.DB.DefaultLine()
			}
			vs, err := rr.LineLog(line, a.Limit, a.BeforeTS, a.BeforeID)
			if err != nil {
				return nil, err
			}
			return versionSummaries(vs), nil
		})

	s.register("version_show",
		"返回单个版本的详情（含相对父版本的文件级统计）。",
		`{"type":"object","properties":{"version_id":{"type":"string"}},"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, rerr := s.repoFor(repoArg(raw))
			if rerr != nil {
				return nil, rerr
			}
			a, err := decode[struct {
				VersionID string `json:"version_id"`
			}](raw)
			if err != nil {
				return nil, err
			}
			id, err := s.resolveVersion(rr, a.VersionID)
			if err != nil {
				return nil, err
			}
			return rr.VersionShow(id)
		})

	s.register("version_diff",
		"返回两个版本之间的差异；B 省略时相对指定 parent_index。",
		`{"type":"object","properties":{"repo":{"type":"string"},"a":{"type":"string"},"b":{"type":"string"},"parent_index":{"type":"integer","minimum":0},"path":{"type":"string"},"stat_only":{"type":"boolean"}},"required":["repo","a"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, rerr := s.repoFor(repoArg(raw))
			if rerr != nil {
				return nil, rerr
			}
			a, err := decode[struct {
				A           string `json:"a"`
				B           string `json:"b"`
				ParentIndex int    `json:"parent_index"`
				Path        string `json:"path"`
				StatOnly    bool   `json:"stat_only"`
			}](raw)
			if err != nil {
				return nil, err
			}
			aid, err := s.resolveVersion(rr, a.A)
			if err != nil {
				return nil, err
			}
			if a.B != "" {
				bid, err := s.resolveVersion(rr, a.B)
				if err != nil {
					return nil, err
				}
				return rr.VersionDiff(aid, bid, a.Path, a.StatOnly)
			}
			v, err := rr.DB.GetVersion(aid)
			if err != nil {
				return nil, err
			}
			ps := v.Parents
			if len(ps) == 0 && !v.Parent.IsZero() {
				ps = []hash.ID{v.Parent}
			}
			if a.ParentIndex < 0 || a.ParentIndex >= len(ps) {
				return nil, coded("invalid_argument", "parent_index 越界")
			}
			return rr.VersionDiff(ps[a.ParentIndex], aid, a.Path, a.StatOnly)
		})

	s.register("lease_query",
		"查询若干路径上的租约（改动前自查，§4.5）。",
		`{"type":"object","properties":{"paths":{"type":"array","items":{"type":"string"}}},"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, rerr := s.repoFor(repoArg(raw))
			if rerr != nil {
				return nil, rerr
			}
			a, err := decode[struct {
				Paths []string `json:"paths"`
			}](raw)
			if err != nil {
				return nil, err
			}
			leases, err := rr.Leases()
			if err != nil {
				return nil, err
			}
			out := []map[string]string{}
			for _, l := range leases {
				for _, p := range a.Paths {
					held := l["path"]
					if held == p || len(held) > 0 && len(p) > len(held) &&
						p[:len(held)] == held && p[len(held)] == '/' {
						out = append(out, l)
						break
					}
				}
			}
			return out, nil
		})

	s.register("blame",
		"行/hunk 级归因：返回每一行由哪个版本、哪个 agent、哪次任务引入（§12.2）。",
		`{"type":"object","properties":{"path":{"type":"string"},"range":{"type":"array","items":{"type":"integer"}},"version_id":{"type":"string"},"granularity":{"type":"string","enum":["line","hunk"]}},"required":["path"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, rerr := s.repoFor(repoArg(raw))
			if rerr != nil {
				return nil, rerr
			}
			a, err := decode[struct {
				Path        string `json:"path"`
				Range       []int  `json:"range"`
				VersionID   string `json:"version_id"`
				Granularity string `json:"granularity"`
			}](raw)
			if err != nil {
				return nil, err
			}
			if a.Path == "" {
				return nil, coded("invalid_argument", "path 不能为空")
			}
			vid, err := s.resolveVersion(rr, a.VersionID)
			if err != nil {
				return nil, err
			}
			opt := repo.BlameOptions{VersionID: vid, Granularity: a.Granularity}
			if len(a.Range) == 2 {
				opt.From, opt.To = a.Range[0], a.Range[1]
			} else if len(a.Range) != 0 {
				return nil, coded("invalid_argument", "range 必须是 [from, to]")
			}
			recs, err := rr.Blame(a.Path, opt)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"path":        a.Path,
				"version_id":  vid.String(),
				"granularity": opt.Granularity,
				"records":     recs,
			}, nil
		})
}

// resolveVersion 解析版本 id；空串表示 head。
func (s *Server) resolveVersion(r *repo.Repo, id string) (hash.ID, error) {
	if id == "" || id == "head" {
		head, ok, err := r.Head()
		if err != nil {
			return hash.ID{}, err
		}
		if !ok {
			return hash.ID{}, coded("version_not_found", "主干尚未初始化")
		}
		return head, nil
	}
	if h, err := hash.Parse(id); err == nil {
		return h, nil
	}
	if len(id) >= 4 {
		h, err := r.DB.FindVersionByPrefix(id)
		if err == nil {
			return h, nil
		}
	}
	return hash.ID{}, coded("version_not_found", "非法版本 id %q", id)
}

// --- 工作区 -------------------------------------------------------------

func (s *Server) registerWorkspace() {
	s.register("ws_open",
		"打开一个工作区并物化出可直接编辑的目录（§4.1、§4.6）。",
		`{"type":"object","properties":{
		   "repo":{"type":"string"},
		   "parent":{"type":["object","string"],"description":"{kind:version|line|workspace,id} 或旧字符串；省略表示默认 line"},
		   "name":{"type":"string"},
		   "task":{"type":"string"},
		   "task_desc":{"type":"string"},
		   "leases":{"type":"array","items":{"type":"string"}},
		   "materialize":{"type":"string","enum":["auto","overlay","copy"]}
		 },"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				Parent      json.RawMessage `json:"parent"`
				Name        string          `json:"name"`
				Task        string          `json:"task"`
				TaskDesc    string          `json:"task_desc"`
				Leases      []string        `json:"leases"`
				Materialize string          `json:"materialize"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			parent, err := parseParentArg(a.Parent)
			if err != nil {
				return nil, err
			}
			task := a.TaskDesc
			if task == "" {
				task = a.Task
			}
			w, err := rr.WS.Open(workspace.OpenRequest{
				Parent: parent, Name: a.Name, TaskDesc: task,
				Leases: a.Leases, Mode: mountmgr.Mode(a.Materialize),
			})
			if err != nil {
				return nil, err
			}
			pk := w.ParentKind
			if w.BaseLine != "" {
				pk = "line"
			}
			return map[string]any{
				"ws_id": w.ID, "dir": rr.WS.Dir(w.ID),
				"parent_kind": pk, "parent_id": w.ParentID,
				"base_line": w.BaseLine, "base_version": w.BaseVersion.String(),
				"materialize": w.Materialize, "state": w.State,
			}, nil
		})

	s.register("ws_list",
		"列出全部工作区（默认不含隐藏 merge/preview 工作区）。",
		`{"type":"object","properties":{"include_hidden":{"type":"boolean"}},"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, rerr := s.repoFor(repoArg(raw))
			if rerr != nil {
				return nil, rerr
			}
			var a struct {
				IncludeHidden bool `json:"include_hidden"`
			}
			_ = json.Unmarshal(raw, &a)
			all, err := rr.WS.List()
			if err != nil {
				return nil, err
			}
			out := make([]map[string]any, 0, len(all))
			for _, w := range all {
				if w.Hidden && !a.IncludeHidden {
					continue
				}
				n, _ := rr.DB.CountOps(w.ID)
				c, _ := rr.DB.CountOpenConflicts(w.ID)
				state := w.State
				if w.LandedTS != 0 {
					state = "landed"
				}
				pk, pid := w.ParentKind, w.ParentID
				if w.BaseLine != "" {
					pk, pid = "line", w.BaseLine
				}
				out = append(out, map[string]any{
					"ws_id": w.ID, "name": w.Name, "state": state,
					"parent_kind": pk, "parent_id": pid,
					"base_version": w.BaseVersion.String(), "base_line": w.BaseLine,
					"captured_ops": n, "conflict_count": c,
					"materialize": w.Materialize, "dir": rr.WS.Dir(w.ID),
				})
			}
			return out, nil
		})

	s.register("ws_info",
		"返回单个工作区的状态。",
		`{"type":"object","properties":{"ws_id":{"type":"string"}},"required":["ws_id"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, rerr := s.repoFor(repoArg(raw))
			if rerr != nil {
				return nil, rerr
			}
			a, err := decode[struct {
				WsID string `json:"ws_id"`
			}](raw)
			if err != nil {
				return nil, err
			}
			return rr.WS.Info(a.WsID)
		})

	s.register("ws_lease",
		"登记或解除路径租约（建议性，§4.5）。",
		`{"type":"object","properties":{"ws_id":{"type":"string"},"add":{"type":"array","items":{"type":"string"}},"release":{"type":"array","items":{"type":"string"}},"mode":{"type":"string","enum":["shared","exclusive"]}},"required":["ws_id"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, rerr := s.repoFor(repoArg(raw))
			if rerr != nil {
				return nil, rerr
			}
			a, err := decode[struct {
				WsID    string   `json:"ws_id"`
				Add     []string `json:"add"`
				Release []string `json:"release"`
				Mode    string   `json:"mode"`
			}](raw)
			if err != nil {
				return nil, err
			}
			if a.Mode == "" {
				a.Mode = "shared"
			}
			if _, err := rr.WS.Get(a.WsID); err != nil {
				return nil, err
			}
			for _, p := range a.Add {
				if err := rr.DB.AddLease(a.WsID, p, a.Mode, nowMS()); err != nil {
					return nil, err
				}
			}
			for _, p := range a.Release {
				if err := rr.DB.ReleaseLease(a.WsID, p); err != nil {
					return nil, err
				}
			}
			leases, err := rr.DB.ListLeases()
			if err != nil {
				return nil, err
			}
			var mine []meta.Lease
			for _, l := range leases {
				if l.WsID == a.WsID {
					mine = append(mine, l)
				}
			}
			return map[string]any{"leases": mine}, nil
		})

	s.register("ws_sync",
		"强制采集：把磁盘变更结算为 op（§5.2）。",
		`{"type":"object","properties":{"ws_id":{"type":"string"}},"required":["ws_id"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, rerr := s.repoFor(repoArg(raw))
			if rerr != nil {
				return nil, rerr
			}
			a, err := decode[struct {
				WsID string `json:"ws_id"`
			}](raw)
			if err != nil {
				return nil, err
			}
			before, _ := rr.DB.CountOps(a.WsID)
			if err := rr.WS.Sync(a.WsID); err != nil {
				return nil, err
			}
			after, _ := rr.DB.CountOps(a.WsID)
			ops, _ := rr.DB.ListOps(a.WsID)
			files := map[string]bool{}
			for _, o := range ops {
				files[o.Path] = true
			}
			// §5.2：连续多轮不稳定的文件本轮被跳过，必须让调用方看见 ——
			// 否则「采集了 0 个改动」和「有文件正在被写所以没采到」无法区分。
			skipped := rr.WS.LastUnstable(a.WsID)
			if skipped == nil {
				skipped = []string{}
			}
			conf, _ := rr.DB.CountOpenConflicts(a.WsID)
			return map[string]any{
				"captured_ops": after, "new_ops": after - before,
				"ops_count": after, "files_touched": len(files),
				"conflicts": conf, "dirty": after > 0,
				"skipped_unstable": skipped,
			}, nil
		})

	s.register("ws_drop",
		"放弃工作区；cascade 决定是否连子工作区一起处理（§11.4）。",
		`{"type":"object","properties":{"ws_id":{"type":"string"},"cascade":{"type":"boolean"},"keep_dir":{"type":"boolean"}},"required":["ws_id"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, rerr := s.repoFor(repoArg(raw))
			if rerr != nil {
				return nil, rerr
			}
			a, err := decode[struct {
				WsID    string `json:"ws_id"`
				Cascade *bool  `json:"cascade"`
				KeepDir bool   `json:"keep_dir"`
			}](raw)
			if err != nil {
				return nil, err
			}
			cascade := false
			if a.Cascade != nil {
				cascade = *a.Cascade
			}
			n, _ := rr.DB.CountOps(a.WsID)
			dropped, reparented, err := rr.WS.Drop(a.WsID, cascade, a.KeepDir)
			if err != nil {
				if errors.Is(err, workspace.ErrNotFound) {
					return map[string]any{"dropped": []string{a.WsID}, "already_dropped": true,
						"reparented": []string{}, "conflicts": []any{}, "dirty_ops_discarded": 0}, nil
				}
				return nil, err
			}
			return map[string]any{
				"dropped": dropped, "reparented": reparented,
				"conflicts": []any{}, "dirty_ops_discarded": n,
			}, nil
		})

	s.register("ws_rebase",
		"把工作区对齐到父/line/version 的最新基准（§4.9.2）。",
		`{"type":"object","properties":{"repo":{"type":"string"},"ws_id":{"type":"string"},"to":{"type":["object","string"]}},"required":["repo","ws_id"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, rerr := s.repoFor(repoArg(raw))
			if rerr != nil {
				return nil, rerr
			}
			var a struct {
				WsID string          `json:"ws_id"`
				To   json.RawMessage `json:"to"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			ref := ""
			if len(a.To) > 0 && string(a.To) != "null" {
				k, id, err := parseTargetArg(a.To)
				if err != nil {
					return nil, err
				}
				switch k {
				case "line", "mainline":
					ref = "line:" + id
					if k == "mainline" {
						ref = "mainline"
					}
				case "version":
					ref = "version:" + id
				case "workspace":
					ref = "workspace:" + id
				}
			}
			return rr.WS.RebaseTo(a.WsID, ref)
		})
}

// --- 冲突 ---------------------------------------------------------------

func (s *Server) registerConflict() {
	s.register("conflict_list",
		"列出某工作区的冲突。",
		`{"type":"object","properties":{"ws_id":{"type":"string"},"include_resolved":{"type":"boolean"}},"required":["ws_id"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, rerr := s.repoFor(repoArg(raw))
			if rerr != nil {
				return nil, rerr
			}
			a, err := decode[struct {
				WsID            string `json:"ws_id"`
				IncludeResolved bool   `json:"include_resolved"`
			}](raw)
			if err != nil {
				return nil, err
			}
			return rr.Conflicts(a.WsID, !a.IncludeResolved)
		})

	s.register("conflict_show",
		"按 key 返回冲突详情（含两侧任务描述与候选解）。",
		`{"type":"object","properties":{"key":{"type":"string"}},"required":["key"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, rerr := s.repoFor(repoArg(raw))
			if rerr != nil {
				return nil, rerr
			}
			a, err := decode[struct {
				Key string `json:"key"`
			}](raw)
			if err != nil {
				return nil, err
			}
			key, err := parseKey(a.Key)
			if err != nil {
				return nil, err
			}
			v, err := rr.ConflictByKey(key)
			if err != nil {
				return nil, err
			}
			return v, nil
		})

	s.register("conflict_resolve",
		"记录一条冲突解决；同 key 重复写入视为成功（§6.4）。",
		`{"type":"object","properties":{"key":{"type":"string"},"resolution":{"type":"object","properties":{"kind":{"type":"string","enum":["take_lower","take_upper","both","custom"]},"content":{"type":"array","items":{"type":"string"}}},"required":["kind"]}},"required":["key","resolution"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, rerr := s.repoFor(repoArg(raw))
			if rerr != nil {
				return nil, rerr
			}
			a, err := decode[struct {
				Key        string `json:"key"`
				Resolution struct {
					Kind    string   `json:"kind"`
					Content []string `json:"content"`
				} `json:"resolution"`
			}](raw)
			if err != nil {
				return nil, err
			}
			key, err := parseKey(a.Key)
			if err != nil {
				return nil, err
			}
			kind := op.ResolutionKind(a.Resolution.Kind)
			if !kind.Valid() {
				return nil, coded("internal", "非法解决方式 %q", a.Resolution.Kind)
			}
			var content [][]byte
			for _, l := range a.Resolution.Content {
				content = append(content, []byte(l))
			}
			if kind == op.ResolveCustom && len(content) == 0 {
				return nil, coded("internal", "custom 解决必须提供 content")
			}
			created, err := rr.Resolve(key, kind, content, "mcp")
			if err != nil {
				return nil, err
			}
			if created {
				_ = rr.ReopenIfNoConflicts(wsIDOfConflict(rr, key))
			}
			return map[string]any{"key": key.String(), "kind": kind, "created": created}, nil
		})

	s.register("conflict_resolve_all",
		"按策略一次性解决某工作区的全部未决冲突。",
		`{"type":"object","properties":{"ws_id":{"type":"string"},"policy":{"type":"string","enum":["prefer_lower","prefer_upper","abort"]}},"required":["ws_id","policy"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, rerr := s.repoFor(repoArg(raw))
			if rerr != nil {
				return nil, rerr
			}
			a, err := decode[struct {
				WsID   string `json:"ws_id"`
				Policy string `json:"policy"`
			}](raw)
			if err != nil {
				return nil, err
			}
			if a.Policy == "abort" {
				return map[string]any{"resolved": 0, "aborted": true}, nil
			}
			kind := op.ResolveTakeLower
			if a.Policy == "prefer_upper" {
				kind = op.ResolveTakeUpper
			}
			rows, err := rr.DB.ListConflicts(a.WsID, true)
			if err != nil {
				return nil, err
			}
			n := 0
			for i := range rows {
				created, err := rr.Resolve(rows[i].Key, kind, nil, "mcp")
				if err != nil {
					return nil, err
				}
				if created {
					n++
				}
			}
			_ = rr.ReopenIfNoConflicts(a.WsID)
			return map[string]any{"resolved": n, "aborted": false}, nil
		})
}

// --- 整合 ---------------------------------------------------------------

func (s *Server) registerLand() {
	s.register("land_preview",
		"预演 land：target.kind=line|workspace；只读，冲突会持久化供 conflict_resolve。",
		`{"type":"object","properties":{"repo":{"type":"string"},"ws_id":{"type":"string"},"target":{"type":["object","string"]},"fold":{"type":"string","enum":["self","stack"]},"expected_head":{"type":"string"},"expected_ws_tree":{"type":"string"}},"required":["repo","ws_id"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				WsID           string          `json:"ws_id"`
				Target         json.RawMessage `json:"target"`
				Fold           string          `json:"fold"`
				ExpectedHead   string          `json:"expected_head"`
				ExpectedWSTree string          `json:"expected_ws_tree"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			kind, id, err := parseTargetArg(a.Target)
			if err != nil {
				return nil, err
			}
			if kind == "workspace" {
				res, err := rr.PreviewInto(a.WsID, id)
				if err != nil {
					return nil, err
				}
				return landView2(res), nil
			}
			line := id
			if kind == "mainline" || line == "" {
				line = ""
			}
			res, err := rr.PreviewTo(a.WsID, repo.LandOptions{
				Line: line, Fold: a.Fold, ExpectedHead: a.ExpectedHead,
				ExpectedWSTree: a.ExpectedWSTree,
			})
			if err != nil && res == nil {
				return nil, err
			}
			return landView2(res), nil
		})

	s.register("ws_land",
		"把工作区落地到命名 line 或直接父工作区。",
		`{"type":"object","properties":{"repo":{"type":"string"},"ws_id":{"type":"string"},"target":{"type":["object","string"]},"fold":{"type":"string","enum":["self","stack"]},"drop_workspace":{"type":"boolean"},"expected_head":{"type":"string"},"expected_ws_tree":{"type":"string"},"expected_ws_ops_epoch":{"type":"integer"},"expected_parent_tree":{"type":"string"},"preview_id":{"type":"string"},"idempotency_key":{"type":"string"},"task":{"type":"string"}},"required":["repo","ws_id"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				WsID               string          `json:"ws_id"`
				Target             json.RawMessage `json:"target"`
				Fold               string          `json:"fold"`
				DropWorkspace      *bool           `json:"drop_workspace"`
				ExpectedHead       string          `json:"expected_head"`
				ExpectedParentTree string          `json:"expected_parent_tree"`
				ExpectedWSTree     string          `json:"expected_ws_tree"`
				ExpectedWSOpsEpoch int64           `json:"expected_ws_ops_epoch"`
				PreviewID          string          `json:"preview_id"`
				IdempotencyKey     string          `json:"idempotency_key"`
				Task               string          `json:"task"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			kind, id, err := parseTargetArg(a.Target)
			if err != nil {
				return nil, err
			}
			drop := true
			if a.DropWorkspace != nil {
				drop = *a.DropWorkspace
			}
			if kind == "workspace" {
				exp := a.ExpectedParentTree
				if exp == "" {
					exp = a.ExpectedHead
				}
				res, err := rr.LandInto(a.WsID, id, exp, a.PreviewID, a.IdempotencyKey)
				if err != nil {
					return nil, err
				}
				return landView2(res), nil
			}
			line := id
			if kind == "mainline" || line == "" {
				line = ""
			}
			res, err := rr.LandTo(a.WsID, repo.LandOptions{
				Line: line, Fold: a.Fold, DropWorkspace: drop,
				ExpectedHead: a.ExpectedHead, ExpectedWSTree: a.ExpectedWSTree,
				IdempotencyKey: a.IdempotencyKey, TaskDesc: a.Task,
			})
			if err != nil {
				if errorsIsLandConflict(err) && res != nil {
					return landView2(res), nil
				}
				return nil, err
			}
			if cfg, err := config.Load(rr.Root); err == nil && cfg.Remote.AutoPush {
				go func() { _, _ = pushRepo(context.Background(), rr, s.Hub, true, 0) }()
			}
			return landView2(res), nil
		})

	s.register("ws_land_into",
		"ws_land(target=workspace) 的显式别名。",
		`{"type":"object","properties":{"repo":{"type":"string"},"ws_id":{"type":"string"},"parent_ws_id":{"type":"string"},"expected_parent_tree":{"type":"string"},"preview_id":{"type":"string"},"idempotency_key":{"type":"string"}},"required":["repo","ws_id","parent_ws_id"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				WsID               string `json:"ws_id"`
				ParentWsID         string `json:"parent_ws_id"`
				ExpectedParentTree string `json:"expected_parent_tree"`
				PreviewID          string `json:"preview_id"`
				IdempotencyKey     string `json:"idempotency_key"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			res, err := rr.LandInto(a.WsID, a.ParentWsID, a.ExpectedParentTree, a.PreviewID, a.IdempotencyKey)
			if err != nil {
				return nil, err
			}
			return landView2(res), nil
		})

	s.register("version_revert",
		"以新版本承载对某个版本的逆变更（可指定目标 line）。",
		`{"type":"object","properties":{"repo":{"type":"string"},"version_id":{"type":"string"},"target_line":{"type":"string"},"expected_head":{"type":"string"},"idempotency_key":{"type":"string"}},"required":["repo","version_id"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				VersionID      string `json:"version_id"`
				TargetLine     string `json:"target_line"`
				ExpectedHead   string `json:"expected_head"`
				IdempotencyKey string `json:"idempotency_key"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			rv, err := rr.RevertTo(a.VersionID, a.TargetLine, a.ExpectedHead, a.IdempotencyKey)
			if err != nil {
				return nil, err
			}
			return map[string]any{"state": "done", "version_id": rv.String()}, nil
		})
}

func landView(res *repo.LandResult) map[string]any {
	conflicts := make([]map[string]any, 0, len(res.Conflicts))
	for i := range res.Conflicts {
		c := &res.Conflicts[i]
		cands := make([]map[string]string, 0, len(c.Candidat))
		for _, cd := range c.Candidat {
			cands = append(cands, map[string]string{"kind": string(cd.Kind), "note": cd.Note})
		}
		conflicts = append(conflicts, map[string]any{
			"key": c.Key.String(), "path": c.Path, "kind": string(c.Kind),
			"state":      "open",
			"base":       toStrLines(c.Base),
			"lower":      map[string]any{"op_id": c.Lower.OpID, "hunks": c.Lower.Hunks},
			"upper":      map[string]any{"op_id": c.Upper.OpID, "hunks": c.Upper.Hunks},
			"candidates": cands,
		})
	}
	out := map[string]any{
		"ops_count":   res.OpsCount,
		"files":       res.Files,
		"conflicts":   conflicts,
		"can_land":    res.CanLand,
		"auto_merged": res.AutoMerged,
	}
	if !res.VersionID.IsZero() {
		out["version_id"] = res.VersionID.String()
		out["state"] = "done"
	}
	return out
}

func toStrLines(ls [][]byte) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = string(l)
	}
	return out
}

func errorsIsLandConflict(err error) bool { return errors.Is(err, repo.ErrLandConflict) }

// --- 同步 ---------------------------------------------------------------

func (s *Server) registerSync() {
	s.register("sync_status",
		"返回本地与远端同步状态：local_head / remote_head / 待上传包 / ahead-behind。远端 push/fetch 只通过 CLI 执行。",
		`{"type":"object","additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, rerr := s.repoFor(repoArg(raw))
			if rerr != nil {
				return nil, rerr
			}
			remote, cfg, _ := remoteFor(rr)
			configured := remote != nil
			remoteHead := ""
			if remote != nil {
				if ref, err := remote.GetRef(ctx); err == nil {
					remoteHead = ref.Head
				}
			}
			st, err := remotesync.GetStatus(rr, remoteHead, configured)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"local_head": st.LocalHead, "remote_head": st.RemoteHead,
				"remote_configured": st.RemoteConfigured,
				"pending_packages":  st.PendingPackages, "last_sync_ts": st.LastSyncTS,
				"ahead": st.Ahead, "behind": st.Behind, "diverged": st.Diverged,
				"remote_key": cfg.Remote.RemoteKey, "remote_prefix": cfg.Remote.Prefix,
			}, nil
		})

	s.register("sync_prepare",
		"生成一个增量包到本地 outbox（只暂存、不上传）。force 忽略阈值，full 强制 checkpoint。",
		`{"type":"object","properties":{"force":{"type":"boolean"},"full":{"type":"boolean"}},"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, rerr := s.repoFor(repoArg(raw))
			if rerr != nil {
				return nil, rerr
			}
			a, err := decode[struct {
				Force bool `json:"force"`
				Full  bool `json:"full"`
			}](raw)
			if err != nil {
				return nil, err
			}
			release, ok := rr.BeginSync()
			if !ok {
				return nil, coded("sync_in_progress", "该仓库已有同步在运行")
			}
			defer release()
			cfg, _ := config.Load(rr.Root)
			m, err := remotesync.Prepare(rr, cfg.RemoteOptions(), a.Force, a.Full)
			if err != nil {
				if errors.Is(err, remotesync.ErrUpToDate) || errors.Is(err, remotesync.ErrNothingToPackage) {
					return map[string]any{"created": false, "reason": err.Error()}, nil
				}
				return nil, err
			}
			return map[string]any{"created": true, "manifest": m}, nil
		})

	s.register("sync_list",
		"列出本地暂存的同步包（等待上传）。",
		`{"type":"object","additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, rerr := s.repoFor(repoArg(raw))
			if rerr != nil {
				return nil, rerr
			}
			rows, err := rr.DB.ListPackages()
			if err != nil {
				return nil, err
			}
			out := make([]map[string]any, 0, len(rows))
			for _, r := range rows {
				out = append(out, packageView(r, ""))
			}
			return out, nil
		})

	s.register("sync_show",
		"查看一个暂存包的清单；verify 为真时全量校验分片与对象。",
		`{"type":"object","properties":{"id":{"type":"string"},"verify":{"type":"boolean"}},"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, rerr := s.repoFor(repoArg(raw))
			if rerr != nil {
				return nil, rerr
			}
			a, err := decode[struct {
				ID     string `json:"id"`
				Verify bool   `json:"verify"`
			}](raw)
			if err != nil {
				return nil, err
			}
			row, err := s.findPackage(rr, a.ID)
			if err != nil {
				return nil, err
			}
			m, err := remotesync.ReadStaged(*row)
			if err != nil {
				return nil, err
			}
			if a.Verify {
				if _, err := remotesync.VerifyDir(row.LocalPath); err != nil {
					return nil, err
				}
			}
			return map[string]any{"manifest": m, "verified": a.Verify}, nil
		})
}

// packageView 输出 snake_case 的包视图（附录 K 约定）。
func packageView(r meta.PackageRow, note string) map[string]any {
	return map[string]any{
		"id":             r.ID,
		"kind":           r.Kind,
		"base_version":   r.BaseVersion,
		"result_version": r.ResultVersion,
		"epoch":          r.Epoch,
		"created_ts":     r.CreatedTS,
		"object_count":   r.ObjectCount,
		"bytes":          r.ByteSize,
		"uploaded":       r.Uploaded,
		"local_path":     r.LocalPath,
		"note":           note,
	}
}

// findPackage 按 id / 唯一前缀 / last 找包记录。
func (s *Server) findPackage(r *repo.Repo, id string) (*meta.PackageRow, error) {
	if id == "" || id == "last" {
		row, err := r.DB.LastPackage()
		if err != nil {
			return nil, coded("not_found", "没有本地暂存包")
		}
		return row, nil
	}
	rows, err := r.DB.ListPackages()
	if err != nil {
		return nil, err
	}
	var hit *meta.PackageRow
	for i := range rows {
		if rows[i].ID == id || (len(id) >= 4 && strings.HasPrefix(rows[i].ID, id)) {
			if hit != nil {
				return nil, coded("invalid_argument", "包 id 前缀 %q 匹配到多个", id)
			}
			row := rows[i]
			hit = &row
		}
	}
	if hit == nil {
		return nil, coded("not_found", "找不到包 %q", id)
	}
	return hit, nil
}

// wsIDOfConflict 由 key 反查工作区 id（用于解决后恢复状态）。
func wsIDOfConflict(r *repo.Repo, key hash.ID) string {
	row, err := r.DB.ListConflictByKey(key)
	if err != nil {
		return ""
	}
	return row.WsID
}
