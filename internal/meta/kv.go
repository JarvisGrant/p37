package meta

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// 内部 KV 命名空间。
//
// 这些记录复用 idempotency_result 表：不新增 DDL，format_ver 保持 2，老二进制
// 升级/回滚都不会因为未知表/列而失败。tool 名统一带 p37 内部前缀，避免与
// 调用方的业务幂等键冲突（业务键只出现在 key 列）。
const (
	KVToolWSClaim     = "__p37/ws_claim"
	KVToolWSTombstone = "__p37/ws_tombstone"
	KVToolWSLand      = "ws_land"
	KVToolWSLandInto  = "ws_land_into"
)

// DefaultTombstoneTTL 是工作区回收记录的默认保留窗口。
const DefaultTombstoneTTL = 7 * 24 * time.Hour

// KVRecord 是一条内部键值记录（包含已过期记录）。
type KVRecord struct {
	Tool      string
	Key       string
	Value     string
	CreatedTS int64
	ExpiresTS int64
}

// GetKV 读取一条记录，不做过期过滤。过期判断由调用方决定（claim 需要看到
// 过期的 pending 记录才能接管）。
func (d *DB) GetKV(tool, key string) (*KVRecord, error) {
	var r KVRecord
	err := d.sql.QueryRow(
		`SELECT tool, key, result_json, created_ts, expires_ts
		   FROM idempotency_result WHERE tool=? AND key=?`, tool, key).
		Scan(&r.Tool, &r.Key, &r.Value, &r.CreatedTS, &r.ExpiresTS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// InsertKVIfAbsent 仅在不存在时插入，返回是否由本次调用插入。
//
// 这是 ws_open/ws_ensure 的 claim 原语：并发调用只有一个 winner。
func (d *DB) InsertKVIfAbsent(tool, key, value string, ts, expiresTS int64) (bool, error) {
	var inserted bool
	err := d.withWrite(func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`INSERT INTO idempotency_result(tool, key, result_json, created_ts, expires_ts)
			 VALUES(?,?,?,?,?) ON CONFLICT(tool,key) DO NOTHING`,
			tool, key, value, ts, expiresTS)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		inserted = n > 0
		return nil
	})
	return inserted, err
}

// PutKV 写入/覆盖一条记录。
func (d *DB) PutKV(tool, key, value string, ts, expiresTS int64) error {
	return d.PutIdempotencyResult(tool, key, value, ts, expiresTS)
}

// TakeOverKV 仅在记录已过期时覆盖，返回是否接管成功。
func (d *DB) TakeOverKV(tool, key, value string, now, expiresTS int64) (bool, error) {
	var taken bool
	err := d.withWrite(func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE idempotency_result
			    SET result_json=?, created_ts=?, expires_ts=?
			  WHERE tool=? AND key=? AND (expires_ts=0 OR expires_ts < ?)`,
			value, now, expiresTS, tool, key, now)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		taken = n > 0
		return nil
	})
	return taken, err
}

// DeleteKV 删除一条记录。
func (d *DB) DeleteKV(tool, key string) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM idempotency_result WHERE tool=? AND key=?`, tool, key)
		return err
	})
}

// ListKV 返回某个 tool 下未过期（expires_ts=0 或 > since）的记录。
func (d *DB) ListKV(tool string, since int64, limit int) ([]KVRecord, error) {
	if limit <= 0 {
		limit = 500
	}
	q := `SELECT tool, key, result_json, created_ts, expires_ts
	        FROM idempotency_result
	       WHERE tool=? AND (expires_ts=0 OR expires_ts > ?)
	       ORDER BY created_ts DESC LIMIT ?`
	rows, err := d.sql.Query(q, tool, since, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []KVRecord
	for rows.Next() {
		var r KVRecord
		if err := rows.Scan(&r.Tool, &r.Key, &r.Value, &r.CreatedTS, &r.ExpiresTS); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListKVAll 返回某个 tool 下全部记录（包括已过期），用于 claim 清理。
func (d *DB) ListKVAll(tool string, limit int) ([]KVRecord, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := d.sql.Query(
		`SELECT tool, key, result_json, created_ts, expires_ts
		   FROM idempotency_result WHERE tool=? ORDER BY created_ts DESC LIMIT ?`, tool, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []KVRecord
	for rows.Next() {
		var r KVRecord
		if err := rows.Scan(&r.Tool, &r.Key, &r.Value, &r.CreatedTS, &r.ExpiresTS); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteExpiredKV 清理已过期的内部记录。
func (d *DB) DeleteExpiredKV(now int64) (int64, error) {
	var n int64
	err := d.withWrite(func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM idempotency_result WHERE expires_ts > 0 AND expires_ts < ?`, now)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// WSClaim 是工作区创建 claim 的持久化表示。
type WSClaim struct {
	Kind        string          `json:"kind"`  // idem | name
	State       string          `json:"state"` // pending | ready
	WSID        string          `json:"ws_id"`
	Name        string          `json:"name"`
	ParentKind  string          `json:"parent_kind"`
	ParentID    string          `json:"parent_id"`
	Fingerprint string          `json:"fingerprint"`
	Result      json.RawMessage `json:"result,omitempty"`
	CreatedTS   int64           `json:"created_ts"`
	UpdatedTS   int64           `json:"updated_ts"`
}

// MarshalWSClaim 编码 claim。
func MarshalWSClaim(c WSClaim) (string, error) {
	b, err := json.Marshal(c)
	return string(b), err
}

// ParseWSClaim 解码 claim；非法记录返回错误。
func ParseWSClaim(s string) (*WSClaim, error) {
	var c WSClaim
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// WSTombstone 是工作区回收记录。
type WSTombstone struct {
	WSID          string `json:"ws_id"`
	Name          string `json:"name"`
	ParentKind    string `json:"parent_kind"`
	ParentID      string `json:"parent_id"`
	Reason        string `json:"reason"`
	Why           string `json:"why,omitempty"`
	StateBefore   string `json:"state_before,omitempty"`
	DroppedAt     int64  `json:"dropped_at"`
	CreatedTS     int64  `json:"created_ts,omitempty"`
	LandedVersion string `json:"landed_version,omitempty"`
	TargetKind    string `json:"target_kind,omitempty"`
	TargetID      string `json:"target_id,omitempty"`
	Line          string `json:"line,omitempty"`
	LineHead      string `json:"line_head,omitempty"`
	DirtyOps      int64  `json:"dirty_ops,omitempty"`
	ExpiresAt     int64  `json:"tombstone_expires_at,omitempty"`
}

// PutWSTombstone 写入一条回收记录（TTL 由 expires 决定，0 表示不过期）。
func (d *DB) PutWSTombstone(t WSTombstone, expires int64) error {
	if t.DroppedAt == 0 {
		t.DroppedAt = time.Now().UnixMilli()
	}
	t.ExpiresAt = expires
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return d.PutKV(KVToolWSTombstone, t.WSID, string(b), t.DroppedAt, expires)
}

// GetWSTombstone 读取未过期的回收记录。
func (d *DB) GetWSTombstone(wsID string, now int64) (*WSTombstone, error) {
	rec, err := d.GetKV(KVToolWSTombstone, wsID)
	if err != nil || rec == nil {
		return nil, err
	}
	if rec.ExpiresTS > 0 && rec.ExpiresTS < now {
		return nil, nil
	}
	var t WSTombstone
	if err := json.Unmarshal([]byte(rec.Value), &t); err != nil {
		return nil, err
	}
	if t.ExpiresAt == 0 {
		t.ExpiresAt = rec.ExpiresTS
	}
	return &t, nil
}

// CountWSTombstones 返回当前未过期的回收记录数。
func (d *DB) CountWSTombstones(now int64) (int64, error) {
	var n int64
	err := d.sql.QueryRow(
		`SELECT COUNT(*) FROM idempotency_result
		  WHERE tool=? AND (expires_ts=0 OR expires_ts > ?)`,
		KVToolWSTombstone, now).Scan(&n)
	return n, err
}

// ListWSTombstones 返回 since 之后、未过期的回收记录。
func (d *DB) ListWSTombstones(since, now int64, limit int) ([]WSTombstone, error) {
	recs, err := d.ListKV(KVToolWSTombstone, since, limit)
	if err != nil {
		return nil, err
	}
	out := make([]WSTombstone, 0, len(recs))
	for _, rec := range recs {
		if rec.ExpiresTS > 0 && rec.ExpiresTS < now {
			continue
		}
		var t WSTombstone
		if err := json.Unmarshal([]byte(rec.Value), &t); err != nil {
			continue
		}
		if t.ExpiresAt == 0 {
			t.ExpiresAt = rec.ExpiresTS
		}
		out = append(out, t)
	}
	return out, nil
}

// insertTombstoneTx 在删除 workspace 的同一事务里写 tombstone。
func (d *DB) insertTombstoneTx(tx *sql.Tx, t WSTombstone) error {
	if t.DroppedAt == 0 {
		t.DroppedAt = time.Now().UnixMilli()
	}
	expires := t.ExpiresAt
	if expires == 0 {
		expires = t.DroppedAt + d.tombstoneTTL.Milliseconds()
	}
	t.ExpiresAt = expires
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	_, err = tx.Exec(
		`INSERT INTO idempotency_result(tool, key, result_json, created_ts, expires_ts)
		 VALUES(?,?,?,?,?)
		 ON CONFLICT(tool,key) DO UPDATE SET result_json=excluded.result_json,
		   created_ts=excluded.created_ts, expires_ts=excluded.expires_ts`,
		KVToolWSTombstone, t.WSID, string(b), t.DroppedAt, expires)
	return err
}

// deleteTombstoneTx 删除同 ws 的 tombstone（正常重建时避免旧记录遮蔽新 ws）。
func (d *DB) deleteTombstoneTx(tx *sql.Tx, wsID string) error {
	_, err := tx.Exec(`DELETE FROM idempotency_result WHERE tool=? AND key=?`,
		KVToolWSTombstone, wsID)
	return err
}
