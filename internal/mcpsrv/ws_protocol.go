package mcpsrv

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/repo"
	"github.com/chenyiwen/p37/internal/workspace"
)

// repoLabel 返回错误上下文里可读的 repo 标识。
func repoLabel(rr *repo.Repo) string {
	if rr == nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(rr.Root, ".p37", "repo.json"))
	if err == nil {
		var m struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if json.Unmarshal(b, &m) == nil {
			if m.Name != "" {
				return m.Name
			}
			if m.ID != "" {
				return m.ID
			}
		}
	}
	return filepath.Base(rr.Root)
}

// durabilityView 声明本地真相源与远端持久性。ws_land 默认 durable=false：
// 只有同步 sync_push 成功覆盖后才算跨 VM durable。
func durabilityView(rr *repo.Repo) map[string]any {
	cfg, _ := config.Load(rr.Root)
	st, _ := rr.DB.GetSyncState()
	remoteConfigured := cfg.Remote.Enabled
	lastPush := int64(0)
	if st.LastSyncTS > 0 {
		lastPush = st.LastSyncTS
	}
	reason := "not_pushed"
	if !remoteConfigured {
		reason = "remote_not_configured"
	}
	return map[string]any{
		"truth_source":      "local_repo",
		"scope":             "local",
		"local_committed":   true,
		"offsite_durable":   false,
		"durable":           false,
		"reason":            reason,
		"remote_configured": remoteConfigured,
		"last_push_ts":      lastPush,
		"remote_head":       st.RemoteHead,
		"requires":          []string{"sync_push"},
	}
}

// persistenceView 是 repo_info / repo_status 的持久性声明。
func persistenceView(rr *repo.Repo) map[string]any {
	cfg, _ := config.Load(rr.Root)
	st, _ := rr.DB.GetSyncState()
	stt, _ := rr.Status()
	durable := false
	scope := "local_only"
	if cfg.Remote.Enabled && st.LastSyncTS > 0 {
		if !stt.HasHead || st.RemoteHead == stt.MainlineHead.String() {
			durable = true
			scope = "offsite"
		}
	}
	return map[string]any{
		"truth_source": "local_repo",
		"local": map[string]any{
			"root": stt.Root, "available": true, "format_ver": stt.FormatVer,
		},
		"remote": map[string]any{
			"configured": cfg.Remote.Enabled, "remote_key": cfg.Remote.RemoteKey,
			"prefix": cfg.Remote.Prefix, "last_push_ts": st.LastSyncTS,
			"remote_head": st.RemoteHead, "last_package_id": st.LastPackageID,
		},
		"durable":       durable,
		"durable_scope": scope,
	}
}

// alreadyLandedData 把 landed / landed-and-dropped 统一成 P34 可幂等消费的形状。
func alreadyLandedData(rr *repo.Repo, life *repo.WSLifecycle) map[string]any {
	res := life.LandedView()
	out := landView2(rr, res)
	out["ws_id"] = ""
	if life.Workspace != nil {
		out["ws_id"] = life.Workspace.ID
		out["name"] = life.Workspace.Name
		out["dir"] = repoWorkspaceDir(rr, life.Workspace.ID)
		out["materialize"] = life.Workspace.Materialize
	}
	if life.Tombstone != nil {
		out["ws_id"] = life.Tombstone.WSID
		out["name"] = life.Tombstone.Name
		out["reason"] = life.Tombstone.Reason
		out["dropped_at"] = life.Tombstone.DroppedAt
		out["landed_version"] = life.Tombstone.LandedVersion
		out["target_kind"] = life.Tombstone.TargetKind
		out["target_id"] = life.Tombstone.TargetID
		out["line"] = life.Tombstone.Line
		out["line_head"] = life.Tombstone.LineHead
	}
	if life.CurrentLineHead != "" {
		out["current_line_head"] = life.CurrentLineHead
		out["line_head"] = life.CurrentLineHead
	}
	// ws_info / ws_sync 会用到这些零值字段；land_preview 也会忽略多余字段。
	out["captured_ops"] = int64(0)
	out["new_ops"] = int64(0)
	out["ops_count"] = int64(0)
	out["files_touched"] = 0
	out["conflicts"] = []any{}
	out["dirty"] = false
	out["skipped_unstable"] = []string{}
	out["already_landed"] = true
	out["state"] = "already_landed"
	out["can_land"] = true
	return out
}

// wsGoneError 只有“有 tombstone 证据”时才返回 ws_gone；完全未知仍是 ws_not_found。
func wsGoneError(rr *repo.Repo, wsID string, life *repo.WSLifecycle) *CodedError {
	if life == nil || life.Tombstone == nil {
		ce := coded("ws_not_found", "工作区不存在: %s", wsID)
		ce.Detail = map[string]any{"repo": repoLabel(rr), "ws_id": wsID,
			"reason":      "never_opened_or_tombstone_expired",
			"legacy_code": "ws_not_found", "retryable": false}
		return ce
	}
	t := life.Tombstone
	detail := map[string]any{
		"repo": repoLabel(rr), "name": t.Name, "ws_id": t.WSID,
		"reason": t.Reason, "why": t.Why, "dropped_at": t.DroppedAt,
		"state_before": t.StateBefore, "landed_version": t.LandedVersion,
		"target_kind": t.TargetKind, "target_id": t.TargetID,
		"line": t.Line, "line_head": t.LineHead,
		"tombstone_expires_at": t.ExpiresAt,
		"legacy_code":          "ws_not_found", "retryable": false,
	}
	if life.CurrentLineHead != "" {
		detail["current_line_head"] = life.CurrentLineHead
	}
	ce := coded("ws_gone", "工作区已被回收")
	ce.Detail = detail
	return ce
}

// wsLifecycleOrError 统一把 live/landed/gone 转成 MCP 可用结果。
func wsLifecycleOrError(rr *repo.Repo, wsID string, allowLanded bool) (*repo.WSLifecycle, any, error) {
	life, err := rr.WSLifecycleOf(wsID)
	if err != nil {
		return nil, nil, err
	}
	switch life.State {
	case repo.WSLive:
		return life, nil, nil
	case repo.WSLanded, repo.WSGoneLanded:
		if allowLanded {
			return life, alreadyLandedData(rr, life), nil
		}
		return life, nil, wsGoneError(rr, wsID, life)
	default:
		return life, nil, wsGoneError(rr, wsID, life)
	}
}

// landErrorView 在 land 返回 NotFound 时用 tombstone/landed_ws 转成可判定结果。
func landErrorView(rr *repo.Repo, wsID string, err error) (any, bool, error) {
	if !errors.Is(err, workspace.ErrNotFound) && !errors.Is(err, meta.ErrNotExist) {
		return nil, false, nil
	}
	life, lerr := rr.WSLifecycleOf(wsID)
	if lerr != nil {
		return nil, true, lerr
	}
	if life.State == repo.WSGoneLanded || life.State == repo.WSLanded {
		return alreadyLandedData(rr, life), true, nil
	}
	return nil, true, wsGoneError(rr, wsID, life)
}

// wsOpenView 是 ws_open / ws_ensure 的统一返回。
func wsOpenView(rr *repo.Repo, w *meta.Workspace, created, reused, replayed bool) map[string]any {
	pk := w.ParentKind
	if w.BaseLine != "" {
		pk = "line"
	}
	return map[string]any{
		"ws_id": w.ID, "dir": rr.WS.Dir(w.ID), "name": w.Name,
		"parent_kind": pk, "parent_id": func() string {
			if w.BaseLine != "" {
				return w.BaseLine
			}
			return w.ParentID
		}(),
		"base_line": w.BaseLine, "base_version": w.BaseVersion.String(),
		"materialize": w.Materialize, "state": w.State,
		"created": created, "reused": reused, "idempotent_replay": replayed,
		"created_ts": w.CreatedTS, "updated_ts": w.UpdatedTS,
	}
}

func claimWSID(out *workspace.OpenOutcome) string {
	if out == nil {
		return ""
	}
	if out.WSID != "" {
		return out.WSID
	}
	if out.Workspace != nil {
		return out.Workspace.ID
	}
	return ""
}

func repoWorkspaceDir(rr *repo.Repo, wsID string) string {
	if rr == nil || wsID == "" {
		return ""
	}
	return rr.WS.Dir(wsID)
}

// currentLineHead 读取 tombstone 记录的 line 当前 head。
func currentLineHead(rr *repo.Repo, line string) string {
	if line == "" {
		return ""
	}
	if h, err := rr.DB.LineHead(line); err == nil {
		return h.String()
	}
	return ""
}
