package meta

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/version"
)

// CommitLand 在**一个事务**内完成 land 的元数据提交（§7.2 步骤 10）。
//
// 可恢复协议（这是 §7.3「最后提交 head」的具体化）：
//
//  1. 写对象（CAS，幂等）
//  2. CommitLand  ← 本函数：写 version 行 + journal + landed_ws，并把折叠集合
//     标记为 integrating（**不删除**它们，以便回滚）
//  3. 写 refs/mainline（原子）  ← 唯一的提交点
//  4. FinishLand ← 删除折叠集合的工作区
//
// 若在 2 与 3 之间崩溃：head 未推进 → ReconcileLand 会把 version 行与
// landed_ws 回滚掉，工作区恢复为 active。
// 若在 3 与 4 之间崩溃：head 已推进 → ReconcileLand 会补做 4。
func (d *DB) CommitLand(v *version.Version, j JournalEntry, foldWsIDs []string) error {
	vid := v.ID().String()
	return d.withWrite(func(tx *sql.Tx) error {
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

		if _, err := tx.Exec(
			`INSERT INTO version(id, parent, tree, epoch, agent, task_hash, task_desc,
			                     ts, msg, ws_id, journal_id)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			vid, v.Parent.String(), v.Tree.String(), int64(v.Epoch), v.Agent,
			v.TaskHash.String(), v.TaskDesc, v.TS, v.Msg,
			nullableString(j.WsID), jid); err != nil {
			return err
		}

		for _, wsID := range foldWsIDs {
			if _, err := tx.Exec(
				`INSERT INTO landed_ws(ws_id, version_id, ts) VALUES(?,?,?)
				 ON CONFLICT(ws_id) DO UPDATE SET version_id=excluded.version_id`,
				wsID, vid, v.TS); err != nil {
				return err
			}
			if _, err := tx.Exec(
				`UPDATE workspace SET state = ? WHERE id = ?`, StateIntegrating, wsID); err != nil {
				return err
			}
		}
		return nil
	})
}

// FinishLand 删除折叠集合的工作区（§7.2 步骤 11）。幂等。
func (d *DB) FinishLand(foldWsIDs []string) error {
	return d.withWrite(func(tx *sql.Tx) error {
		for _, wsID := range foldWsIDs {
			if _, err := tx.Exec(`DELETE FROM stat_cache WHERE ws_id = ?`, wsID); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM op WHERE ws_id = ?`, wsID); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM ws_effective_tree WHERE ws_id = ?`, wsID); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM conflict WHERE ws_id = ?`, wsID); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM lease WHERE ws_id = ?`, wsID); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM workspace WHERE id = ?`, wsID); err != nil {
				return err
			}
		}
		return nil
	})
}

// RollbackLand 撤销一次未完成的 land（head 未推进的情形）。
func (d *DB) RollbackLand(versionID string) error {
	return d.withWrite(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM version WHERE id = ?`, versionID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM journal WHERE version_id = ?`, versionID); err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT ws_id FROM landed_ws WHERE version_id = ?`, versionID)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.Exec(
				`UPDATE workspace SET state = ? WHERE id = ?`, StateActive, id); err != nil {
				return err
			}
		}
		_, err = tx.Exec(`DELETE FROM landed_ws WHERE version_id = ?`, versionID)
		return err
	})
}

// ReconcileLand 在启动时修复未完成的 land（§7.7）。返回处理的条目数。
//
// 1. 折叠集合中状态为 integrating 的工作区：
//   - 若 head 已指向其 landed 版本 → 补做 FinishLand
//   - 否则 → 回滚（恢复 active 并清掉 landed_ws/version/journal）
func (d *DB) ReconcileLand() (int, error) {
	mainHead, ok, err := d.Head()
	if err != nil {
		return 0, err
	}
	mainStr := ""
	if ok {
		mainStr = mainHead.String()
	}
	lineHeads := map[string]string{}
	if rows, err := d.sql.Query(`SELECT name, head FROM line WHERE deleted_ts=0`); err == nil {
		for rows.Next() {
			var n, h string
			if err := rows.Scan(&n, &h); err == nil {
				lineHeads[n] = h
			}
		}
		_ = rows.Close()
	}

	rows, err := d.sql.Query(
		`SELECT l.ws_id, l.version_id, l.target_kind, l.target_id FROM landed_ws l
		   JOIN workspace w ON w.id = l.ws_id
		  WHERE w.state = ?`, StateIntegrating)
	if err != nil {
		return 0, err
	}
	type pair struct{ wsID, verID, kind, target string }
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.wsID, &p.verID, &p.kind, &p.target); err != nil {
			_ = rows.Close()
			return 0, err
		}
		pairs = append(pairs, p)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	done := map[string][]string{}
	rollback := map[string][]string{}
	for _, p := range pairs {
		head := mainStr
		if p.kind == "line" {
			head = lineHeads[p.target]
		}
		if p.verID == head {
			done[p.verID] = append(done[p.verID], p.wsID)
		} else {
			rollback[p.verID] = append(rollback[p.verID], p.wsID)
		}
	}

	n := 0
	for verID, ids := range done {
		if err := d.FinishLand(ids); err != nil {
			return n, fmt.Errorf("meta: 补做 FinishLand(%s): %w", verID, err)
		}
		n += len(ids)
	}
	for verID, ids := range rollback {
		_ = ids
		if err := d.RollbackLand(verID); err != nil {
			return n, fmt.Errorf("meta: 回滚 land(%s): %w", verID, err)
		}
		n += len(ids)
	}
	return n, nil
}

// OrphanVersions 返回存在于 version 表但不被 head 链引用且非保留的版本（供 fsck）。
//
// 这里只做最小实现：返回 head 之外的全部版本 id。
func (d *DB) OrphanVersions() ([]hash.ID, error) {
	head, ok, err := d.Head()
	if err != nil {
		return nil, err
	}
	rows, err := d.sql.Query(`SELECT id FROM version`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []hash.ID
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		id, err := hash.Parse(s)
		if err != nil {
			return nil, err
		}
		if ok && id == head {
			continue
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ErrNoSuchWSLand 表示没有可回滚的 land 记录。
var ErrNoSuchWSLand = errors.New("meta: 无对应 land 记录")
