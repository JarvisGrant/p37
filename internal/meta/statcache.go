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

// DeleteUnder 实现 capture.StatCache。
func (s *StatCache) DeleteUnder(wsID, prefix string) {
	_ = s.db.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`DELETE FROM stat_cache WHERE ws_id = ? AND (path = ? OR path LIKE ? ESCAPE '\')`,
			wsID, prefix, escapeLike(prefix)+"/%")
		return err
	})
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

var _ capture.StatCache = (*StatCache)(nil)
