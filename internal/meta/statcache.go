package meta

import (
	"database/sql"
	"strings"

	"github.com/chenyiwen/p37/internal/capture"
	"github.com/chenyiwen/p37/internal/hash"
)

// StatCache 是 capture.StatCache 在元数据库上的实现（§4.7、§5.5、附录 J）。
//
// stat_cache 与 ops 表一样是**可重建的缓存**：删掉整张表后，下一次采集靠重新
// 读取与哈希即可恢复（代价是那一轮变慢）。
type StatCache struct{ db *DB }

// StatCache 返回适配器。
func (d *DB) StatCache() *StatCache { return &StatCache{db: d} }

// Get 实现 capture.StatCache。
func (s *StatCache) Get(wsID, path string) (capture.StatEntry, bool) {
	var (
		e          capture.StatEntry
		kind, mode int
		contentStr sql.NullString
		objectStr  sql.NullString
		entriesStr sql.NullString
	)
	err := s.db.sql.QueryRow(
		`SELECT kind, mtime_ns, size, inode, mode, content_hash, object_id, entries_sig
		   FROM stat_cache WHERE ws_id = ? AND path = ?`, wsID, path).
		Scan(&kind, &e.MtimeNS, &e.Size, &e.Inode, &mode,
			&contentStr, &objectStr, &entriesStr)
	if err != nil {
		return capture.StatEntry{}, false
	}
	e.Path = path
	e.Kind = uint8(kind)
	e.Mode = uint64(mode)
	e.ContentHash = parseHashOrZero(contentStr)
	e.ObjectID = parseHashOrZero(objectStr)
	e.EntriesSig = parseHashOrZero(entriesStr)
	return e, true
}

// Put 实现 capture.StatCache。
func (s *StatCache) Put(wsID string, e capture.StatEntry) {
	// 缓存写失败不影响正确性，因此忽略错误（下一轮会重算）。
	_ = s.db.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO stat_cache(ws_id, path, kind, mtime_ns, size, inode, mode,
			                        content_hash, object_id, entries_sig)
			 VALUES(?,?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(ws_id, path) DO UPDATE SET
			   kind=excluded.kind, mtime_ns=excluded.mtime_ns, size=excluded.size,
			   inode=excluded.inode, mode=excluded.mode,
			   content_hash=excluded.content_hash, object_id=excluded.object_id,
			   entries_sig=excluded.entries_sig`,
			wsID, e.Path, int(e.Kind), e.MtimeNS, e.Size, e.Inode, int(e.Mode),
			hashOrNil(e.ContentHash), hashOrNil(e.ObjectID), hashOrNil(e.EntriesSig))
		return err
	})
}

// PutMany 实现 capture.StatCache：**一个事务**写完一批。
//
// 逐条 Put 会产生 O(文件数) 个事务，是 ws_open / ws_sync 慢的主因。
func (s *StatCache) PutMany(wsID string, es []capture.StatEntry) error {
	if len(es) == 0 {
		return nil
	}
	return s.db.withWrite(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(
			`INSERT INTO stat_cache(ws_id, path, kind, mtime_ns, size, inode, mode,
			                        content_hash, object_id, entries_sig)
			 VALUES(?,?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(ws_id, path) DO UPDATE SET
			   kind=excluded.kind, mtime_ns=excluded.mtime_ns, size=excluded.size,
			   inode=excluded.inode, mode=excluded.mode,
			   content_hash=excluded.content_hash, object_id=excluded.object_id,
			   entries_sig=excluded.entries_sig`)
		if err != nil {
			return err
		}
		defer func() { _ = stmt.Close() }()
		for _, e := range es {
			if _, err := stmt.Exec(wsID, e.Path, int(e.Kind), e.MtimeNS, e.Size,
				e.Inode, int(e.Mode), hashOrNil(e.ContentHash),
				hashOrNil(e.ObjectID), hashOrNil(e.EntriesSig)); err != nil {
				return err
			}
		}
		return nil
	})
}

// Load 实现 capture.StatCache：一次查询取出该工作区的全部条目。
func (s *StatCache) Load(wsID string) (map[string]capture.StatEntry, error) {
	rows, err := s.db.sql.Query(
		`SELECT path, kind, mtime_ns, size, inode, mode,
		        content_hash, object_id, entries_sig
		   FROM stat_cache WHERE ws_id = ?`, wsID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]capture.StatEntry, 1024)
	for rows.Next() {
		var (
			e          capture.StatEntry
			kind, mode int
			contentStr sql.NullString
			objectStr  sql.NullString
			entriesStr sql.NullString
		)
		if err := rows.Scan(&e.Path, &kind, &e.MtimeNS, &e.Size, &e.Inode, &mode,
			&contentStr, &objectStr, &entriesStr); err != nil {
			return nil, err
		}
		e.Kind = uint8(kind)
		e.Mode = uint64(mode)
		e.ContentHash = parseHashOrZero(contentStr)
		e.ObjectID = parseHashOrZero(objectStr)
		e.EntriesSig = parseHashOrZero(entriesStr)
		out[e.Path] = e
	}
	return out, rows.Err()
}

// DeleteUnder 实现 capture.StatCache。
func (s *StatCache) DeleteUnder(wsID, prefix string) {
	_ = s.db.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`DELETE FROM stat_cache WHERE ws_id = ? AND (path = ? OR path LIKE ? ESCAPE '\')`,
			wsID, prefix, escapeLike(prefix)+"/%")
		return err
	})
}

// Clear 清空某个工作区的全部 stat 缓存，返回删除行数。
//
// 用途：rebase 会**重写**工作区内容，而缓存里记的是重写之前的 stat 与对象 id。
// 留着它不仅会拖慢下一次采集（都要重新比对），更危险的是「mtime 与 size 恰好
// 都没变」的路径会被快路径误判为未变，直接返回旧对象 id。
func (s *StatCache) Clear(wsID string) (int64, error) {
	var n int64
	err := s.db.withWrite(func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM stat_cache WHERE ws_id = ?`, wsID)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// Count 返回某工作区的缓存条目数。
func (s *StatCache) Count(wsID string) (int64, error) {
	var n int64
	err := s.db.sql.QueryRow(`SELECT COUNT(*) FROM stat_cache WHERE ws_id = ?`, wsID).Scan(&n)
	return n, err
}

// CountAll 返回全表条目数（用于容量观测，§附录 J.2）。
func (s *StatCache) CountAll() (int64, error) {
	var n int64
	err := s.db.sql.QueryRow(`SELECT COUNT(*) FROM stat_cache`).Scan(&n)
	return n, err
}

func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	return strings.ReplaceAll(s, `_`, `\_`)
}

func parseHashOrZero(ns sql.NullString) hash.ID {
	if !ns.Valid || ns.String == "" {
		return hash.ID{}
	}
	id, err := hash.Parse(ns.String)
	if err != nil {
		return hash.ID{}
	}
	return id
}

func hashOrNil(id hash.ID) any {
	if id.IsZero() {
		return nil
	}
	return id.String()
}

// modeOrNil 把可选的权限位转成 SQL 值（nil 保持 NULL = 该 op 不携带此信息）。
func modeOrNil(m *uint64) any {
	if m == nil {
		return nil
	}
	return int64(*m)
}

var _ capture.StatCache = (*StatCache)(nil)

// ClearAll 清空全部 stat 缓存并返回删除行数。
//
// 为什么需要它：GC 会扫掉「只被缓存引用」的对象（例如采集过程中产生的中间
// Tree 对象 —— 它们不属于任何版本的树，只出现在 stat_cache.object_id 里）。
// 缓存留着悬空 id 会让下一次采集直接返回一个不存在的对象 id，树就烂了。
// stat_cache 本来就是**纯缓存**，清掉即可，下一轮采集会重建。
func (s *StatCache) ClearAll() (int64, error) {
	var n int64
	err := s.db.withWrite(func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM stat_cache`)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}
