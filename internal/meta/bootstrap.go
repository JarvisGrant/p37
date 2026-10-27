package meta

import (
	"database/sql"
	"errors"
	"time"

	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/version"
)

// RootState 是仓库根状态的一致性快照，供 repo_init / repo_health 使用。
//
// 它只读 SQLite，不打开对象存储、不跑迁移。
type RootState struct {
	Repo         Repo
	Lines        []Line
	VersionCount int64
	// Roots 是 parent 为零/空的版本；正常仓库应当恰好有一个。
	Roots []hash.ID
	// Leaves 是没有任何子版本的版本；正常情况下 head 应当是其中之一。
	Leaves []hash.ID
}

// RootState 读取 repo/line/version 的一致性快照。
func (d *DB) RootState() (*RootState, error) {
	repo, err := d.GetRepo()
	if err != nil {
		return nil, err
	}
	lines, err := d.ListLines()
	if err != nil {
		return nil, err
	}
	var n int64
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM version`).Scan(&n); err != nil {
		return nil, err
	}
	rows, err := d.sql.Query(`SELECT id, parent FROM version`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	zero := hash.Zero.String()
	ids := map[string]hash.ID{}
	referenced := map[string]bool{}
	for rows.Next() {
		var idStr string
		var parent sql.NullString
		if err := rows.Scan(&idStr, &parent); err != nil {
			return nil, err
		}
		id, err := hash.Parse(idStr)
		if err != nil {
			return nil, err
		}
		ids[idStr] = id
		if parent.Valid && parent.String != "" && parent.String != zero {
			referenced[parent.String] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 多父版本的 parent 在 version_parent 里；parent 列只保存首父。
	// 对叶子判定必须把 version_parent 的 parent_id 也算进去。
	prs, err := d.sql.Query(`SELECT DISTINCT parent_id FROM version_parent`)
	if err != nil {
		return nil, err
	}
	for prs.Next() {
		var p string
		if err := prs.Scan(&p); err != nil {
			_ = prs.Close()
			return nil, err
		}
		referenced[p] = true
	}
	if err := prs.Close(); err != nil {
		return nil, err
	}
	st := &RootState{Repo: repo, Lines: lines, VersionCount: n}
	for idStr, id := range ids {
		if !referenced[idStr] {
			st.Leaves = append(st.Leaves, id)
		}
	}
	rootRows, err := d.sql.Query(
		`SELECT id FROM version WHERE parent IS NULL OR parent='' OR parent=?`, zero)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rootRows.Close() }()
	for rootRows.Next() {
		var idStr string
		if err := rootRows.Scan(&idStr); err != nil {
			return nil, err
		}
		id, err := hash.Parse(idStr)
		if err != nil {
			return nil, err
		}
		st.Roots = append(st.Roots, id)
	}
	return st, rootRows.Err()
}

// CountReachable 返回从 head 出发沿 parent/version_parent 可达的版本数。
func (d *DB) CountReachable(head hash.ID) (int64, error) {
	if head.IsZero() {
		return 0, nil
	}
	seen := map[string]bool{head.String(): true}
	queue := []string{head.String()}
	var n int64
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		n++
		parents, err := d.parentsOf(cur)
		if err != nil {
			return 0, err
		}
		for _, p := range parents {
			if p == "" || p == hash.Zero.String() || seen[p] {
				continue
			}
			seen[p] = true
			queue = append(queue, p)
		}
	}
	return n, nil
}

func (d *DB) parentsOf(id string) ([]string, error) {
	rows, err := d.sql.Query(
		`SELECT parent_id FROM version_parent WHERE version_id=? ORDER BY ordinal`, id)
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(out) > 0 {
		return out, nil
	}
	var p sql.NullString
	err = d.sql.QueryRow(`SELECT parent FROM version WHERE id=?`, id).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	if p.Valid && p.String != "" {
		out = append(out, p.String)
	}
	return out, nil
}

// Bootstrap 在一个事务里写 repo 行、v0、默认 line 与 refs 所需的 head。
//
// 供原子初始化使用：调用方已经建好表（DB.Init），这里只做数据写入。
// journal 非空时同事务写一条血缘（import 需要）。
func (d *DB) Bootstrap(root string, v *version.Version, defaultLine string, journal *JournalEntry) error {
	if v == nil {
		return errors.New("meta: bootstrap 缺少 version")
	}
	if defaultLine == "" {
		defaultLine = "main"
	}
	head := v.ID()
	return d.withWrite(func(tx *sql.Tx) error {
		if err := insertVersionNoJournalTx(tx, v); err != nil {
			return err
		}
		if journal != nil {
			if err := insertJournalTx(tx, *journal); err != nil {
				return err
			}
		}
		now := time.Now().UnixMilli()
		if _, err := tx.Exec(
			`UPDATE repo SET root=?, default_line=?, mainline_head=?, version_count=1, epoch=? WHERE id=1`,
			root, defaultLine, head.String(), int64(v.Epoch)); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO line(name, head, protected, created_from, created_ts, updated_ts)
			 VALUES(?,?,1,?,?,?)
			 ON CONFLICT(name) DO UPDATE SET head=excluded.head, protected=1,
			   created_from=excluded.created_from, updated_ts=excluded.updated_ts, deleted_ts=0`,
			defaultLine, head.String(), hash.Zero.String(), now, now); err != nil {
			return err
		}
		return nil
	})
}

// SetRootState 在事务里修复 repo head/line，不新建版本。
//
// 用于 repair：认领已有 root v0 后补 mainline_head、line 与 version_count。
func (d *DB) SetRootState(root, defaultLine string, head hash.ID, count int64, epoch uint64) error {
	if defaultLine == "" {
		defaultLine = "main"
	}
	return d.withWrite(func(tx *sql.Tx) error {
		return setRootStateTx(tx, root, defaultLine, head, count, epoch)
	})
}

func setRootStateTx(tx *sql.Tx, root, defaultLine string, head hash.ID, count int64, epoch uint64) error {
	now := time.Now().UnixMilli()
	if _, err := tx.Exec(
		`UPDATE repo SET root=?, default_line=?, mainline_head=?, version_count=?, epoch=? WHERE id=1`,
		root, defaultLine, head.String(), count, int64(epoch)); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO line(name, head, protected, created_from, created_ts, updated_ts)
		 VALUES(?,?,1,?,?,?)
		 ON CONFLICT(name) DO UPDATE SET head=excluded.head, protected=1,
		   updated_ts=excluded.updated_ts, deleted_ts=0`,
		defaultLine, head.String(), hash.Zero.String(), now, now); err != nil {
		return err
	}
	return nil
}

// SwapRoot 用一个新 v0 替换旧 v0（不写 repo.root）。
func (d *DB) SwapRoot(old hash.ID, v *version.Version, defaultLine string, journal *JournalEntry) error {
	return d.swapRootAt("", old, v, defaultLine, journal, nil)
}

// SwapRootAt 与 SwapRoot 相同，但显式写入 repo.root。
func (d *DB) SwapRootAt(root string, old hash.ID, v *version.Version, defaultLine string, journal *JournalEntry) error {
	return d.swapRootAt(root, old, v, defaultLine, journal, nil)
}

// SwapFault 是测试/验收用的事务内失败注入。
type SwapFault func(step string) error

// SwapRootAtWithFault 暴露事务内故障点：swap_after_delete / swap_after_insert /
// swap_before_commit。返回 error 时整个 SQLite 事务回滚。
func (d *DB) SwapRootAtWithFault(root string, old hash.ID, v *version.Version,
	defaultLine string, journal *JournalEntry, fault SwapFault) error {
	return d.swapRootAt(root, old, v, defaultLine, journal, fault)
}

func (d *DB) swapRootAt(root string, old hash.ID, v *version.Version,
	defaultLine string, journal *JournalEntry, fault SwapFault) error {
	if v == nil {
		return errors.New("meta: swap 缺少 version")
	}
	if defaultLine == "" {
		defaultLine = "main"
	}
	head := v.ID()
	fail := func(step string) error {
		if fault != nil {
			return fault(step)
		}
		return nil
	}
	return d.withWrite(func(tx *sql.Tx) error {
		if !old.IsZero() {
			if _, err := tx.Exec(`DELETE FROM version_parent WHERE version_id=?`, old.String()); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM version WHERE id=?`, old.String()); err != nil {
				return err
			}
		}
		if err := fail("swap_after_delete"); err != nil {
			return err
		}
		if err := insertVersionNoJournalTx(tx, v); err != nil {
			return err
		}
		if journal != nil {
			if err := insertJournalTx(tx, *journal); err != nil {
				return err
			}
		}
		if err := fail("swap_after_insert"); err != nil {
			return err
		}
		if err := setRootStateTx(tx, root, defaultLine, head, 1, v.Epoch); err != nil {
			return err
		}
		return fail("swap_before_commit")
	})
}

func insertVersionNoJournalTx(tx *sql.Tx, v *version.Version) error {
	id := v.ID()
	_, err := tx.Exec(
		`INSERT INTO version(id, parent, tree, epoch, agent, task_hash, task_desc,
		                     ts, msg, ws_id, journal_id,
		                     idempotency_key, idempotency_tool, idempotency_ts)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO NOTHING`,
		id.String(), v.Parent.String(), v.Tree.String(), int64(v.Epoch),
		v.Agent, v.TaskHash.String(), v.TaskDesc, v.TS, v.Msg,
		nullableString(v.WsID), nullableInt(v.JournalID),
		nullableString(v.IdempotencyKey), nullableString(v.IdempotencyTool),
		nullableInt(v.IdempotencyTS))
	if err != nil {
		return err
	}
	return insertVersionParentsTx(tx, id, v)
}

func insertJournalTx(tx *sql.Tx, e JournalEntry) error {
	_, err := tx.Exec(
		`INSERT INTO journal(version_id, ws_id, agent, task_desc, ts, ops_json)
		 VALUES(?,?,?,?,?,?)`,
		e.VersionID, e.WsID, e.Agent, e.TaskDesc, e.TS, e.OpsJSON)
	return err
}
