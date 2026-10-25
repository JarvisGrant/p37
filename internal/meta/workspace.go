package meta

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

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
	BaseLine    string
	TaskDesc    string
	State       string
	Materialize string
	Mounted     bool
	Hidden      bool
	Kind        string
	LandedTS    int64
	CreatedTS   int64
	UpdatedTS   int64
	// OpsEpoch 是 op-log 的单调版本号：每次 Sync 整体重建 op-log 都 +1。
	// 有效树缓存与 land 预演令牌以它为键，避免「op 条数相同但内容变化」
	// 造成缓存误命中（会话 #90 的回退根因）。
	OpsEpoch int64
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
			                       base_version, base_line, task_desc, state, materialize, mounted,
			                       hidden, kind, landed_ts,
			                       created_ts, updated_ts, materialized_ts)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			w.ID, w.Name, w.ParentKind, w.ParentID, w.BaseTree.String(),
			w.BaseVersion.String(), w.BaseLine, w.TaskDesc, w.State, w.Materialize, boolToInt(w.Mounted),
			boolToInt(w.Hidden), w.Kind, w.LandedTS,
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
	var hidden, landed int64
	var kind string
	var baseLine string
	err := d.sql.QueryRow(
		`SELECT id, name, parent_kind, parent_id, base_tree, base_version, base_line,
		        task_desc, state, materialize, mounted, hidden, kind, landed_ts,
		        created_ts, updated_ts, materialized_ts, ops_epoch
		   FROM workspace WHERE id = ?`, id).
		Scan(&w.ID, &w.Name, &w.ParentKind, &w.ParentID, &baseTreeStr, &baseVerStr, &baseLine,
			&w.TaskDesc, &w.State, &w.Materialize, &mounted, &hidden, &kind, &landed,
			&w.CreatedTS, &w.UpdatedTS, &w.MaterializedTS, &w.OpsEpoch)
	w.BaseLine = baseLine
	w.Hidden = hidden != 0
	w.Kind = kind
	w.LandedTS = landed
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
		`SELECT id, name, parent_kind, parent_id, base_tree, base_version, base_line,
		        task_desc, state, materialize, mounted, hidden, kind, landed_ts,
		        created_ts, updated_ts, materialized_ts, ops_epoch
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
		var baseLine, kind string
		var hidden, landed int64
		if err := rows.Scan(&w.ID, &w.Name, &w.ParentKind, &w.ParentID, &bt, &bv, &baseLine,
			&w.TaskDesc, &w.State, &w.Materialize, &mounted, &hidden, &kind, &landed,
			&w.CreatedTS, &w.UpdatedTS, &w.MaterializedTS, &w.OpsEpoch); err != nil {
			return nil, err
		}
		w.Mounted = mounted != 0
		w.BaseLine = baseLine
		w.Hidden = hidden != 0
		w.Kind = kind
		w.LandedTS = landed
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

// DeleteWorkspace 删除工作区（级联清理 op/lease/stat_cache/conflict/resolution）。
func (d *DB) DeleteWorkspace(id string) error {
	return d.deleteWorkspaceTx(id, nil)
}

// DeleteWorkspaceWithTombstone 在同一事务里删除工作区并写回收记录。
func (d *DB) DeleteWorkspaceWithTombstone(id string, t WSTombstone) error {
	return d.deleteWorkspaceTx(id, &t)
}

func (d *DB) deleteWorkspaceTx(id string, t *WSTombstone) error {
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
		// conflict / lease 有 ON DELETE CASCADE，但显式删除不依赖 pragma，
		// 也让「删行」与 FinishLand 的清理集合保持一致。
		if _, err := tx.Exec(`DELETE FROM conflict WHERE ws_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM lease WHERE ws_id = ?`, id); err != nil {
			return err
		}
		// resolution 没有外键（key 是主键），不删会随工作区使用单调增长。
		if _, err := tx.Exec(`DELETE FROM resolution WHERE ws_id = ?`, id); err != nil {
			return err
		}
		if t != nil {
			if err := d.insertTombstoneTx(tx, *t); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`DELETE FROM workspace WHERE id = ?`, id)
		return err
	})
}

// FindLiveWorkspaceByNameParent 按 (name, canonical parent) 查一个仍可写入的
// 工作区。line parent 存的是 base_line（workspace.parent_id 是当时的 version），
// version/workspace parent 用 parent_kind+parent_id 精确匹配。
func (d *DB) FindLiveWorkspaceByNameParent(name, kind, id string) (*Workspace, error) {
	base := `SELECT id, name, parent_kind, parent_id, base_tree, base_version, base_line,
	                task_desc, state, materialize, mounted, hidden, kind, landed_ts,
	                created_ts, updated_ts, materialized_ts
	           FROM workspace
	          WHERE name=? AND hidden=0 AND landed_ts=0
	            AND state IN ('active','conflicted','rebasing')`
	var row *sql.Row
	switch kind {
	case "line":
		row = d.sql.QueryRow(base+` AND base_line=? ORDER BY created_ts LIMIT 1`, name, id)
	case "version":
		row = d.sql.QueryRow(base+` AND parent_kind='version' AND parent_id=? AND base_line=''
		                          ORDER BY created_ts LIMIT 1`, name, id)
	case "workspace":
		row = d.sql.QueryRow(base+` AND parent_kind='workspace' AND parent_id=?
		                          ORDER BY created_ts LIMIT 1`, name, id)
	default:
		row = d.sql.QueryRow(base+` ORDER BY created_ts LIMIT 1`, name)
	}
	var (
		w           Workspace
		baseTreeStr string
		baseVerStr  string
		mounted     int
		hidden      int64
		landed      int64
	)
	err := row.Scan(&w.ID, &w.Name, &w.ParentKind, &w.ParentID, &baseTreeStr, &baseVerStr,
		&w.BaseLine, &w.TaskDesc, &w.State, &w.Materialize, &mounted, &hidden, &w.Kind,
		&landed, &w.CreatedTS, &w.UpdatedTS, &w.MaterializedTS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	w.Mounted = mounted != 0
	w.Hidden = hidden != 0
	w.LandedTS = landed
	if w.BaseTree, err = hash.Parse(baseTreeStr); err != nil {
		return nil, err
	}
	if w.BaseVersion, err = hash.Parse(baseVerStr); err != nil {
		return nil, err
	}
	return &w, nil
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

// LandedWSInfo 是 landed_ws 的完整视图。
type LandedWSInfo struct {
	WSID       string
	VersionID  string
	TargetKind string
	TargetID   string
	TS         int64
}

// InsertLandedWS 记录一个已落地的工作区。
func (d *DB) InsertLandedWS(wsID, versionID string, ts int64) error {
	return d.InsertLandedWSWithTarget(wsID, versionID, "", "", ts)
}

// InsertLandedWSWithTarget 记录落地版本与目标（line / workspace）。
func (d *DB) InsertLandedWSWithTarget(wsID, versionID, targetKind, targetID string, ts int64) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO landed_ws(ws_id, version_id, target_kind, target_id, ts) VALUES(?,?,?,?,?)
			 ON CONFLICT(ws_id) DO UPDATE SET version_id=excluded.version_id,
			   target_kind=excluded.target_kind, target_id=excluded.target_id, ts=excluded.ts`,
			wsID, versionID, targetKind, targetID, ts)
		return err
	})
}

// GetLandedWS 查询工作区是否已落地及其版本。
func (d *DB) GetLandedWS(wsID string) (string, bool, error) {
	info, ok, err := d.GetLandedWSInfo(wsID)
	if err != nil || !ok {
		return "", ok, err
	}
	return info.VersionID, true, nil
}

// GetLandedWSInfo 查询落地版本与目标。
func (d *DB) GetLandedWSInfo(wsID string) (*LandedWSInfo, bool, error) {
	var info LandedWSInfo
	err := d.sql.QueryRow(
		`SELECT ws_id, version_id, target_kind, target_id, ts
		   FROM landed_ws WHERE ws_id = ?`, wsID).
		Scan(&info.WSID, &info.VersionID, &info.TargetKind, &info.TargetID, &info.TS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &info, true, nil
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

// ReplaceOps 用一个事务整体替换某工作区的 op-log。
//
// 这是 Sync 的唯一写入口。三件事必须原子完成，否则会出现「op 已换新、
// 有效树缓存还是旧树」的窗口（会话 #90：父的测试修正被子会话 land 回退）：
//
//  1. 清空旧 op；
//  2. 写入新 op；
//  3. ops_epoch+1 并删除 ws_effective_tree 缓存。
//
// 即使调用方传空 ops 也要执行，因为「改动被撤销」同样是一次版本推进。
func (d *DB) ReplaceOps(wsID string, ops []op.Op) error {
	return d.withWrite(func(tx *sql.Tx) error {
		cur, err := opsSignatureTx(tx, wsID)
		if err != nil {
			return err
		}
		next, err := opsSignature(ops)
		if err != nil {
			return err
		}
		if cur == next {
			// 周期扫描 / ws_sync 在「内容没变」时不推进版本，否则 P34 拿到的
			// ops_epoch 会被无意义地刷新，preview → land 之间凭空出现 target_moved。
			return nil
		}
		if _, err := tx.Exec(`DELETE FROM op WHERE ws_id = ?`, wsID); err != nil {
			return err
		}
		for i := range ops {
			if err := insertOpTx(tx, &ops[i]); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(
			`UPDATE workspace SET ops_epoch = ops_epoch + 1 WHERE id = ?`, wsID); err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM ws_effective_tree WHERE ws_id = ?`, wsID)
		return err
	})
}

// opsSignatureTx 读取当前 op-log 的规范化签名（忽略 captured_ts/by 这类噪声）。
func opsSignatureTx(tx *sql.Tx, wsID string) (string, error) {
	rows, err := tx.Query(
		`SELECT id, seq, kind, path, COALESCE(dest_path,''), entry_kind,
		        COALESCE(mode_before,-1), COALESCE(mode_after,-1),
		        COALESCE(blob_before,''), COALESCE(blob_after,''),
		        hunks_json, COALESCE(attr_json,'')
		   FROM op WHERE ws_id = ? ORDER BY seq`, wsID)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	var b strings.Builder
	for rows.Next() {
		var (
			id, kind, path, dest, hunks, attr string
			seq, entryKind, modeB, modeA      int64
			before, after                     string
		)
		if err := rows.Scan(&id, &seq, &kind, &path, &dest, &entryKind,
			&modeB, &modeA, &before, &after, &hunks, &attr); err != nil {
			return "", err
		}
		writeOpSig(&b, id, seq, kind, path, dest, entryKind, modeB, modeA, before, after, hunks, attr)
	}
	return b.String(), rows.Err()
}

// opsSignature 把待写入的 op 规范化为与 opsSignatureTx 完全一致的字符串。
func opsSignature(ops []op.Op) (string, error) {
	var b strings.Builder
	for i := range ops {
		o := &ops[i]
		hunks, err := op.MarshalHunks(o.Hunks)
		if err != nil {
			return "", err
		}
		attr := ""
		if o.Attr != nil {
			attr = fmt.Sprintf(`{"eol":%d}`, o.Attr.EOL)
		}
		writeOpSig(&b, o.ID, o.Seq, string(o.Kind), o.Path, o.DestPath,
			int64(o.EntryKindOf()), modeSig(o.ModeBefore), modeSig(o.ModeAfter),
			hashSig(o.BaseFile), hashSig(o.NewFile), hunks, attr)
	}
	return b.String(), nil
}

func writeOpSig(b *strings.Builder, id string, seq int64, kind, path, dest string,
	entryKind, modeB, modeA int64, before, after, hunks, attr string) {
	fmt.Fprintf(b, "%s\x00%d\x00%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%s\x00%s\x00%s\x00%s\n",
		id, seq, kind, path, dest, entryKind, modeB, modeA, before, after, hunks, attr)
}

func modeSig(m *uint64) int64 {
	if m == nil {
		return -1
	}
	return int64(*m)
}

func hashSig(h hash.ID) string {
	if h.IsZero() {
		return ""
	}
	return h.String()
}

// WorkspaceOpsEpoch 返回工作区的单调 op-log 版本号。
func (d *DB) WorkspaceOpsEpoch(wsID string) (int64, error) {
	var epoch int64
	err := d.sql.QueryRow(`SELECT ops_epoch FROM workspace WHERE id = ?`, wsID).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotExist
	}
	return epoch, err
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

// SetWorkspaceLanded 标记工作区已落地（drop_workspace=false 时保留但只读）。
func (d *DB) SetWorkspaceLanded(id string, ts int64) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE workspace SET landed_ts=?, state=?, updated_ts=? WHERE id=?`,
			ts, StateActive, ts, id)
		return err
	})
}

// CountVisibleWorkspaces 返回**真正占用 max_active** 的工作区数。
//
// 只统计 hidden=0、未落地、且处于活跃状态的行：
//   - landed_ts != 0 的行是 drop_workspace=false 保留下来的只读工作区，
//     它们不再参与并发/磁盘预算，必须排除，否则配额只减不增；
//   - integrating 是 land 的中间态（提交点前后），由 ReconcileLand 收尾；
//   - broken 不计入额度（挂载/目录异常时不应把用户锁死），但在 ws_quota
//     里单独暴露，便于调用方决定是否清理。
func (d *DB) CountVisibleWorkspaces() (int, error) {
	var n int
	err := d.sql.QueryRow(
		`SELECT COUNT(*) FROM workspace
		  WHERE hidden=0 AND landed_ts=0 AND state IN (?,?,?)`,
		StateActive, StateConflicted, StateRebasing).Scan(&n)
	return n, err
}

// CountLandedWorkspaces 返回 drop_workspace=false 保留下来的只读工作区数。
func (d *DB) CountLandedWorkspaces() (int, error) {
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM workspace WHERE hidden=0 AND landed_ts<>0`).Scan(&n)
	return n, err
}

// CountBrokenWorkspaces 返回可见的 broken 工作区数。
func (d *DB) CountBrokenWorkspaces() (int, error) {
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM workspace WHERE hidden=0 AND landed_ts=0 AND state=?`,
		StateBroken).Scan(&n)
	return n, err
}

// CountHiddenWorkspaces 返回隐藏工作区数。
func (d *DB) CountHiddenWorkspaces() (int, error) {
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM workspace WHERE hidden=1`).Scan(&n)
	return n, err
}

// UpdateWorkspaceBaseLine 更新基准树、line 名与 updated_ts（line 父 rebase）。
func (d *DB) UpdateWorkspaceBaseLine(id string, baseTree, baseVersion hash.ID, baseLine string, ts int64) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE workspace SET base_tree=?, base_version=?, base_line=?, updated_ts=? WHERE id=?`,
			baseTree.String(), baseVersion.String(), baseLine, ts, id)
		return err
	})
}

// SetWorkspaceHidden 设置隐藏标记与 kind。
func (d *DB) SetWorkspaceHidden(id string, hidden bool, kind string) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE workspace SET hidden=?, kind=? WHERE id=?`,
			boolToInt(hidden), kind, id)
		return err
	})
}
