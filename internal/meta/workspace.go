package meta

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/op"
)

// 工作区状态取值（§4.3）。
const (
	StateActive      = "active"
	StateRebasing    = "rebasing"
	StateConflicted  = "conflicted"
	StateIntegrating = "integrating"
	StateBroken      = "broken"
)

// 物化模式（D21）。
const (
	MaterializeOverlay = "overlay"
	MaterializeCopy    = "copy"
)

// 父类型。
const (
	ParentVersion   = "version"
	ParentWorkspace = "workspace"
)

// Workspace 是 workspace 表的一行（§4.1）。
type Workspace struct {
	ID          string
	Name        string
	ParentKind  string
	ParentID    string
	BaseTree    hash.ID
	BaseVersion hash.ID
	TaskDesc    string
	State       string
	Materialize string
	Mounted     bool
	CreatedTS   int64
	UpdatedTS   int64
	// MaterializedTS 是工作区最近一次物化（挂载/复制）完成时刻，Unix 纳秒。
	//
	// 采集用它做**惰性判定**：文件不比这个时刻更新，就说明它仍是基准树里
	// 那一份，可以直接复用对象 id，不必读盘哈希（见 capture.Options）。
	MaterializedTS int64
}

// InsertWorkspace 插入一行。
func (d *DB) InsertWorkspace(w Workspace) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO workspace(id, name, parent_kind, parent_id, base_tree,
			                       base_version, task_desc, state, materialize, mounted,
			                       created_ts, updated_ts, materialized_ts)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			w.ID, w.Name, w.ParentKind, w.ParentID, w.BaseTree.String(),
			w.BaseVersion.String(), w.TaskDesc, w.State, w.Materialize, boolToInt(w.Mounted),
			w.CreatedTS, w.UpdatedTS, w.MaterializedTS)
		return err
	})
}

// GetWorkspace 按 id 读取。不存在返回 ErrNotExist。
func (d *DB) GetWorkspace(id string) (*Workspace, error) {
	var (
		w           Workspace
		baseTreeStr string
		baseVerStr  string
		mounted     int
	)
	err := d.sql.QueryRow(
		`SELECT id, name, parent_kind, parent_id, base_tree, base_version,
		        task_desc, state, materialize, mounted, created_ts, updated_ts,
		        materialized_ts
		   FROM workspace WHERE id = ?`, id).
		Scan(&w.ID, &w.Name, &w.ParentKind, &w.ParentID, &baseTreeStr, &baseVerStr,
			&w.TaskDesc, &w.State, &w.Materialize, &mounted, &w.CreatedTS, &w.UpdatedTS,
			&w.MaterializedTS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	w.Mounted = mounted != 0
	if w.BaseTree, err = hash.Parse(baseTreeStr); err != nil {
		return nil, err
	}
	if w.BaseVersion, err = hash.Parse(baseVerStr); err != nil {
		return nil, err
	}
	return &w, nil
}

// ListWorkspaces 返回全部工作区（按创建时间升序）。
func (d *DB) ListWorkspaces() ([]Workspace, error) {
	rows, err := d.sql.Query(
		`SELECT id, name, parent_kind, parent_id, base_tree, base_version,
		        task_desc, state, materialize, mounted, created_ts, updated_ts
		   FROM workspace ORDER BY created_ts, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Workspace
	for rows.Next() {
		var (
			w       Workspace
			bt, bv  string
			mounted int
		)
		if err := rows.Scan(&w.ID, &w.Name, &w.ParentKind, &w.ParentID, &bt, &bv,
			&w.TaskDesc, &w.State, &w.Materialize, &mounted,
			&w.CreatedTS, &w.UpdatedTS); err != nil {
			return nil, err
		}
		w.Mounted = mounted != 0
		if w.BaseTree, err = hash.Parse(bt); err != nil {
			return nil, err
		}
		if w.BaseVersion, err = hash.Parse(bv); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// CountWorkspaces 返回工作区总数（含所有状态）。
func (d *DB) CountWorkspaces() (int, error) {
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM workspace`).Scan(&n)
	return n, err
}

// UpdateWorkspaceState 更新状态与 updated_ts。
func (d *DB) UpdateWorkspaceState(id, state string, ts int64) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE workspace SET state = ?, updated_ts = ? WHERE id = ?`,
			state, ts, id)
		return err
	})
}

// UpdateWorkspaceMount 更新物化模式、挂载标记与物化时刻。
func (d *DB) UpdateWorkspaceMount(id, mode string, mounted bool, refTS, materializedTS int64) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE workspace SET materialize = ?, mounted = ?, updated_ts = ?,
			                      materialized_ts = ? WHERE id = ?`,
			mode, boolToInt(mounted), refTS, materializedTS, id)
		return err
	})
}

// UpdateWorkspaceParent 更新父指针（§4.4 的重挂）。
func (d *DB) UpdateWorkspaceParent(id, kind, parentID string, ts int64) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE workspace SET parent_kind = ?, parent_id = ?, updated_ts = ? WHERE id = ?`,
			kind, parentID, ts, id)
		return err
	})
}

// UpdateWorkspaceBase 更新基准树与基准版本（§4.9.2 的 ws_rebase）。
func (d *DB) UpdateWorkspaceBase(id string, baseTree, baseVersion hash.ID, ts int64) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE workspace SET base_tree = ?, base_version = ?, updated_ts = ? WHERE id = ?`,
			baseTree.String(), baseVersion.String(), ts, id)
		return err
	})
}

// DeleteWorkspace 删除工作区（级联清理 op/lease/stat_cache/conflict）。
func (d *DB) DeleteWorkspace(id string) error {
	return d.withWrite(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM stat_cache WHERE ws_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM op WHERE ws_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM ws_effective_tree WHERE ws_id = ?`, id); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM workspace WHERE id = ?`, id)
		return err
	})
}

// Children 返回某个工作区的子工作区。
func (d *DB) Children(id string) ([]Workspace, error) {
	all, err := d.ListWorkspaces()
	if err != nil {
		return nil, err
	}
	var out []Workspace
	for _, w := range all {
		if w.ParentKind == ParentWorkspace && w.ParentID == id {
			out = append(out, w)
		}
	}
	return out, nil
}

// --- landed_ws（幂等，§11.6） ------------------------------------------

// InsertLandedWS 记录一个已落地的工作区。
func (d *DB) InsertLandedWS(wsID, versionID string, ts int64) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO landed_ws(ws_id, version_id, ts) VALUES(?,?,?)
			 ON CONFLICT(ws_id) DO UPDATE SET version_id=excluded.version_id, ts=excluded.ts`,
			wsID, versionID, ts)
		return err
	})
}

// GetLandedWS 查询工作区是否已落地及其版本。
func (d *DB) GetLandedWS(wsID string) (string, bool, error) {
	var vid string
	err := d.sql.QueryRow(`SELECT version_id FROM landed_ws WHERE ws_id = ?`, wsID).Scan(&vid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return vid, true, nil
}

// --- 有效树缓存（§4.9.1） ----------------------------------------------

// GetEffectiveTree 读取缓存；opsEpoch 不匹配视为未命中。
func (d *DB) GetEffectiveTree(wsID string, opsEpoch int64) (hash.ID, bool, error) {
	var (
		epoch int64
		tree  string
	)
	err := d.sql.QueryRow(
		`SELECT ops_epoch, tree FROM ws_effective_tree WHERE ws_id = ?`, wsID).
		Scan(&epoch, &tree)
	if errors.Is(err, sql.ErrNoRows) {
		return hash.ID{}, false, nil
	}
	if err != nil {
		return hash.ID{}, false, err
	}
	if epoch != opsEpoch {
		return hash.ID{}, false, nil
	}
	id, err := hash.Parse(tree)
	if err != nil {
		return hash.ID{}, false, err
	}
	return id, true, nil
}

// PutEffectiveTree 写入缓存。
func (d *DB) PutEffectiveTree(wsID string, opsEpoch int64, tree hash.ID) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO ws_effective_tree(ws_id, ops_epoch, tree) VALUES(?,?,?)
			 ON CONFLICT(ws_id) DO UPDATE SET ops_epoch=excluded.ops_epoch, tree=excluded.tree`,
			wsID, opsEpoch, tree.String())
		return err
	})
}

// --- 租约（§4.5） -------------------------------------------------------

// Lease 是 lease 表的一行。
type Lease struct {
	WsID string
	Path string
	Mode string
}

// AddLease 登记租约（幂等）。
func (d *DB) AddLease(wsID, path, mode string, ts int64) error {
	return d.withWrite(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM lease WHERE ws_id=? AND path=?`, wsID, path).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			_, err := tx.Exec(`UPDATE lease SET mode=? WHERE ws_id=? AND path=?`,
				mode, wsID, path)
			return err
		}
		_, err := tx.Exec(
			`INSERT INTO lease(ws_id, path, mode, created_ts) VALUES(?,?,?,?)`,
			wsID, path, mode, ts)
		return err
	})
}

// ReleaseLease 解除租约。
func (d *DB) ReleaseLease(wsID, path string) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM lease WHERE ws_id=? AND path=?`, wsID, path)
		return err
	})
}

// ListLeases 返回全部租约。
func (d *DB) ListLeases() ([]Lease, error) {
	rows, err := d.sql.Query(`SELECT ws_id, path, mode FROM lease ORDER BY path, ws_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Lease
	for rows.Next() {
		var l Lease
		if err := rows.Scan(&l.WsID, &l.Path, &l.Mode); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// --- op（§5.1） ---------------------------------------------------------

// InsertOps 追加一批 op。
func (d *DB) InsertOps(ops []op.Op) error {
	if len(ops) == 0 {
		return nil
	}
	return d.withWrite(func(tx *sql.Tx) error {
		for i := range ops {
			if err := insertOpTx(tx, &ops[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

func insertOpTx(tx *sql.Tx, o *op.Op) error {
	hunks, err := op.MarshalHunks(o.Hunks)
	if err != nil {
		return err
	}
	var attrJSON any
	var eol any
	if o.Attr != nil {
		attrJSON = fmt.Sprintf(`{"eol":%d}`, o.Attr.EOL)
		eol = int(o.Attr.EOL)
	}
	var dest any
	if o.DestPath != "" {
		dest = o.DestPath
	}
	_, err = tx.Exec(
		`INSERT INTO op(id, ws_id, seq, kind, path, hunks_json, attr_json, eol,
		                dest_path, entry_kind, mode_before, mode_after,
		                blob_before, blob_after,
		                captured_ts, captured_by, derived)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   seq=excluded.seq, kind=excluded.kind, path=excluded.path,
		   hunks_json=excluded.hunks_json, attr_json=excluded.attr_json,
		   eol=excluded.eol, dest_path=excluded.dest_path,
		   entry_kind=excluded.entry_kind,
		   mode_before=excluded.mode_before, mode_after=excluded.mode_after,
		   blob_before=excluded.blob_before, blob_after=excluded.blob_after`,
		o.ID, o.WsID, o.Seq, string(o.Kind), o.Path, hunks, attrJSON, eol,
		dest, int(o.EntryKindOf()), modeOrNil(o.ModeBefore), modeOrNil(o.ModeAfter),
		hashOrNil(o.BaseFile), hashOrNil(o.NewFile),
		o.CapturedTS, o.CapturedBy, boolToInt(o.Derived))
	return err
}

// ListOps 按 seq 升序返回某工作区的 op。
func (d *DB) ListOps(wsID string) ([]op.Op, error) {
	rows, err := d.sql.Query(
		`SELECT id, ws_id, seq, kind, path, hunks_json, eol, dest_path, entry_kind,
		        mode_before, mode_after, blob_before, blob_after, attr_json,
		        captured_ts, captured_by, derived
		   FROM op WHERE ws_id = ? ORDER BY seq`, wsID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []op.Op
	for rows.Next() {
		var (
			o         op.Op
			kind      string
			hunks     string
			eol       sql.NullInt64
			dest      sql.NullString
			entryKind int
			modeB     sql.NullInt64
			modeA     sql.NullInt64
			before    sql.NullString
			after     sql.NullString
			attrJSON  sql.NullString
			derived   int
		)
		if err := rows.Scan(&o.ID, &o.WsID, &o.Seq, &kind, &o.Path, &hunks, &eol,
			&dest, &entryKind, &modeB, &modeA, &before, &after, &attrJSON,
			&o.CapturedTS, &o.CapturedBy, &derived); err != nil {
			return nil, err
		}
		o.Kind = op.Kind(kind)
		o.EntryKind = uint8(entryKind)
		o.Derived = derived != 0
		if modeB.Valid {
			o.ModeBefore = op.ModePtr(uint64(modeB.Int64))
		}
		if modeA.Valid {
			o.ModeAfter = op.ModePtr(uint64(modeA.Int64))
		}
		if o.Hunks, err = op.UnmarshalHunks(hunks); err != nil {
			return nil, err
		}
		if eol.Valid {
			o.Attr = &op.Attr{EOL: uint8(eol.Int64)}
		}
		if dest.Valid {
			o.DestPath = dest.String
		}
		o.BaseFile = parseHashOrZero(before)
		o.NewFile = parseHashOrZero(after)
		out = append(out, o)
	}
	return out, rows.Err()
}

// CountOps 返回某工作区的 op 数。
func (d *DB) CountOps(wsID string) (int64, error) {
	var n int64
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM op WHERE ws_id = ?`, wsID).Scan(&n)
	return n, err
}

// DeleteOpsUnder 删除某路径及其下的 op（文件被删除时调用）。
func (d *DB) DeleteOpsUnder(wsID, path string) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`DELETE FROM op WHERE ws_id = ? AND (path = ? OR path LIKE ? ESCAPE '\')`,
			wsID, path, escapeLike(path)+"/%")
		return err
	})
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// DeleteOpsUnderRoot 清空某工作区的全部 op（采集时整体重建 op-log，§4.1）。
func (d *DB) DeleteOpsUnderRoot(wsID string) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM op WHERE ws_id = ?`, wsID)
		return err
	})
}
