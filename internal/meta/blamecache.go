package meta

import (
	"database/sql"
	"errors"
)

// --- blame_cache（§12.2、附录 J） ----------------------------------------

// PutBlameCache 写入一个 (version, path) 的行归属映射。
//
// 覆盖写：同一 (version, path) 的结果是确定性的，重算结果必然相同，
// 因此不需要「不可变」语义（这一点与 resolution 不同）。
func (d *DB) PutBlameCache(versionID, path, mappingJSON string, ts int64) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO blame_cache(version_id, path, mapping_json, created_ts)
			 VALUES(?,?,?,?)
			 ON CONFLICT(version_id, path) DO UPDATE SET
			   mapping_json = excluded.mapping_json,
			   created_ts   = excluded.created_ts`,
			versionID, path, mappingJSON, ts)
		return err
	})
}

// GetBlameCache 读取缓存；未命中返回 ErrNotExist。
func (d *DB) GetBlameCache(versionID, path string) (string, error) {
	var m string
	err := d.sql.QueryRow(
		`SELECT mapping_json FROM blame_cache WHERE version_id = ? AND path = ?`,
		versionID, path).Scan(&m)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotExist
	}
	return m, err
}

// DeleteBlameCacheForVersion 删除某版本的全部 blame 缓存。
//
// 供 GC 使用（§12.2「version_id 被 GC 回收时，连带删除对应的 blame_cache 行」）。
func (d *DB) DeleteBlameCacheForVersion(versionID string) (int64, error) {
	var n int64
	err := d.withWrite(func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM blame_cache WHERE version_id = ?`, versionID)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}
