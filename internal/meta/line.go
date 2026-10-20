package meta

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/version"
)

// Line 是一条命名版本线（移动 head）。
type Line struct {
	Name        string
	Head        hash.ID
	Protected   bool
	CreatedFrom hash.ID
	CreatedTS   int64
	UpdatedTS   int64
	DeletedTS   int64
}

// LineExists 报告 line 是否存在（未删除）。
func (d *DB) LineExists(name string) (bool, error) {
	var n int
	err := d.sql.QueryRow(
		`SELECT COUNT(*) FROM line WHERE name = ? AND deleted_ts = 0`, name).Scan(&n)
	return n > 0, err
}

// GetLine 读取一条 line。不存在返回 ErrNotExist。
func (d *DB) GetLine(name string) (*Line, error) {
	var (
		l         Line
		head      string
		from      sql.NullString
		protected int
	)
	err := d.sql.QueryRow(
		`SELECT name, head, protected, created_from, created_ts, updated_ts, deleted_ts
		   FROM line WHERE name = ?`, name).
		Scan(&l.Name, &head, &protected, &from, &l.CreatedTS, &l.UpdatedTS, &l.DeletedTS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	if l.Head, err = hash.Parse(head); err != nil {
		return nil, err
	}
	l.Protected = protected != 0
	if from.Valid && from.String != "" {
		if l.CreatedFrom, err = hash.Parse(from.String); err != nil {
			return nil, err
		}
	}
	return &l, nil
}

// ListLines 返回全部未删除 line（按名字排序）。
func (d *DB) ListLines() ([]Line, error) {
	rows, err := d.sql.Query(
		`SELECT name, head, protected, created_from, created_ts, updated_ts
		   FROM line WHERE deleted_ts = 0 ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Line
	for rows.Next() {
		var (
			l         Line
			head      string
			from      sql.NullString
			protected int
		)
		if err := rows.Scan(&l.Name, &head, &protected, &from, &l.CreatedTS, &l.UpdatedTS); err != nil {
			return nil, err
		}
		if l.Head, err = hash.Parse(head); err != nil {
			return nil, err
		}
		l.Protected = protected != 0
		if from.Valid && from.String != "" {
			if l.CreatedFrom, err = hash.Parse(from.String); err != nil {
				return nil, err
			}
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// CreateLine 创建一条 line。已存在（未删除）返回 ErrLineExists。
func (d *DB) CreateLine(name string, head, from hash.ID, protected bool, ts int64) error {
	return d.withWrite(func(tx *sql.Tx) error {
		var deleted int64
		var existing string
		err := tx.QueryRow(`SELECT head, deleted_ts FROM line WHERE name = ?`, name).
			Scan(&existing, &deleted)
		switch {
		case err == nil && deleted == 0:
			return ErrLineExists
		case err == nil:
			// 已删除的 line 可以重建；保留 created_from 语义由调用方决定。
			_, err = tx.Exec(
				`UPDATE line SET head=?, protected=?, created_from=?, created_ts=?,
				        updated_ts=?, deleted_ts=0 WHERE name=?`,
				head.String(), boolToInt(protected), nullableHash(from), ts, ts, name)
			return err
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		_, err = tx.Exec(
			`INSERT INTO line(name, head, protected, created_from, created_ts, updated_ts)
			 VALUES(?,?,?,?,?,?)`,
			name, head.String(), boolToInt(protected), nullableHash(from), ts, ts)
		return err
	})
}

// ErrLineExists 表示 line 已存在。
var ErrLineExists = errors.New("meta: line 已存在")

// DeleteLine 标记 line 已删除（保留墓碑供远端同步识别）。
func (d *DB) DeleteLine(name string, ts int64) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE line SET deleted_ts = ?, updated_ts = ? WHERE name = ?`,
			ts, ts, name)
		return err
	})
}

// SetLineProtected 设置保护位。
func (d *DB) SetLineProtected(name string, protected bool, ts int64) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE line SET protected=?, updated_ts=? WHERE name=?`,
			boolToInt(protected), ts, name)
		return err
	})
}

// SetLineHead 移动 line head（不产生版本）。仅用于 line_reset / 同步快进。
func (d *DB) SetLineHead(name string, head hash.ID, ts int64) error {
	return d.withWrite(func(tx *sql.Tx) error {
		return setLineHeadTx(tx, name, head, ts)
	})
}

func setLineHeadTx(tx *sql.Tx, name string, head hash.ID, ts int64) error {
	res, err := tx.Exec(`UPDATE line SET head=?, updated_ts=? WHERE name=? AND deleted_ts=0`,
		head.String(), ts, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotExist
	}
	var def string
	if err := tx.QueryRow(`SELECT default_line FROM repo WHERE id=1`).Scan(&def); err != nil {
		return err
	}
	if def == name {
		if _, err := tx.Exec(`UPDATE repo SET mainline_head=? WHERE id=1`, head.String()); err != nil {
			return err
		}
	}
	return nil
}

// LineHead 返回 line 当前 head。
func (d *DB) LineHead(name string) (hash.ID, error) {
	l, err := d.GetLine(name)
	if err != nil {
		return hash.ID{}, err
	}
	if l.DeletedTS != 0 {
		return hash.ID{}, ErrNotExist
	}
	return l.Head, nil
}

// DefaultLine 返回仓库默认 line 名。
func (d *DB) DefaultLine() (string, error) {
	r, err := d.GetRepo()
	if err != nil {
		return "", err
	}
	if r.DefaultLine == "" {
		return "main", nil
	}
	return r.DefaultLine, nil
}

// SetDefaultLine 修改默认 line（本轮工具层不暴露，供迁移/测试使用）。
func (d *DB) SetDefaultLine(name string) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE repo SET default_line=? WHERE id=1`, name)
		return err
	})
}

// --- 版本 DAG -----------------------------------------------------------

// ParentIDs 返回一个版本的全部父（按 ordinal）。
func (d *DB) ParentIDs(id hash.ID) ([]hash.ID, error) {
	rows, err := d.sql.Query(
		`SELECT parent_id FROM version_parent WHERE version_id=? ORDER BY ordinal`, id.String())
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
		p, err := hash.Parse(s)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if len(out) > 0 {
		return out, rows.Err()
	}
	v, err := d.GetVersion(id)
	if err != nil {
		return nil, err
	}
	if !v.Parent.IsZero() {
		out = append(out, v.Parent)
	}
	return out, rows.Err()
}

// AncestorDepths 返回从 head 出发所有可达版本及其最短距离。
func (d *DB) AncestorDepths(head hash.ID) (map[hash.ID]int, error) {
	out := map[hash.ID]int{}
	if head.IsZero() {
		return out, nil
	}
	type item struct {
		id    hash.ID
		depth int
	}
	q := []item{{head, 0}}
	for len(q) > 0 {
		it := q[0]
		q = q[1:]
		if old, ok := out[it.id]; ok && old <= it.depth {
			continue
		}
		out[it.id] = it.depth
		ps, err := d.ParentIDs(it.id)
		if err != nil {
			if errors.Is(err, ErrNotExist) {
				continue
			}
			return nil, err
		}
		for _, p := range ps {
			if p.IsZero() {
				continue
			}
			if _, ok := out[p]; !ok {
				q = append(q, item{p, it.depth + 1})
			}
		}
	}
	return out, nil
}

// Ancestors 返回 head 的所有祖先（含 head）。
func (d *DB) Ancestors(head hash.ID) (map[hash.ID]bool, error) {
	depths, err := d.AncestorDepths(head)
	if err != nil {
		return nil, err
	}
	out := make(map[hash.ID]bool, len(depths))
	for id := range depths {
		out[id] = true
	}
	return out, nil
}

// IsAncestor 报告 anc 是否为 desc 的祖先（含相等）。
func (d *DB) IsAncestor(anc, desc hash.ID) (bool, error) {
	if anc.IsZero() || desc.IsZero() {
		return false, nil
	}
	if anc == desc {
		return true, nil
	}
	seen := map[hash.ID]bool{}
	stack := []hash.ID{desc}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		ps, err := d.ParentIDs(cur)
		if err != nil {
			if errors.Is(err, ErrNotExist) {
				continue
			}
			return false, err
		}
		for _, p := range ps {
			if p == anc {
				return true, nil
			}
			if !seen[p] {
				stack = append(stack, p)
			}
		}
	}
	return false, nil
}

// MergeBase 计算 a、b 的最低公共祖先。多个 LCA 时返回 ErrMergeBaseAmbiguous。
func (d *DB) MergeBase(a, b hash.ID) (hash.ID, error) {
	base, all, err := d.MergeBases(a, b)
	if err != nil {
		return hash.ID{}, err
	}
	if len(all) > 1 {
		return hash.ID{}, ErrMergeBaseAmbiguous
	}
	return base, nil
}

// ErrMergeBaseAmbiguous 表示存在多个最低公共祖先。
var ErrMergeBaseAmbiguous = errors.New("meta: merge base 不唯一")

// MergeBases 返回一个确定的 base 与全部候选。
func (d *DB) MergeBases(a, b hash.ID) (hash.ID, []hash.ID, error) {
	if a.IsZero() || b.IsZero() {
		return hash.ID{}, nil, nil
	}
	as, err := d.Ancestors(a)
	if err != nil {
		return hash.ID{}, nil, err
	}
	bs, err := d.Ancestors(b)
	if err != nil {
		return hash.ID{}, nil, err
	}
	var common []hash.ID
	for id := range as {
		if bs[id] {
			common = append(common, id)
		}
	}
	if len(common) == 0 {
		return hash.ID{}, nil, nil
	}
	// 去掉「是另一个候选祖先」的候选，只留最低层。
	var lowest []hash.ID
	for _, c := range common {
		higher := false
		for _, o := range common {
			if c == o {
				continue
			}
			ok, err := d.IsAncestor(c, o)
			if err != nil {
				return hash.ID{}, nil, err
			}
			if ok {
				higher = true
				break
			}
		}
		if !higher {
			lowest = append(lowest, c)
		}
	}
	sort.Slice(lowest, func(i, j int) bool { return lowest[i].String() < lowest[j].String() })
	if len(lowest) == 0 {
		return hash.ID{}, nil, nil
	}
	return lowest[0], lowest, nil
}

// Reachable 返回从 roots 可达的全部版本。
func (d *DB) Reachable(roots []hash.ID) (map[hash.ID]bool, error) {
	out := map[hash.ID]bool{}
	for _, r := range roots {
		if r.IsZero() || out[r] {
			continue
		}
		anc, err := d.Ancestors(r)
		if err != nil {
			return nil, err
		}
		for id := range anc {
			out[id] = true
		}
	}
	return out, nil
}

// LogFrom 沿 head 的祖先图返回版本，按 (ts desc, id desc) 排序。
// beforeTS/beforeID 为游标（0/零表示从头）。
func (d *DB) LogFrom(head hash.ID, limit int, beforeTS int64, beforeID hash.ID) ([]VersionRow, error) {
	if head.IsZero() {
		return nil, nil
	}
	anc, err := d.Ancestors(head)
	if err != nil {
		return nil, err
	}
	rows, err := d.sql.Query(
		`SELECT id, parent, ts, agent, task_desc, epoch FROM version ORDER BY ts DESC, id DESC`)
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
		id, err := hash.Parse(idStr)
		if err != nil {
			return nil, err
		}
		if !anc[id] {
			continue
		}
		if beforeTS > 0 && (r.TS > beforeTS || (r.TS == beforeTS && !beforeID.IsZero() && id.String() >= beforeID.String())) {
			continue
		}
		if r.ID, err = hash.Parse(idStr); err != nil {
			return nil, err
		}
		if r.Parent, err = hash.Parse(parentStr); err != nil {
			return nil, err
		}
		r.Epoch = uint64(epoch)
		out = append(out, r)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

// --- 幂等 ---------------------------------------------------------------

// IdempotencyResult 是通用幂等结果。
type IdempotencyResult struct {
	Tool       string
	Key        string
	ResultJSON string
	CreatedTS  int64
	ExpiresTS  int64
}

// PutIdempotencyResult 写入/覆盖一条幂等结果。
func (d *DB) PutIdempotencyResult(tool, key, resultJSON string, ts, expiresTS int64) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO idempotency_result(tool, key, result_json, created_ts, expires_ts)
			 VALUES(?,?,?,?,?)
			 ON CONFLICT(tool,key) DO UPDATE SET result_json=excluded.result_json,
			   created_ts=excluded.created_ts, expires_ts=excluded.expires_ts`,
			tool, key, resultJSON, ts, expiresTS)
		return err
	})
}

// GetIdempotencyResult 读取一条未过期结果。
func (d *DB) GetIdempotencyResult(tool, key string, now int64) (*IdempotencyResult, error) {
	var r IdempotencyResult
	err := d.sql.QueryRow(
		`SELECT tool, key, result_json, created_ts, expires_ts
		   FROM idempotency_result WHERE tool=? AND key=?`, tool, key).
		Scan(&r.Tool, &r.Key, &r.ResultJSON, &r.CreatedTS, &r.ExpiresTS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if r.ExpiresTS > 0 && r.ExpiresTS < now {
		return nil, nil
	}
	return &r, nil
}

// FindVersionByIdempotency 按 tool/key 找版本。
func (d *DB) FindVersionByIdempotency(tool, key string, now int64) (*version.Version, error) {
	var idStr string
	var ts int64
	err := d.sql.QueryRow(
		`SELECT id, idempotency_ts FROM version
		  WHERE idempotency_tool=? AND idempotency_key=?
		  ORDER BY idempotency_ts DESC LIMIT 1`, tool, key).Scan(&idStr, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if ts > 0 && now-ts > int64(7*24*time.Hour/time.Millisecond) {
		return nil, nil
	}
	id, err := hash.Parse(idStr)
	if err != nil {
		return nil, err
	}
	return d.GetVersion(id)
}

// SetVersionIdempotency 把幂等键写到版本行。
func (d *DB) SetVersionIdempotency(id hash.ID, tool, key string, ts int64) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE version SET idempotency_key=?, idempotency_tool=?, idempotency_ts=?
			  WHERE id=?`, key, tool, ts, id.String())
		return err
	})
}

func nullableHash(h hash.ID) any {
	if h.IsZero() {
		return nil
	}
	return h.String()
}

// LineView 是 line 的 JSON 友好视图。
type LineView struct {
	Name        string  `json:"name"`
	Head        hash.ID `json:"head"`
	Protected   bool    `json:"protected"`
	CreatedFrom hash.ID `json:"created_from"`
	CreatedTS   int64   `json:"created_ts"`
	UpdatedTS   int64   `json:"updated_ts"`
}

// View 返回可序列化视图。
func (l *Line) View() LineView {
	return LineView{Name: l.Name, Head: l.Head, Protected: l.Protected,
		CreatedFrom: l.CreatedFrom, CreatedTS: l.CreatedTS, UpdatedTS: l.UpdatedTS}
}

// String 便于日志。
func (l *Line) String() string {
	return fmt.Sprintf("line(%s -> %s)", l.Name, l.Head.String()[:16])
}

// IdempotentVersionIDs 返回 idempotency_ts 不早于 since 的版本。
func (d *DB) IdempotentVersionIDs(since int64) ([]hash.ID, error) {
	rows, err := d.sql.Query(
		`SELECT id FROM version WHERE idempotency_ts IS NOT NULL AND idempotency_ts >= ?`, since)
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
		out = append(out, id)
	}
	return out, rows.Err()
}
