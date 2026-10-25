package mcpsrv

import (
	"context"
	"encoding/json"
	"time"

	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/remotesync"
)

// registerRepoHealth 注册启动自检聚合工具。
func (s *Server) registerRepoHealth() {
	s.register("repo_health",
		"一次返回 repo 路径/挂载、持久性、quota、工作区计数、line heads 与告警。只读。",
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
			q, err := rr.WS.Quota()
			if err != nil {
				return nil, err
			}
			ws, err := rr.WS.List()
			if err != nil {
				return nil, err
			}
			lines, err := rr.Lines()
			if err != nil {
				return nil, err
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
			path := st.Root
			mountPoint := mountmgr.IsMountPoint(rr.Root)
			available, degraded := true, false
			if s.Hub != nil {
				if e, err := s.Hub.Get(repoArg(raw)); err == nil {
					path, mountPoint = e.Path, e.Mounted
					available, degraded = e.Available, e.Degraded
				}
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
			warnings := []string{}
			if !available {
				warnings = append(warnings, "repo path unavailable")
			}
			if degraded {
				warnings = append(warnings, "repo degraded")
			}
			if q.ActiveAvailable == 0 {
				warnings = append(warnings, "workspace quota exhausted")
			}
			if q.Broken > 0 {
				warnings = append(warnings, "broken workspaces need repair")
			}
			if rs != nil && rs.PendingPackages > 0 {
				warnings = append(warnings, "pending sync packages")
			}
			return map[string]any{
				"repo": map[string]any{
					"id": repoID(rr), "name": repoLabel(rr), "path": path,
					"available": available, "degraded": degraded,
					"mount_point": mountPoint,
				},
				"p37_version": Version,
				"format_ver":  st.FormatVer,
				"persistence": persist,
				"quota":       q,
				"workspaces":  counts,
				"lines": map[string]any{
					"count": len(lines), "default_line": defaultLine, "heads": heads,
				},
				"last_land_ts": st.LastLandTS,
				"warnings":     warnings,
			}, nil
		})
}
