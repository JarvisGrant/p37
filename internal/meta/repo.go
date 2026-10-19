package meta

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/version"
)

// ErrNoHead 表示主干尚未初始化。
var ErrNoHead = errors.New("meta: 主干尚未初始化")

// Repo 是 repo 表的单行快照。
type Repo struct {
	Root         string
	CreatedTS    int64
	FormatVer    int
	MainlineHead hash.ID // 无效时 IsZero()
	VersionCount int64
	Epoch        uint64
}

// GetRepo 读取仓库单行。
func (d *DB) GetRepo() (Repo, error) {
	var (
		r    Repo
		head sql.NullString
	)
	err := d.sql.QueryRow(
		`SELECT root, created_ts, format_ver, mainline_head, version_count, epoch
		   FROM repo WHERE id = 1`).
		Scan(&r.Root, &r.CreatedTS, &r.FormatVer, &head, &r.VersionCount, &r.Epoch)
	if err != nil {
		return Repo{}, err
	}
	if head.Valid && head.String != "" {
		id, err := hash.Parse(head.String)
		if err != nil {
			return Repo{}, fmt.Errorf("meta: mainline_head 非法: %w", err)
		}
		r.MainlineHead = id
	}
	return r, nil
}

// SetMainlineHead 更新主干 head 与版本计数。
func (d *DB) SetMainlineHead(head hash.ID, versionCount int64, epoch uint64) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE repo SET mainline_head = ?, version_count = ?, epoch = ? WHERE id = 1`,
			head.String(), versionCount, epoch)
		return err
	})
}

// Head 返回主干 head 与是否存在。
func (d *DB) Head() (hash.ID, bool, error) {
	r, err := d.GetRepo()
	if err != nil {
		return hash.ID{}, false, err
	}
	return r.MainlineHead, !r.MainlineHead.IsZero(), nil
}

// InsertVersion 写入一条 version 记录。
func (d *DB) InsertVersion(v *version.Version) error {
	id := v.ID()
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO version(id, parent, tree, epoch, agent, task_hash, task_desc,
			                     ts, msg, ws_id, journal_id)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(id) DO NOTHING`,
			id.String(), v.Parent.String(), v.Tree.String(), int64(v.Epoch),
			v.Agent, v.TaskHash.String(), v.TaskDesc, v.TS, v.Msg,
			nullableString(v.WsID), nullableInt(v.JournalID))
		return err
	})
}

// GetVersion 按 id 读取 version 记录。不存在返回 ErrNotExist。
func (d *DB) GetVersion(id hash.ID) (*version.Version, error) {
	var (
		idStr, parentStr, treeStr, taskHash string
		epoch                               int64
		agent, taskDesc, msg                string
		ts                                  int64
		wsID                                sql.NullString
		journalID                           sql.NullInt64
	)
	err := d.sql.QueryRow(
		`SELECT id, parent, tree, epoch, agent, task_hash, task_desc, ts, msg, ws_id, journal_id
		   FROM version WHERE id = ?`, id.String()).
		Scan(&idStr, &parentStr, &treeStr, &epoch, &agent, &taskHash, &taskDesc,
			&ts, &msg, &wsID, &journalID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotExist
	}
	if err != nil {
		return nil, err
	}

	parent, err := hash.Parse(parentStr)
	if err != nil {
		return nil, err
	}
	tree, err := hash.Parse(treeStr)
	if err != nil {
		return nil, err
	}
	th, err := hash.Parse(taskHash)
	if err != nil {
		return nil, err
	}
	v := &version.Version{Meta: version.Meta{
		Parent:   parent,
		Tree:     tree,
		Epoch:    uint64(epoch),
		Agent:    agent,
		TaskHash: th,
		TaskDesc: taskDesc,
		TS:       ts,
		Msg:      msg,
	}}
	if wsID.Valid {
		v.WsID = wsID.String
	}
	if journalID.Valid {
		v.JournalID = journalID.Int64
	}
	// 校验存储的 id 与重算一致（防止元数据被篡改）。
	if v.ID() != id {
		return nil, fmt.Errorf("meta: version %s 内容与 id 不符（重算为 %s）", id, v.ID())
	}
	return v, nil
}

// ErrNotExist 表示记录不存在。
var ErrNotExist = errors.New("meta: 记录不存在")

// ErrAmbiguous 表示前缀匹配到多个记录。
var ErrAmbiguous = errors.New("meta: 前缀匹配到多个记录")

// FindVersionByPrefix 按 id 十六进制前缀查找唯一版本。
//
// 供 CLI 使用：`p37 log` 打印的是短 id，用户理应用它回喂给 show/revert/blame。
func (d *DB) FindVersionByPrefix(prefix string) (hash.ID, error) {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if prefix == "" {
		return hash.ID{}, ErrNotExist
	}
	rows, err := d.sql.Query(
		`SELECT id FROM version WHERE id LIKE ? || '%' ORDER BY ts DESC LIMIT 2`, prefix)
	if err != nil {
		return hash.ID{}, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return hash.ID{}, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return hash.ID{}, err
	}
	switch len(ids) {
	case 0:
		return hash.ID{}, ErrNotExist
	case 1:
		return hash.Parse(ids[0])
	default:
		return hash.ID{}, ErrAmbiguous
	}
}

// CountVersions 返回版本总数。
func (d *DB) CountVersions() (int64, error) {
	var n int64
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM version`).Scan(&n)
	return n, err
}

// VersionRow 是版本列表项（version_log 用）。
type VersionRow struct {
	ID     hash.ID
	Parent hash.ID
	TS     int64
	Agent  string
	Task   string
	Epoch  uint64
}

// ListVersions 按 ts 倒序返回版本列表。
func (d *DB) ListVersions(limit int, beforeTS int64) ([]VersionRow, error) {
	q := `SELECT id, parent, ts, agent, task_desc, epoch FROM version`
	args := []any{}
	if beforeTS > 0 {
		q += ` WHERE ts < ?`
		args = append(args, beforeTS)
	}
	q += ` ORDER BY ts DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := d.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []VersionRow
	for rows.Next() {
		var (
			idStr, parentStr string
			r                VersionRow
			epoch            int64
		)
		if err := rows.Scan(&idStr, &parentStr, &r.TS, &r.Agent, &r.Task, &epoch); err != nil {
			return nil, err
		}
		if r.ID, err = hash.Parse(idStr); err != nil {
			return nil, err
		}
		if r.Parent, err = hash.Parse(parentStr); err != nil {
			return nil, err
		}
		r.Epoch = uint64(epoch)
		out = append(out, r)
	}
	return out, rows.Err()
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// DeleteVersion 删除一条版本记录（仅用于覆盖初始 v0）。
func (d *DB) DeleteVersion(id hash.ID) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM version WHERE id = ?`, id.String())
		return err
	})
}
