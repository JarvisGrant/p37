package meta

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/op"
)

// --- 冲突与解决（§6.4） -------------------------------------------------

// ConflictRow 是 conflict 表的一行。
type ConflictRow struct {
	Key       hash.ID
	WsID      string
	OpID      string
	HunkIdx   int
	Path      string
	Kind      op.ConflictKind
	State     string
	BaseJSON  string
	LowerJSON string
	UpperJSON string
	CandsJSON string
	CreatedTS int64
}

// UpsertConflicts 写入未决冲突（按 key 幂等）。
func (d *DB) UpsertConflicts(rows []ConflictRow) error {
	if len(rows) == 0 {
		return nil
	}
	return d.withWrite(func(tx *sql.Tx) error {
		for i := range rows {
			r := &rows[i]
			if r.CreatedTS == 0 {
				r.CreatedTS = time.Now().UnixMilli()
			}
			if _, err := tx.Exec(
				`INSERT INTO conflict(key, ws_id, op_id, hunk_index, path, kind, state,
				                      base_json, lower_json, upper_json, candidates_json,
				                      created_ts)
				 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
				 ON CONFLICT(key) DO UPDATE SET state=excluded.state`,
				r.Key.String(), r.WsID, r.OpID, r.HunkIdx, r.Path, string(r.Kind), r.State,
				r.BaseJSON, r.LowerJSON, r.UpperJSON, r.CandsJSON, r.CreatedTS); err != nil {
				return err
			}
		}
		return nil
	})
}

// ListConflicts 返回某工作区的冲突（按 path, key）。
func (d *DB) ListConflicts(wsID string, onlyOpen bool) ([]ConflictRow, error) {
	q := `SELECT key, ws_id, op_id, hunk_index, path, kind, state,
	             base_json, lower_json, upper_json, candidates_json, created_ts
	        FROM conflict WHERE ws_id = ?`
	if onlyOpen {
		q += ` AND state = 'open'`
	}
	q += ` ORDER BY path, key`

	rows, err := d.sql.Query(q, wsID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []ConflictRow
	for rows.Next() {
		var (
			r    ConflictRow
			keyS string
			kind string
		)
		if err := rows.Scan(&keyS, &r.WsID, &r.OpID, &r.HunkIdx, &r.Path, &kind,
			&r.State, &r.BaseJSON, &r.LowerJSON, &r.UpperJSON, &r.CandsJSON,
			&r.CreatedTS); err != nil {
			return nil, err
		}
		if r.Key, err = hash.Parse(keyS); err != nil {
			return nil, err
		}
		r.Kind = op.ConflictKind(kind)
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountOpenConflicts 返回未决冲突数。
func (d *DB) CountOpenConflicts(wsID string) (int64, error) {
	var n int64
	err := d.sql.QueryRow(
		`SELECT COUNT(*) FROM conflict WHERE ws_id = ? AND state = 'open'`, wsID).Scan(&n)
	return n, err
}

// ClearConflicts 清空某工作区的冲突（落地成功或重新预览前）。
func (d *DB) ClearConflicts(wsID string) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM conflict WHERE ws_id = ?`, wsID)
		return err
	})
}

// ResolutionStore 是 op.Resolver 在元数据库上的实现。
type ResolutionStore struct{ db *DB }

// Resolver 返回适配器。
func (d *DB) Resolver() *ResolutionStore { return &ResolutionStore{db: d} }

// Lookup 实现 op.Resolver。
func (r *ResolutionStore) Lookup(key hash.ID) (op.Resolution, bool, error) {
	var (
		wsID, path, lowerS, upperS, kind, payload string
	)
	err := r.db.sql.QueryRow(
		`SELECT ws_id, path, lower_hash, upper_hash, kind, payload_json
		   FROM resolution WHERE key = ?`, key.String()).
		Scan(&wsID, &path, &lowerS, &upperS, &kind, &payload)
	if err == sql.ErrNoRows {
		return op.Resolution{}, false, nil
	}
	if err != nil {
		return op.Resolution{}, false, nil
	}

	res := op.Resolution{
		Key:  key,
		WsID: wsID,
		Path: path,
		Kind: op.ResolutionKind(kind),
	}
	if res.LowerHash, err = hash.Parse(lowerS); err != nil {
		return op.Resolution{}, false, err
	}
	if res.UpperHash, err = hash.Parse(upperS); err != nil {
		return op.Resolution{}, false, err
	}
	if kind == string(op.ResolveCustom) && payload != "" {
		var wire struct {
			Content []string `json:"content"`
		}
		if err := json.Unmarshal([]byte(payload), &wire); err == nil {
			content, err := decodeBase64Lines(wire.Content)
			if err != nil {
				return op.Resolution{}, false, err
			}
			res.Content = content
		}
	}
	return res, true, nil
}

// Put 写入一条 resolution。已存在且 kind 相同时返回 false（重复写入视为成功）。
func (r *ResolutionStore) Put(res op.Resolution, createdBy string) (bool, error) {
	if !res.Kind.Valid() {
		return false, fmt.Errorf("meta: 非法解决方式 %q", res.Kind)
	}
	payload := "{}"
	if res.Kind == op.ResolveCustom {
		b, err := json.Marshal(map[string]any{"content": encodeBase64Lines(res.Content)})
		if err != nil {
			return false, err
		}
		payload = string(b)
	}

	created := false
	err := r.db.withWrite(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM resolution WHERE key = ?`,
			res.Key.String()).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil // 已存在：不可更新（§6.4）
		}
		created = true
		_, err := tx.Exec(
			`INSERT INTO resolution(key, ws_id, path, lower_hash, upper_hash, kind,
			                        payload_json, created_ts, created_by)
			 VALUES(?,?,?,?,?,?,?,?,?)`,
			res.Key.String(), res.WsID, res.Path, res.LowerHash.String(),
			res.UpperHash.String(), string(res.Kind), payload,
			time.Now().UnixMilli(), createdBy)
		return err
	})
	return created, err
}

// CountResolutions 返回记录数。
func (d *DB) CountResolutions() (int64, error) {
	var n int64
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM resolution`).Scan(&n)
	return n, err
}

// --- journal（§12.1） --------------------------------------------------

// JournalEntry 是一条血缘记录。
type JournalEntry struct {
	VersionID string
	WsID      string
	Agent     string
	TaskDesc  string
	TS        int64
	OpsJSON   string
}

// InsertJournal 写入一条血缘并返回其自增 id。
func (d *DB) InsertJournal(e JournalEntry) (int64, error) {
	var id int64
	err := d.withWrite(func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`INSERT INTO journal(version_id, ws_id, agent, task_desc, ts, ops_json)
			 VALUES(?,?,?,?,?,?)`,
			e.VersionID, e.WsID, e.Agent, e.TaskDesc, e.TS, e.OpsJSON)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// GetJournal 按版本 id 读取血缘。
func (d *DB) GetJournal(versionID string) (*JournalEntry, error) {
	var e JournalEntry
	err := d.sql.QueryRow(
		`SELECT version_id, ws_id, agent, task_desc, ts, ops_json
		   FROM journal WHERE version_id = ? ORDER BY id DESC LIMIT 1`, versionID).
		Scan(&e.VersionID, &e.WsID, &e.Agent, &e.TaskDesc, &e.TS, &e.OpsJSON)
	if err == sql.ErrNoRows {
		return nil, ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// encodeBase64Lines / decodeBase64Lines 复用 op 包的行编码约定。
func encodeBase64Lines(ls [][]byte) []string {
	if len(ls) == 0 {
		return nil
	}
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = base64Std.EncodeToString(l)
	}
	return out
}

func decodeBase64Lines(ss []string) ([][]byte, error) {
	if len(ss) == 0 {
		return nil, nil
	}
	out := make([][]byte, len(ss))
	for i, s := range ss {
		b, err := base64Std.DecodeString(s)
		if err != nil {
			return nil, err
		}
		out[i] = b
	}
	return out, nil
}

var base64Std = base64.StdEncoding

// ListConflictByKey 按 key 读取单条冲突。
func (d *DB) ListConflictByKey(key hash.ID) (*ConflictRow, error) {
	var (
		r    ConflictRow
		keyS string
		kind string
	)
	err := d.sql.QueryRow(
		`SELECT key, ws_id, op_id, hunk_index, path, kind, state,
		        base_json, lower_json, upper_json, candidates_json, created_ts
		   FROM conflict WHERE key = ?`, key.String()).
		Scan(&keyS, &r.WsID, &r.OpID, &r.HunkIdx, &r.Path, &kind, &r.State,
			&r.BaseJSON, &r.LowerJSON, &r.UpperJSON, &r.CandsJSON, &r.CreatedTS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	if r.Key, err = hash.Parse(keyS); err != nil {
		return nil, err
	}
	r.Kind = op.ConflictKind(kind)
	return &r, nil
}

// MarkConflictResolved 把冲突标记为已解决。
func (d *DB) MarkConflictResolved(key hash.ID) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE conflict SET state = 'resolved' WHERE key = ?`, key.String())
		return err
	})
}
