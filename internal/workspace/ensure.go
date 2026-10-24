package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
)

const (
	claimKindIdem = "idem"
	claimKindName = "name"

	claimPending = "pending"
	claimReady   = "ready"

	// claimLease 是 pending claim 的存活窗口；owner 崩溃后最多等这么久即可接管。
	claimLease = 15 * time.Minute
	// claimWait 是并发 loser 等待 winner 的上限；超时返回可重试错误。
	claimWait = 30 * time.Second
	// idemReadyTTL 是 ws_open/ws_ensure 幂等结果的保留窗口。
	idemReadyTTL = 30 * 24 * time.Hour
)

// CanonicalParent 把 parent 参数规范成稳定的 (kind,id)，用于 ws_ensure 的
// 同名同 parent 判定。line parent 用 line 名（而不是会移动的 head）。
func (m *Manager) CanonicalParent(parent string) (string, string, error) {
	parent = strings.TrimSpace(parent)
	if parent == "" || parent == "mainline" {
		def, err := m.db.DefaultLine()
		if err != nil {
			return "", "", err
		}
		return "line", def, nil
	}
	if strings.HasPrefix(parent, "line:") {
		name := strings.TrimPrefix(parent, "line:")
		if _, err := m.db.LineHead(name); err != nil {
			return "", "", fmt.Errorf("%w: line %s", ErrNotFound, name)
		}
		return "line", name, nil
	}
	if strings.HasPrefix(parent, "workspace:") {
		id := strings.TrimPrefix(parent, "workspace:")
		if _, err := m.db.GetWorkspace(id); err != nil {
			return "", "", fmt.Errorf("%w: parent %s", ErrNotFound, id)
		}
		return "workspace", id, nil
	}
	if strings.HasPrefix(parent, "version:") {
		parent = strings.TrimPrefix(parent, "version:")
	}
	if h, err := hash.Parse(parent); err == nil {
		if _, err := m.db.GetVersion(h); err == nil {
			return "version", h.String(), nil
		}
	} else if isHexPrefix(parent) {
		if h, err := m.db.FindVersionByPrefix(parent); err == nil {
			return "version", h.String(), nil
		}
	}
	if h, err := m.db.LineHead(parent); err == nil {
		if _, err := m.db.GetVersion(h); err == nil {
			return "line", parent, nil
		}
	}
	if _, err := m.db.GetWorkspace(parent); err == nil {
		return "workspace", parent, nil
	}
	return "", "", fmt.Errorf("%w: parent %s", ErrNotFound, parent)
}

func claimKeyIdem(key string) string { return "idem:" + key }

func claimKeyName(name, parentKind, parentID string) string {
	sum := sha256.Sum256([]byte(name + "\x00" + parentKind + "\x00" + parentID))
	return "name:" + hex.EncodeToString(sum[:])
}

func fingerprint(name, parentKind, parentID string) string {
	return name + "\x00" + parentKind + "\x00" + parentID
}

func (m *Manager) getClaim(tool, key string) (*meta.WSClaim, int64, error) {
	rec, err := m.db.GetKV(tool, key)
	if err != nil || rec == nil {
		return nil, 0, err
	}
	c, err := meta.ParseWSClaim(rec.Value)
	if err != nil {
		return nil, rec.ExpiresTS, nil
	}
	return c, rec.ExpiresTS, nil
}

func (m *Manager) putClaim(tool, key string, c meta.WSClaim, expires int64) error {
	c.UpdatedTS = m.now()
	b, err := meta.MarshalWSClaim(c)
	if err != nil {
		return err
	}
	return m.db.PutKV(tool, key, b, c.CreatedTS, expires)
}

func (m *Manager) deleteClaim(tool, key string) { _ = m.db.DeleteKV(tool, key) }

// acquireClaim 抢一个 claim。返回 owned=true 表示本次调用必须完成创建；
// 返回 ready 的 claim 表示已有结果。
func (m *Manager) acquireClaim(tool, key string, c meta.WSClaim) (*meta.WSClaim, bool, error) {
	deadline := time.Now().Add(claimWait)
	for {
		now := m.now()
		rec, err := m.db.GetKV(tool, key)
		if err != nil {
			return nil, false, err
		}
		if rec == nil {
			c.State = claimPending
			c.CreatedTS, c.UpdatedTS = now, now
			b, err := meta.MarshalWSClaim(c)
			if err != nil {
				return nil, false, err
			}
			ok, err := m.db.InsertKVIfAbsent(tool, key, b, now, now+claimLease.Milliseconds())
			if err != nil {
				return nil, false, err
			}
			if ok {
				return &c, true, nil
			}
			continue
		}
		cur, err := meta.ParseWSClaim(rec.Value)
		if err != nil {
			// 损坏记录：直接覆盖，避免永久卡死。
			if ok, _ := m.db.TakeOverKV(tool, key, mustClaim(c, now, now+claimLease.Milliseconds()), now, now+claimLease.Milliseconds()); ok {
				return &c, true, nil
			}
			continue
		}
		if cur.State == claimReady {
			return cur, false, nil
		}
		// pending：过期则接管。
		if rec.ExpiresTS > 0 && rec.ExpiresTS < now {
			c.State = claimPending
			c.CreatedTS, c.UpdatedTS = now, now
			ok, err := m.db.TakeOverKV(tool, key, mustClaim(c, now, now+claimLease.Milliseconds()), now, now+claimLease.Milliseconds())
			if err != nil {
				return nil, false, err
			}
			if ok {
				return &c, true, nil
			}
			continue
		}
		if time.Now().After(deadline) {
			retry := int64(250)
			if rec.ExpiresTS > now {
				retry = rec.ExpiresTS - now
			}
			return nil, false, &OpenInProgressError{WSID: cur.WSID, RetryAfterMS: retry}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func mustClaim(c meta.WSClaim, created, expires int64) string {
	c.State = claimPending
	c.CreatedTS, c.UpdatedTS = created, created
	b, _ := meta.MarshalWSClaim(c)
	return b
}

// OpenWithKey 打开工作区；同一 idempotency_key 重放返回同一个 ws_id。
func (m *Manager) OpenWithKey(req OpenRequest) (*OpenOutcome, error) {
	if req.IdempotencyKey == "" {
		w, err := m.Open(req)
		if err != nil {
			return nil, err
		}
		return &OpenOutcome{Workspace: w, WSID: w.ID, Created: true}, nil
	}
	parentKind, parentID, err := m.CanonicalParent(req.Parent)
	if err != nil {
		return nil, err
	}
	fp := fingerprint(req.Name, parentKind, parentID)
	tool, key := meta.KVToolWSClaim, claimKeyIdem(req.IdempotencyKey)
	c := meta.WSClaim{Kind: claimKindIdem, Name: req.Name, ParentKind: parentKind,
		ParentID: parentID, Fingerprint: fp}
	cur, owned, err := m.acquireClaim(tool, key, c)
	if err != nil {
		return nil, err
	}
	if !owned {
		if cur.Fingerprint != "" && cur.Fingerprint != fp {
			return nil, &IdemConflictError{Key: req.IdempotencyKey, WSID: cur.WSID,
				Field: "name/parent", Existing: cur.Fingerprint, Requested: fp}
		}
		w, _ := m.db.GetWorkspace(cur.WSID)
		return &OpenOutcome{Workspace: w, WSID: cur.WSID, Created: false, Reused: true, Replayed: true}, nil
	}
	wsID, err := newID("ws")
	if err != nil {
		m.deleteClaim(tool, key)
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			m.deleteClaim(tool, key)
		}
	}()
	req.ReservedID = wsID
	w, err := m.Open(req)
	if err != nil {
		return nil, err
	}
	cur.WSID, cur.State = w.ID, claimReady
	cur.CreatedTS = m.now()
	cur.UpdatedTS = cur.CreatedTS
	if err := m.putClaim(tool, key, *cur, m.now()+idemReadyTTL.Milliseconds()); err != nil {
		return nil, err
	}
	ok = true
	return &OpenOutcome{Workspace: w, WSID: w.ID, Created: true}, nil
}

// Ensure 按 (name, parent) 原子地 open-or-get。name 必填。
func (m *Manager) Ensure(req OpenRequest) (*OpenOutcome, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: ws_ensure 必须提供 name", ErrWrongState)
	}
	req.Name = name
	parentKind, parentID, err := m.CanonicalParent(req.Parent)
	if err != nil {
		return nil, err
	}
	fp := fingerprint(name, parentKind, parentID)

	// 显式 key 的幂等结果优先：同一个 key 永远返回第一次创建的工作区。
	if req.IdempotencyKey != "" {
		tool, key := meta.KVToolWSClaim, claimKeyIdem(req.IdempotencyKey)
		if cur, _, err := m.getClaim(tool, key); err != nil {
			return nil, err
		} else if cur != nil {
			if cur.State == claimReady {
				if cur.Fingerprint != "" && cur.Fingerprint != fp {
					return nil, &IdemConflictError{Key: req.IdempotencyKey, WSID: cur.WSID,
						Field: "name/parent", Existing: cur.Fingerprint, Requested: fp}
				}
				w, _ := m.db.GetWorkspace(cur.WSID)
				return &OpenOutcome{Workspace: w, WSID: cur.WSID, Created: false, Reused: true, Replayed: true}, nil
			}
			if _, owned, err := m.acquireClaim(tool, key, meta.WSClaim{
				Kind: claimKindIdem, Name: name, ParentKind: parentKind, ParentID: parentID,
				Fingerprint: fp}); err != nil {
				return nil, err
			} else if !owned {
				if cur, _, _ = m.getClaim(tool, key); cur != nil && cur.State == claimReady {
					w, _ := m.db.GetWorkspace(cur.WSID)
					return &OpenOutcome{Workspace: w, WSID: cur.WSID, Created: false, Reused: true, Replayed: true}, nil
				}
			}
		}
	}

	tool, key := meta.KVToolWSClaim, claimKeyName(name, parentKind, parentID)
	c := meta.WSClaim{Kind: claimKindName, Name: name, ParentKind: parentKind,
		ParentID: parentID, Fingerprint: fp}
	cur, owned, err := m.acquireClaim(tool, key, c)
	if err != nil {
		return nil, err
	}
	if !owned {
		if cur.Fingerprint != "" && cur.Fingerprint != fp {
			return nil, &IdemConflictError{Key: key, WSID: cur.WSID,
				Field: "name/parent", Existing: cur.Fingerprint, Requested: fp}
		}
		w, _ := m.db.GetWorkspace(cur.WSID)
		if w != nil && w.LandedTS == 0 {
			m.mapIdemClaim(req.IdempotencyKey, fp, w.ID, true)
			return &OpenOutcome{Workspace: w, WSID: w.ID, Created: false, Reused: true}, nil
		}
		// claim 指向的工作区已经消失（landed/drop）：释放后用同一 name 建新一代。
		m.deleteClaim(tool, key)
		return m.Ensure(req)
	}
	ok := false
	defer func() {
		if !ok {
			m.deleteClaim(tool, key)
		}
	}()

	// 兼容老仓库里已经存在、但没有 claim 的同名工作区。
	if w, err := m.db.FindLiveWorkspaceByNameParent(name, parentKind, parentID); err != nil {
		return nil, err
	} else if w != nil {
		cur.WSID, cur.State = w.ID, claimReady
		cur.CreatedTS = m.now()
		if err := m.putClaim(tool, key, *cur, 0); err != nil {
			return nil, err
		}
		m.mapIdemClaim(req.IdempotencyKey, fp, w.ID, false)
		ok = true
		return &OpenOutcome{Workspace: w, WSID: w.ID, Created: false, Reused: true}, nil
	}

	wsID, err := newID("ws")
	if err != nil {
		return nil, err
	}
	cur.WSID = wsID
	// 先把 ws_id 写回 pending claim，让并发 loser 至少能看到可重试的 id。
	_ = m.putClaim(tool, key, *cur, m.now()+claimLease.Milliseconds())
	req.ReservedID = wsID
	w, err := m.Open(req)
	if err != nil {
		return nil, err
	}
	cur.State = claimReady
	cur.CreatedTS = m.now()
	if err := m.putClaim(tool, key, *cur, 0); err != nil {
		return nil, err
	}
	m.mapIdemClaim(req.IdempotencyKey, fp, w.ID, false)
	ok = true
	return &OpenOutcome{Workspace: w, WSID: w.ID, Created: true}, nil
}

// mapIdemClaim 把一次 ensure 的 ws 映射到调用方的显式 key（如果提供）。
func (m *Manager) mapIdemClaim(key, fp, wsID string, replay bool) {
	if key == "" {
		return
	}
	tool := meta.KVToolWSClaim
	ck := claimKeyIdem(key)
	if cur, _, _ := m.getClaim(tool, ck); cur != nil && cur.State == claimReady {
		return // 已有最终结果，不覆盖（同 key 不同请求由调用方入口检查）
	}
	c := meta.WSClaim{Kind: claimKindIdem, State: claimReady, WSID: wsID,
		Fingerprint: fp, CreatedTS: m.now(), UpdatedTS: m.now()}
	_ = m.putClaim(tool, ck, c, m.now()+idemReadyTTL.Milliseconds())
}

// ReleaseNameClaims 释放指向某个工作区的 name claim（land/drop 后调用）。
func (m *Manager) ReleaseNameClaims(wsID string) { _ = m.releaseNameClaims(wsID) }

func (m *Manager) releaseNameClaims(wsID string) error {
	recs, err := m.db.ListKVAll(meta.KVToolWSClaim, 2000)
	if err != nil {
		return err
	}
	for _, rec := range recs {
		c, err := meta.ParseWSClaim(rec.Value)
		if err != nil || c.Kind != claimKindName || c.WSID != wsID {
			continue
		}
		_ = m.db.DeleteKV(meta.KVToolWSClaim, rec.Key)
	}
	return nil
}

// SweepClaims 清理过期 pending 和指向已消失工作区的 name claim。
func (m *Manager) SweepClaims() (int, error) {
	recs, err := m.db.ListKVAll(meta.KVToolWSClaim, 5000)
	if err != nil {
		return 0, err
	}
	now := m.now()
	n := 0
	for _, rec := range recs {
		c, err := meta.ParseWSClaim(rec.Value)
		if err != nil {
			_ = m.db.DeleteKV(meta.KVToolWSClaim, rec.Key)
			n++
			continue
		}
		if c.State == claimPending && rec.ExpiresTS > 0 && rec.ExpiresTS < now {
			_ = m.db.DeleteKV(meta.KVToolWSClaim, rec.Key)
			n++
			continue
		}
		if c.Kind == claimKindName && c.WSID != "" {
			w, _ := m.db.GetWorkspace(c.WSID)
			if w == nil || w.LandedTS != 0 {
				_ = m.db.DeleteKV(meta.KVToolWSClaim, rec.Key)
				n++
			}
		}
	}
	if _, err := m.db.DeleteExpiredKV(now); err != nil {
		return n, err
	}
	return n, nil
}

// IsOpenInProgress 辅助调用方判断错误类型。
func IsOpenInProgress(err error) bool { return errors.Is(err, ErrOpenInProgress) }
