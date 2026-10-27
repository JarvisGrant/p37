package mcpsrv

import (
	"context"
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/hub"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/remotesync"
	"github.com/chenyiwen/p37/internal/repo"
)

// registerRepoHealth 注册启动自检聚合工具。
//
// 0.4.3 起它是 P36 /ready 的廉价入口：
//   - 未初始化返回 ok=true + initialized=false + next_action，不返回 5xx；
//   - with_quota=false 时不打开 repo、不跑 EnsureSchema/migrate；
//   - repo 未注册时可用 path 探测（attach 失败后 registry 可能还没有行）。
func (s *Server) registerRepoHealth() {
	s.register("repo_health",
		"返回 repo 初始化完整性、路径/挂载、quota、工作区计数、line heads 与告警。只读。",
		`{"type":"object","properties":{
		  "path":{"type":"string","description":"repo 未注册时可直接用挂载路径探测"},
		  "deep":{"type":"boolean","default":false,"description":"额外校验 head 的对象树（doctor 用）"},
		  "with_quota":{"type":"boolean","default":true,"description":"P36 /ready 可传 false 走只读快路径"}
		},"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			var a struct {
				Repo      string `json:"repo"`
				Path      string `json:"path"`
				Deep      bool   `json:"deep"`
				WithQuota *bool  `json:"with_quota"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			withQuota := true
			if a.WithQuota != nil {
				withQuota = *a.WithQuota
			}
			if s.Hub == nil {
				if s.R == nil {
					return nil, coded("repo_not_found", "没有可用的仓库")
				}
				in := repo.IntegrityOpen(s.R)
				out := healthBase(in, nil, s.R.Root)
				if in.Initialized {
					s.addHealthDetails(out, s.R, nil)
				}
				return out, nil
			}

			in, err := s.Hub.Health(a.Repo, a.Path, a.Deep)
			if err != nil {
				return nil, err
			}
			var entry *hub.Entry
			if a.Repo != "" {
				if e, gerr := s.Hub.Get(a.Repo); gerr == nil {
					entry = e
				}
			}
			out := healthBase(in, entry, in.Path)
			if in.Initialized && withQuota && a.Repo != "" &&
				entry != nil && filepath.Clean(entry.Path) != filepath.Clean(in.Path) {
				appendWarning(out, "repo path changed; quota skipped until rebind/re-register")
			} else if in.Initialized && withQuota && a.Repo != "" {
				rr, oerr := s.Hub.Open(a.Repo)
				if oerr != nil {
					appendWarning(out, "repo open failed: "+oerr.Error())
				} else {
					s.addHealthDetails(out, rr, entry)
				}
			}
			return out, nil
		})
}

func healthBase(in *repo.Integrity, e *hub.Entry, path string) map[string]any {
	if path == "" && e != nil {
		path = e.Path
	}
	repoInfo := map[string]any{
		"name": "", "path": path, "available": true, "degraded": false, "mount_point": false,
	}
	if e != nil {
		repoInfo["id"] = e.ID
		repoInfo["name"] = e.Name
		repoInfo["path"] = e.Path
		repoInfo["available"] = e.Available
		repoInfo["degraded"] = e.Degraded
		repoInfo["mount_point"] = e.Mounted
		if path != "" {
			// 调用方显式给了 path（P36 /ready）时以它为准，避免 registry
			// 还指向旧路径时把探测结果标错。
			repoInfo["path"] = path
		}
	} else if path != "" {
		repoInfo["name"] = filepath.Base(path)
		if m, merr := repo.ReadMeta(path); merr == nil && m != nil {
			repoInfo["id"] = m.ID
			repoInfo["name"] = m.Name
		}
		if pm, perr := mountProbe(path); perr == nil {
			repoInfo["mount_point"] = pm
		}
	}
	persist := map[string]any{
		"truth_source": "local_repo",
		"local": map[string]any{
			"root": path, "available": true,
		},
		"durable": false, "durable_scope": "local_only",
	}
	warnings := append([]string{}, in.Warnings...)
	if !in.Initialized {
		reason := in.Reason
		if reason == "" {
			reason = "repo_not_initialized"
		}
		warnings = append(warnings, reason)
	}
	return map[string]any{
		"repo":               repoInfo,
		"initialized":        in.Initialized,
		"reason":             in.Reason,
		"next_action":        in.NextAction,
		"path":               path,
		"default_line":       in.DefaultLine,
		"default_line_ready": in.DefaultLineReady,
		"mainline_head":      in.MainlineHead,
		"version_count":      in.VersionCount,
		"user_files":         in.UserFiles,
		"root_count":         in.RootCount,
		"root_tree_empty":    in.RootTreeEmpty,
		"history_not_empty":  in.HistoryNotEmpty,
		"active_workspaces":  in.ActiveWorkspaces,
		"leases":             in.Leases,
		"data_ok":            in.DataOK,
		"repo_db_ok":         in.RepoDBOK,
		"repo_json_ok":       in.RepoJSONOK,
		"root_ok":            in.RootOK,
		"refs_ok":            in.RefsOK,
		"refs_head":          in.RefsHead,
		"refs_match":         in.RefsMatch,
		"format_ok":          in.FormatOK,
		"format_ver":         in.FormatVer,
		"p37_version":        Version,
		"persistence":        persist,
		"quota":              nil,
		"workspaces":         map[string]any{"total": 0},
		"lines": map[string]any{
			"count": 0, "default_line": in.DefaultLine, "heads": map[string]string{},
		},
		"warnings": warnings,
	}
}

func (s *Server) addHealthDetails(out map[string]any, rr *repo.Repo, entry *hub.Entry) {
	st, err := rr.Status()
	if err != nil {
		appendWarning(out, "status failed: "+err.Error())
		return
	}
	q, err := rr.WS.Quota()
	if err != nil {
		appendWarning(out, "quota failed: "+err.Error())
	}
	ws, err := rr.WS.List()
	if err != nil {
		appendWarning(out, "workspace list failed: "+err.Error())
	}
	lines, err := rr.Lines()
	if err != nil {
		appendWarning(out, "line list failed: "+err.Error())
	}
	cfg, _ := config.Load(rr.Root)
	rs, _ := remotesync.GetStatus(rr, "", cfg.Remote.Enabled)
	now := time.Now().UnixMilli()
	tombs, _ := rr.DB.CountWSTombstones(now)
	defaultLine, _ := rr.DB.DefaultLine()

	counts := map[string]any{
		"total": len(ws), "active": 0, "conflicted": 0, "landed": 0,
		"broken": 0, "hidden": 0, "tombstones": tombs,
	}
	for i := range ws {
		w := &ws[i]
		if w.Hidden {
			counts["hidden"] = counts["hidden"].(int) + 1
		}
		if w.LandedTS != 0 {
			counts["landed"] = counts["landed"].(int) + 1
			continue
		}
		switch w.State {
		case "active":
			counts["active"] = counts["active"].(int) + 1
		case "conflicted":
			counts["conflicted"] = counts["conflicted"].(int) + 1
		case "broken":
			counts["broken"] = counts["broken"].(int) + 1
		}
	}
	heads := map[string]string{}
	for i := range lines {
		heads[lines[i].Name] = lines[i].Head.String()
	}
	persist := persistenceView(rr)
	persist["local_committed"] = true
	if rs != nil {
		persist["pending_packages"] = rs.PendingPackages
		persist["ahead"] = rs.Ahead
		persist["behind"] = rs.Behind
		persist["diverged"] = rs.Diverged
		persist["local_head"] = rs.LocalHead
	}
	in := repo.IntegrityOpen(rr)
	out["initialized"] = in.Initialized
	out["reason"] = in.Reason
	out["next_action"] = in.NextAction
	out["default_line"] = defaultLine
	out["default_line_ready"] = in.DefaultLineReady
	out["mainline_head"] = st.MainlineHead.String()
	out["version_count"] = st.VersionCount
	out["user_files"] = in.UserFiles
	out["root_count"] = in.RootCount
	out["root_tree_empty"] = in.RootTreeEmpty
	out["history_not_empty"] = in.HistoryNotEmpty
	out["active_workspaces"] = in.ActiveWorkspaces
	out["leases"] = in.Leases
	out["data_ok"] = in.DataOK
	out["repo_db_ok"] = in.RepoDBOK
	out["repo_json_ok"] = in.RepoJSONOK
	out["root_ok"] = in.RootOK
	out["refs_ok"] = in.RefsOK
	out["refs_match"] = in.RefsMatch
	out["format_ok"] = in.FormatOK
	out["format_ver"] = st.FormatVer
	out["p37_version"] = Version
	out["persistence"] = persist
	out["quota"] = q
	out["workspaces"] = counts
	out["lines"] = map[string]any{
		"count": len(lines), "default_line": defaultLine, "heads": heads,
	}
	out["last_land_ts"] = st.LastLandTS
	for _, w := range in.Warnings {
		appendWarning(out, w)
	}

	repoInfo, _ := out["repo"].(map[string]any)
	if repoInfo == nil {
		repoInfo = map[string]any{}
		out["repo"] = repoInfo
	}
	repoInfo["id"] = repoID(rr)
	repoInfo["name"] = repoLabel(rr)
	repoInfo["path"] = rr.Root
	repoInfo["mount_point"] = mountmgr.IsMountPoint(rr.Root)
	repoInfo["available"] = true
	repoInfo["degraded"] = false
	if entry != nil {
		repoInfo["id"] = entry.ID
		repoInfo["name"] = entry.Name
		repoInfo["path"] = entry.Path
		repoInfo["mount_point"] = entry.Mounted
		repoInfo["available"] = entry.Available
		repoInfo["degraded"] = entry.Degraded
		if !entry.Available {
			appendWarning(out, "repo path unavailable")
		}
		if entry.Degraded {
			appendWarning(out, "repo degraded")
		}
	}
	if q != nil && q.ActiveAvailable == 0 {
		appendWarning(out, "workspace quota exhausted")
	}
	if q != nil && q.Broken > 0 {
		appendWarning(out, "broken workspaces need repair")
	}
	if rs != nil && rs.PendingPackages > 0 {
		appendWarning(out, "pending sync packages")
	}
}

func appendWarning(out map[string]any, msg string) {
	ws, _ := out["warnings"].([]string)
	out["warnings"] = append(ws, msg)
}

func mountProbe(path string) (bool, error) {
	return mountmgr.IsMountPoint(path), nil
}
