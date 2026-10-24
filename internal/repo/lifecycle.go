package repo

import (
	"errors"
	"time"

	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
)

// WSLifecycleState 是工作区的生命周期判定。
type WSLifecycleState string

const (
	WSLive       WSLifecycleState = "live"
	WSLanded     WSLifecycleState = "landed"
	WSGoneLanded WSLifecycleState = "gone_landed"
	WSGone       WSLifecycleState = "gone"
)

// WSLifecycle 是 ws_info / ws_sync / land 对“这个 ws_id 现在是什么”的统一答案。
type WSLifecycle struct {
	State           WSLifecycleState
	Workspace       *meta.Workspace
	Tombstone       *meta.WSTombstone
	Landed          *meta.LandedWSInfo
	CurrentLineHead string
}

// WSLifecycleOf 先看活行，再看 tombstone，最后用 landed_ws 兜底老仓库。
func (r *Repo) WSLifecycleOf(wsID string) (*WSLifecycle, error) {
	w, err := r.DB.GetWorkspace(wsID)
	if err != nil && !errors.Is(err, meta.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if w.LandedTS != 0 {
			info, ok, _ := r.DB.GetLandedWSInfo(wsID)
			life := &WSLifecycle{State: WSLanded, Workspace: w}
			if ok {
				life.Landed = info
				if info.TargetKind == "line" {
					if h, err := r.DB.LineHead(info.TargetID); err == nil {
						life.CurrentLineHead = h.String()
					}
				}
			}
			if w.BaseLine != "" {
				if h, err := r.DB.LineHead(w.BaseLine); err == nil {
					life.CurrentLineHead = h.String()
				}
			}
			return life, nil
		}
		return &WSLifecycle{State: WSLive, Workspace: w}, nil
	}
	now := time.Now().UnixMilli()
	if t, err := r.DB.GetWSTombstone(wsID, now); err != nil {
		return nil, err
	} else if t != nil {
		life := &WSLifecycle{State: WSGone, Tombstone: t}
		if t.Reason == "landed_and_dropped" {
			life.State = WSGoneLanded
		}
		if t.Line != "" {
			if h, err := r.DB.LineHead(t.Line); err == nil {
				life.CurrentLineHead = h.String()
			}
		}
		if info, ok, _ := r.DB.GetLandedWSInfo(wsID); ok {
			life.Landed = info
		}
		return life, nil
	}
	if info, ok, err := r.DB.GetLandedWSInfo(wsID); err != nil {
		return nil, err
	} else if ok {
		t := &meta.WSTombstone{
			WSID: wsID, Reason: "landed_and_dropped", Why: "landed_ws",
			LandedVersion: info.VersionID, TargetKind: info.TargetKind,
			TargetID: info.TargetID, LineHead: info.VersionID, DroppedAt: info.TS,
		}
		if info.TargetKind == "line" {
			t.Line = info.TargetID
			if h, err := r.DB.LineHead(t.Line); err == nil {
				t.LineHead = h.String()
			}
		}
		life := &WSLifecycle{State: WSGoneLanded, Tombstone: t, Landed: info}
		if h, err := r.DB.LineHead(t.Line); err == nil {
			life.CurrentLineHead = h.String()
		}
		return life, nil
	}
	return &WSLifecycle{State: WSGone}, nil
}

// LandedView 由 Lifecycle 构造已经落地/已回收的统一返回。
func (l *WSLifecycle) LandedView() *LandResult {
	t := l.Tombstone
	if t == nil {
		t = &meta.WSTombstone{}
	}
	vid := t.LandedVersion
	if vid == "" && l.Landed != nil {
		vid = l.Landed.VersionID
	}
	if t.Line == "" && l.Landed != nil && l.Landed.TargetKind == "line" {
		t.Line = l.Landed.TargetID
		t.TargetKind, t.TargetID = l.Landed.TargetKind, l.Landed.TargetID
		t.LineHead = l.Landed.VersionID
	}
	res := &LandResult{
		CanLand: true, AlreadyLanded: true, State: "already_landed",
		Line: t.Line, Reason: t.Reason, DroppedAt: t.DroppedAt,
		TargetHead: parseHash(t.LineHead), WSTree: parseHash(t.LineHead),
		CurrentLineHead: parseHash(l.CurrentLineHead), PreviewID: t.WSID,
	}
	if vid != "" {
		res.VersionID = parseHash(vid)
		res.LandedVersion = parseHash(vid)
	}
	return res
}

func parseHash(s string) hash.ID {
	h, _ := hash.Parse(s)
	return h
}
