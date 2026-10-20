package meta

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/version"
)

// CommitLineLand 在**一个事务**内完成「land 到命名 line」的元数据提交。
//
// line head 的更新是唯一提交点；version 行、version_parent、journal、
// landed_ws 与 idempotency 必须在同一事务里可见。
func (d *DB) CommitLineLand(v *version.Version, j JournalEntry, line string,
	foldWsIDs []string, idemTool, idemKey string, expectedHead hash.ID) error {
	vid := v.ID().String()
	return d.withWrite(func(tx *sql.Tx) error {
		if expectedHead.IsZero() {
			var cur string
			err := tx.QueryRow(`SELECT head FROM line WHERE name=? AND deleted_ts=0`, line).Scan(&cur)
			if err != nil {
				if err == sql.ErrNoRows {
					return ErrNotExist
				}
				return err
			}
			if cur != v.Parent.String() {
				return fmt.Errorf("%w: line %s head 已变化", ErrLineHeadMoved, line)
			}
		} else {
			var cur string
			if err := tx.QueryRow(
				`SELECT head FROM line WHERE name=? AND deleted_ts=0`, line).Scan(&cur); err != nil {
				if err == sql.ErrNoRows {
					return ErrNotExist
				}
				return err
			}
			if cur != expectedHead.String() {
				return fmt.Errorf("%w: line %s head 已变化", ErrLineHeadMoved, line)
			}
		}
		if err := insertVersionTx(tx, v, j, idemTool, idemKey); err != nil {
			return err
		}
		for _, wsID := range foldWsIDs {
			if _, err := tx.Exec(
				`INSERT INTO landed_ws(ws_id, version_id, target_kind, target_id, ts)
				 VALUES(?,?,?,?,?)
				 ON CONFLICT(ws_id) DO UPDATE SET version_id=excluded.version_id,
				   target_kind=excluded.target_kind, target_id=excluded.target_id, ts=excluded.ts`,
				wsID, vid, "line", line, v.TS); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE workspace SET state=?, landed_ts=?, updated_ts=? WHERE id=?`,
				StateIntegrating, v.TS, v.TS, wsID); err != nil {
				return err
			}
		}
		return setLineHeadTx(tx, line, v.ID(), v.TS)
	})
}

// CommitMerge 提交一次 line_merge（可能 fast-forward 由调用方处理）。
func (d *DB) CommitMerge(v *version.Version, j JournalEntry, line string,
	expectedHead hash.ID, idemTool, idemKey string) error {
	vid := v.ID().String()
	return d.withWrite(func(tx *sql.Tx) error {
		var cur string
		if err := tx.QueryRow(
			`SELECT head FROM line WHERE name=? AND deleted_ts=0`, line).Scan(&cur); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotExist
			}
			return err
		}
		if !expectedHead.IsZero() && cur != expectedHead.String() {
			return fmt.Errorf("%w: line %s head 已变化", ErrLineHeadMoved, line)
		}
		if cur != v.Parent.String() {
			return fmt.Errorf("%w: line %s head 已变化", ErrLineHeadMoved, line)
		}
		if err := insertVersionTx(tx, v, j, idemTool, idemKey); err != nil {
			return err
		}
		_ = vid
		return setLineHeadTx(tx, line, v.ID(), v.TS)
	})
}

// ErrLineHeadMoved 表示 line head 与调用方预期不一致。
var ErrLineHeadMoved = fmt.Errorf("meta: line head 已变化")

func insertVersionTx(tx *sql.Tx, v *version.Version, j JournalEntry,
	idemTool, idemKey string) error {
	vid := v.ID().String()
	if j.VersionID == "" {
		j.VersionID = vid
	}
	res, err := tx.Exec(
		`INSERT INTO journal(version_id, ws_id, agent, task_desc, ts, ops_json)
		 VALUES(?,?,?,?,?,?)`,
		j.VersionID, j.WsID, j.Agent, j.TaskDesc, j.TS, j.OpsJSON)
	if err != nil {
		return err
	}
	jid, err := res.LastInsertId()
	if err != nil {
		return err
	}
	var idemKeyArg, idemToolArg any
	var idemTS any
	if idemKey != "" {
		idemKeyArg, idemToolArg, idemTS = idemKey, idemTool, v.TS
	}
	_, err = tx.Exec(
		`INSERT INTO version(id, parent, tree, epoch, agent, task_hash, task_desc,
		                     ts, msg, ws_id, journal_id,
		                     idempotency_key, idempotency_tool, idempotency_ts)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		vid, v.Parent.String(), v.Tree.String(), int64(v.Epoch), v.Agent,
		v.TaskHash.String(), v.TaskDesc, v.TS, v.Msg,
		nullableString(j.WsID), jid, idemKeyArg, idemToolArg, idemTS)
	if err != nil {
		return err
	}
	return insertVersionParentsTx(tx, v.ID(), v)
}

// RecordLineReset 写一条 line_reset 审计记录。
func (d *DB) RecordLineReset(line string, from, to hash.ID, reason string, forced bool, ts int64) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO line_reset_log(line, from_head, to_head, reason, forced, ts)
			 VALUES(?,?,?,?,?,?)`,
			line, from.String(), to.String(), reason, boolToInt(forced), ts)
		return err
	})
}

// LineResetLog 是审计记录的一行。
type LineResetLog struct {
	ID     int64  `json:"id"`
	Line   string `json:"line"`
	From   string `json:"from_head"`
	To     string `json:"to_head"`
	Reason string `json:"reason"`
	Forced bool   `json:"forced"`
	TS     int64  `json:"ts"`
}

// ListLineResetLog 返回某条 line 的 reset 审计。
func (d *DB) ListLineResetLog(line string, limit int) ([]LineResetLog, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT id, line, from_head, to_head, reason, forced, ts FROM line_reset_log`
	args := []any{}
	if line != "" {
		q += ` WHERE line=?`
		args = append(args, line)
	}
	q += ` ORDER BY ts DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := d.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []LineResetLog
	for rows.Next() {
		var r LineResetLog
		var forced int
		if err := rows.Scan(&r.ID, &r.Line, &r.From, &r.To, &r.Reason, &forced, &r.TS); err != nil {
			return nil, err
		}
		r.Forced = forced != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// MigrateIfNeeded 把 v1 仓库升级到当前格式。升级前做一次一致性备份。
//
// 说明：虽然 cli 有显式 p37 migrate，这里也允许 open 时自动升级，避免 P36
// 旧实例因为升级顺序而无法启动；升级是幂等的。
func (d *DB) MigrateIfNeeded() (bool, error) {
	v, err := d.UserVersion()
	if err != nil {
		return false, err
	}
	if v == 0 {
		return false, nil
	}
	if v >= FormatVer {
		return false, nil
	}
	// 备份：VACUUM INTO 在 WAL 下也能得到一致快照。
	backupDir := filepath.Join(filepath.Dir(d.path), "backup")
	if err := ensureDir(backupDir); err != nil {
		return false, err
	}
	backup := fmt.Sprintf("%s/p37-v%d-%d.db", backupDir, v, time.Now().UnixMilli())
	if _, err := d.sql.Exec(`VACUUM INTO ?`, backup); err != nil {
		// VACUUM INTO 不支持参数化时退回文件复制；失败不阻塞升级。
		_ = copyFile(d.path, backup)
	}
	if err := d.EnsureSchema(); err != nil {
		return false, err
	}
	if _, err := d.sql.Exec(`UPDATE repo SET format_ver=? WHERE id=1`, FormatVer); err != nil {
		return false, err
	}
	if _, err := d.sql.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, FormatVer)); err != nil {
		return false, err
	}
	return true, nil
}

func ensureDir(p string) error { return os.MkdirAll(p, 0o700) }

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}
