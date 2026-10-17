package meta

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/chenyiwen/p37/internal/cas"
	"github.com/chenyiwen/p37/internal/hash"
)

// ObjectIndex 是 cas.ObjectIndex 在元数据库上的适配器。
//
// object_index 表**只是缓存**：CAS 对象的存储形态自带类型，因此索引可以随时
// 由 cas.Store.RebuildIndex 重建（§8.3）。
type ObjectIndex struct{ db *DB }

// ObjectIndex 返回适配器。
func (d *DB) ObjectIndex() *ObjectIndex { return &ObjectIndex{db: d} }

// Has 实现 cas.ObjectIndex。
func (o *ObjectIndex) Has(id hash.ID) (bool, error) {
	var one int
	err := o.db.sql.QueryRow(`SELECT 1 FROM object_index WHERE id = ?`, id.String()).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// Type 实现 cas.ObjectIndex。
func (o *ObjectIndex) Type(id hash.ID) (uint8, error) {
	var typ int
	err := o.db.sql.QueryRow(`SELECT type FROM object_index WHERE id = ?`, id.String()).Scan(&typ)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, cas.ErrNotExist
	case err != nil:
		return 0, err
	}
	return uint8(typ), nil
}

// Add 实现 cas.ObjectIndex。
func (o *ObjectIndex) Add(id hash.ID, typ uint8, size, rawSize int64) error {
	return o.db.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO object_index(id, type, size, raw_size) VALUES(?,?,?,?)
			 ON CONFLICT(id) DO UPDATE SET type=excluded.type, size=excluded.size,
			                               raw_size=excluded.raw_size`,
			id.String(), int(typ), size, rawSize)
		return err
	})
}

// Remove 实现 cas.ObjectIndex。
func (o *ObjectIndex) Remove(id hash.ID) error {
	return o.db.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM object_index WHERE id = ?`, id.String())
		return err
	})
}

// Iterate 实现 cas.ObjectIndex，按 id 升序回调。
func (o *ObjectIndex) Iterate(fn func(id hash.ID, typ uint8, size, rawSize int64) error) error {
	rows, err := o.db.sql.Query(`SELECT id, type, size, raw_size FROM object_index ORDER BY id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			idStr         string
			typ           int
			size, rawSize int64
		)
		if err := rows.Scan(&idStr, &typ, &size, &rawSize); err != nil {
			return err
		}
		id, err := hash.Parse(idStr)
		if err != nil {
			return fmt.Errorf("meta: object_index 含非法 id %q: %w", idStr, err)
		}
		if err := fn(id, uint8(typ), size, rawSize); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Count 返回索引中的对象数。
func (o *ObjectIndex) Count() (int64, error) {
	var n int64
	err := o.db.sql.QueryRow(`SELECT COUNT(*) FROM object_index`).Scan(&n)
	return n, err
}

// Clear 清空索引（重建前使用）。
func (o *ObjectIndex) Clear() error {
	return o.db.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM object_index`)
		return err
	})
}

var _ cas.ObjectIndex = (*ObjectIndex)(nil)
