package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/op"
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
}

// --- 只读 ---------------------------------------------------------------

func (s *Server) registerRead() {
	s.register("repo_status",
		"返回仓库与（可选）工作区的当前状态，含租约列表。",
		`{"type":"object","properties":{"ws":{"type":"string","description":"工作区 id"}},"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			a, err := decode[struct {
				Ws string `json:"ws"`
			}](raw)
			if err != nil {
				return nil, err
			}
			st, err := s.R.Status()
			if err != nil {
				return nil, err
			}
			leases, err := s.R.Leases()
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
				info, err := s.R.WS.Info(a.Ws)
				if err != nil {
					return nil, err
				}
				violations, _ := s.R.LeaseViolations(a.Ws)
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
		"按时间倒序返回主干版本列表。",
		`{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":500},"before_ts":{"type":"integer"}},"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			a, err := decode[struct {
				Limit    int   `json:"limit"`
				BeforeTS int64 `json:"before_ts"`
			}](raw)
			if err != nil {
				return nil, err
			}
			if a.Limit <= 0 {
				a.Limit = 50
			}
			if a.Limit > 500 {
				a.Limit = 500
			}
			vs, err := s.R.Log(a.Limit, a.BeforeTS)
			if err != nil {
				return nil, err
			}
			out := make([]map[string]any, 0, len(vs))
			for _, v := range vs {
				out = append(out, map[string]any{
					"id": v.ID.String(), "parent": v.Parent.String(),
					"ts": v.TS, "agent": v.Agent, "task": v.Task, "epoch": v.Epoch,
				})
			}
			return out, nil
		})

	s.register("version_show",
		"返回单个版本的详情（含相对父版本的文件级统计）。",
		`{"type":"object","properties":{"version_id":{"type":"string"}},"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			a, err := decode[struct {
				VersionID string `json:"version_id"`
			}](raw)
			if err != nil {
				return nil, err
			}
			id, err := s.resolveVersion(a.VersionID)
			if err != nil {
				return nil, err
			}
			return s.R.VersionShow(id)
		})

	s.register("version_diff",
		"返回两个版本之间的差异。",
		`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"},"path":{"type":"string"},"stat_only":{"type":"boolean"}},"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			a, err := decode[struct {
				A        string `json:"a"`
				B        string `json:"b"`
				Path     string `json:"path"`
				StatOnly bool   `json:"stat_only"`
			}](raw)
			if err != nil {
				return nil, err
			}
			aid, err := s.resolveVersion(a.A)
			if err != nil {
				return nil, err
			}
			bid, err := s.resolveVersion(a.B)
			if err != nil {
				return nil, err
			}
			return s.R.VersionDiff(aid, bid, a.Path, a.StatOnly)
		})

	s.register("lease_query",
		"查询若干路径上的租约（改动前自查，§4.5）。",
		`{"type":"object","properties":{"paths":{"type":"array","items":{"type":"string"}}},"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			a, err := decode[struct {
				Paths []string `json:"paths"`
			}](raw)
			if err != nil {
				return nil, err
			}
			leases, err := s.R.Leases()
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
				return nil, coded("INVALID_ARGUMENT", "path 不能为空")
			}
			vid, err := s.resolveVersion(a.VersionID)
			if err != nil {
				return nil, err
			}
			opt := repo.BlameOptions{VersionID: vid, Granularity: a.Granularity}
			if len(a.Range) == 2 {
				opt.From, opt.To = a.Range[0], a.Range[1]
			} else if len(a.Range) != 0 {
				return nil, coded("INVALID_ARGUMENT", "range 必须是 [from, to]")
			}
			recs, err := s.R.Blame(a.Path, opt)
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
func (s *Server) resolveVersion(id string) (hash.ID, error) {
	if id == "" || id == "head" {
		head, ok, err := s.R.Head()
		if err != nil {
			return hash.ID{}, err
		}
		if !ok {
			return hash.ID{}, coded("VERSION_NOT_FOUND", "主干尚未初始化")
		}
		return head, nil
	}
	h, err := hash.Parse(id)
	if err != nil {
		return hash.ID{}, coded("VERSION_NOT_FOUND", "非法版本 id %q", id)
	}
	return h, nil
}

// --- 工作区 -------------------------------------------------------------

func (s *Server) registerWorkspace() {
	s.register("ws_open",
		"打开一个工作区并物化出可直接编辑的目录（§4.1、§4.6）。",
		`{"type":"object","properties":{
		   "parent":{"type":"string","description":"版本 id 或工作区 id；省略表示主干 head"},
		   "name":{"type":"string"},
		   "task_desc":{"type":"string"},
		   "leases":{"type":"array","items":{"type":"string"}},
		   "materialize":{"type":"string","enum":["auto","overlay","copy"]}
		 },"required":["task_desc"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			a, err := decode[struct {
				Parent      string   `json:"parent"`
				Name        string   `json:"name"`
				TaskDesc    string   `json:"task_desc"`
				Leases      []string `json:"leases"`
				Materialize string   `json:"materialize"`
			}](raw)
			if err != nil {
				return nil, err
			}
			w, err := s.R.WS.Open(workspace.OpenRequest{
				Parent:   a.Parent,
				Name:     a.Name,
				TaskDesc: a.TaskDesc,
				Leases:   a.Leases,
				Mode:     mountmgr.Mode(a.Materialize),
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"ws_id": w.ID, "dir": s.R.WS.Dir(w.ID),
				"parent_kind": w.ParentKind, "parent_id": w.ParentID,
				"base_version": w.BaseVersion.String(),
				"materialize":  w.Materialize,
			}, nil
		})

	s.register("ws_list",
		"列出全部工作区。",
		`{"type":"object","additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			all, err := s.R.WS.List()
			if err != nil {
				return nil, err
			}
			out := make([]map[string]any, 0, len(all))
			for _, w := range all {
				n, _ := s.R.DB.CountOps(w.ID)
				c, _ := s.R.DB.CountOpenConflicts(w.ID)
				out = append(out, map[string]any{
					"ws_id": w.ID, "name": w.Name, "state": w.State,
					"parent_kind": w.ParentKind, "parent_id": w.ParentID,
					"base_version": w.BaseVersion.String(),
					"captured_ops": n, "conflict_count": c,
					"materialize": w.Materialize, "dir": s.R.WS.Dir(w.ID),
				})
			}
			return out, nil
		})

	s.register("ws_info",
		"返回单个工作区的状态。",
		`{"type":"object","properties":{"ws_id":{"type":"string"}},"required":["ws_id"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			a, err := decode[struct {
				WsID string `json:"ws_id"`
			}](raw)
			if err != nil {
				return nil, err
			}
			return s.R.WS.Info(a.WsID)
		})

	s.register("ws_lease",
		"登记或解除路径租约（建议性，§4.5）。",
		`{"type":"object","properties":{"ws_id":{"type":"string"},"add":{"type":"array","items":{"type":"string"}},"release":{"type":"array","items":{"type":"string"}},"mode":{"type":"string","enum":["shared","exclusive"]}},"required":["ws_id"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
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
			if _, err := s.R.WS.Get(a.WsID); err != nil {
				return nil, err
			}
			for _, p := range a.Add {
				if err := s.R.DB.AddLease(a.WsID, p, a.Mode, nowMS()); err != nil {
					return nil, err
				}
			}
			for _, p := range a.Release {
				if err := s.R.DB.ReleaseLease(a.WsID, p); err != nil {
					return nil, err
				}
			}
			leases, err := s.R.DB.ListLeases()
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
			a, err := decode[struct {
				WsID string `json:"ws_id"`
			}](raw)
			if err != nil {
				return nil, err
			}
			before, _ := s.R.DB.CountOps(a.WsID)
			if err := s.R.WS.Sync(a.WsID); err != nil {
				return nil, err
			}
			after, _ := s.R.DB.CountOps(a.WsID)
			ops, _ := s.R.DB.ListOps(a.WsID)
			files := map[string]bool{}
			for _, o := range ops {
				files[o.Path] = true
			}
			return map[string]any{
				"captured_ops": after, "new_ops": after - before,
				"files_touched": len(files),
			}, nil
		})

	s.register("ws_drop",
		"放弃工作区；cascade 决定是否连子工作区一起处理（§11.4）。",
		`{"type":"object","properties":{"ws_id":{"type":"string"},"cascade":{"type":"boolean"},"keep_dir":{"type":"boolean"}},"required":["ws_id"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			a, err := decode[struct {
				WsID    string `json:"ws_id"`
				Cascade *bool  `json:"cascade"`
				KeepDir bool   `json:"keep_dir"`
			}](raw)
			if err != nil {
				return nil, err
			}
			cascade := true
			if a.Cascade != nil {
				cascade = *a.Cascade
			}
			n, _ := s.R.DB.CountOps(a.WsID)
			dropped, reparented, err := s.R.WS.Drop(a.WsID, cascade, a.KeepDir)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"dropped": dropped, "reparented": reparented,
				"conflicts": []any{}, "dirty_ops_discarded": n,
			}, nil
		})

	s.register("ws_rebase",
		"把工作区对齐到父/主干的最新基准（§4.9.2）。",
		`{"type":"object","properties":{"ws_id":{"type":"string"}},"required":["ws_id"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			a, err := decode[struct {
				WsID string `json:"ws_id"`
			}](raw)
			if err != nil {
				return nil, err
			}
			return s.R.WS.Rebase(a.WsID)
		})
}

// --- 冲突 ---------------------------------------------------------------

func (s *Server) registerConflict() {
	s.register("conflict_list",
		"列出某工作区的冲突。",
		`{"type":"object","properties":{"ws_id":{"type":"string"},"include_resolved":{"type":"boolean"}},"required":["ws_id"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			a, err := decode[struct {
				WsID            string `json:"ws_id"`
				IncludeResolved bool   `json:"include_resolved"`
			}](raw)
			if err != nil {
				return nil, err
			}
			return s.R.Conflicts(a.WsID, !a.IncludeResolved)
		})

	s.register("conflict_show",
		"按 key 返回冲突详情（含两侧任务描述与候选解）。",
		`{"type":"object","properties":{"key":{"type":"string"}},"required":["key"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
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
			v, err := s.R.ConflictByKey(key)
			if err != nil {
				return nil, err
			}
			return v, nil
		})

	s.register("conflict_resolve",
		"记录一条冲突解决；同 key 重复写入视为成功（§6.4）。",
		`{"type":"object","properties":{"key":{"type":"string"},"resolution":{"type":"object","properties":{"kind":{"type":"string","enum":["take_lower","take_upper","both","custom"]},"content":{"type":"array","items":{"type":"string"}}},"required":["kind"]}},"required":["key","resolution"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
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
				return nil, coded("INTERNAL", "非法解决方式 %q", a.Resolution.Kind)
			}
			var content [][]byte
			for _, l := range a.Resolution.Content {
				content = append(content, []byte(l))
			}
			if kind == op.ResolveCustom && len(content) == 0 {
				return nil, coded("INTERNAL", "custom 解决必须提供 content")
			}
			created, err := s.R.Resolve(key, kind, content, "mcp")
			if err != nil {
				return nil, err
			}
			if created {
				_ = s.R.ReopenIfNoConflicts(wsIDOfConflict(s.R, key))
			}
			return map[string]any{"key": key.String(), "kind": kind, "created": created}, nil
		})

	s.register("conflict_resolve_all",
		"按策略一次性解决某工作区的全部未决冲突。",
		`{"type":"object","properties":{"ws_id":{"type":"string"},"policy":{"type":"string","enum":["prefer_lower","prefer_upper","abort"]}},"required":["ws_id","policy"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
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
			rows, err := s.R.DB.ListConflicts(a.WsID, true)
			if err != nil {
				return nil, err
			}
			n := 0
			for i := range rows {
				created, err := s.R.Resolve(rows[i].Key, kind, nil, "mcp")
				if err != nil {
					return nil, err
				}
				if created {
					n++
				}
			}
			_ = s.R.ReopenIfNoConflicts(a.WsID)
			return map[string]any{"resolved": n, "aborted": false}, nil
		})
}

// --- 整合 ---------------------------------------------------------------

func (s *Server) registerLand() {
	s.register("land_preview",
		"纯只读预演：返回若此刻落地会产生的冲突与文件统计（§7.5、D22）。",
		`{"type":"object","properties":{"ws_id":{"type":"string"}},"required":["ws_id"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			a, err := decode[struct {
				WsID string `json:"ws_id"`
			}](raw)
			if err != nil {
				return nil, err
			}
			res, err := s.R.Preview(a.WsID)
			if err != nil {
				return nil, err
			}
			return landView(res), nil
		})

	s.register("ws_land",
		"把工作区所在的折叠集合折叠为恰好一个版本（§7.2）。",
		`{"type":"object","properties":{"ws_id":{"type":"string"}},"required":["ws_id"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			a, err := decode[struct {
				WsID string `json:"ws_id"`
			}](raw)
			if err != nil {
				return nil, err
			}
			res, err := s.R.Land(a.WsID)
			if err != nil {
				if errorsIsLandConflict(err) && res != nil {
					return landView(res), nil
				}
				return nil, err
			}
			return landView(res), nil
		})

	s.register("version_revert",
		"以新版本承载对某个版本的逆变更（§7.6）。",
		`{"type":"object","properties":{"version_id":{"type":"string"}},"required":["version_id"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			a, err := decode[struct {
				VersionID string `json:"version_id"`
			}](raw)
			if err != nil {
				return nil, err
			}
			id, err := hash.Parse(a.VersionID)
			if err != nil {
				return nil, coded("VERSION_NOT_FOUND", "非法版本 id %q", a.VersionID)
			}
			rv, err := s.R.Revert(id)
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
		"返回本地与远端的同步状态（P2/P3；当前为占位实现）。",
		`{"type":"object","additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			head, ok, err := s.R.Head()
			if err != nil {
				return nil, err
			}
			h := ""
			if ok {
				h = head.String()
			}
			return map[string]any{
				"local_head": h, "remote_head": h,
				"pending_packages": 0, "last_sync_ts": 0,
			}, nil
		})
}

// wsIDOfConflict 由 key 反查工作区 id（用于解决后恢复状态）。
func wsIDOfConflict(r *repo.Repo, key hash.ID) string {
	row, err := r.DB.ListConflictByKey(key)
	if err != nil {
		return ""
	}
	return row.WsID
}
