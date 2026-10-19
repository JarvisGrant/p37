package meta

import (
	"database/sql"
	"errors"
	"time"
)

// PackageRow 是 `package` 表的一行（§9 的同步增量包）。
//
// package 是**本地暂存**的同步单元：生成时只写 local_path，uploaded=0；
// 上传成功后置 1。包内容与远端布局无关，见 internal/remotesync。
type PackageRow struct {
	ID            string
	Kind          string // checkpoint | delta
	BaseVersion   string
	ResultVersion string
	Epoch         uint64
	CreatedTS     int64
	ObjectCount   int
	ByteSize      int64
	LocalPath     string
	Uploaded      bool
}

// InsertPackage 写入一条包记录（同 id 覆盖）。
func (d *DB) InsertPackage(p PackageRow) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO package(id, kind, base_version, result_version, epoch,
			                     created_ts, object_count, byte_size, local_path, uploaded)
			 VALUES(?,?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(id) DO UPDATE SET
			   kind=excluded.kind, base_version=excluded.base_version,
			   result_version=excluded.result_version, epoch=excluded.epoch,
			   created_ts=excluded.created_ts, object_count=excluded.object_count,
			   byte_size=excluded.byte_size, local_path=excluded.local_path,
			   uploaded=excluded.uploaded`,
			p.ID, p.Kind, nullable(p.BaseVersion), p.ResultVersion, int64(p.Epoch),
			p.CreatedTS, p.ObjectCount, p.ByteSize, nullable(p.LocalPath), boolToInt(p.Uploaded))
		return err
	})
}

// GetPackage 按 id 取一条包记录。
func (d *DB) GetPackage(id string) (*PackageRow, error) {
	row := d.sql.QueryRow(
		`SELECT id, kind, base_version, result_version, epoch, created_ts,
		        object_count, byte_size, local_path, uploaded
		   FROM package WHERE id = ?`, id)
	return scanPackage(row)
}

// ListPackages 按创建时间升序返回全部包记录。
func (d *DB) ListPackages() ([]PackageRow, error) {
	rows, err := d.sql.Query(
		`SELECT id, kind, base_version, result_version, epoch, created_ts,
		        object_count, byte_size, local_path, uploaded
		   FROM package ORDER BY created_ts, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []PackageRow
	for rows.Next() {
		p, err := scanPackage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// LastPackage 返回创建时间最新的一条包记录；没有则 ErrNotExist。
func (d *DB) LastPackage() (*PackageRow, error) {
	row := d.sql.QueryRow(
		`SELECT id, kind, base_version, result_version, epoch, created_ts,
		        object_count, byte_size, local_path, uploaded
		   FROM package ORDER BY created_ts DESC, id DESC LIMIT 1`)
	p, err := scanPackage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotExist
	}
	return p, err
}

// CountPendingPackages 返回尚未上传的包数。
func (d *DB) CountPendingPackages() (int64, error) {
	var n int64
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM package WHERE uploaded = 0`).Scan(&n)
	return n, err
}

// MarkPackageUploaded 标记包已上传。
func (d *DB) MarkPackageUploaded(id string) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE package SET uploaded = 1 WHERE id = ?`, id)
		return err
	})
}

// DeletePackage 删除一条包记录。
func (d *DB) DeletePackage(id string) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM package WHERE id = ?`, id)
		return err
	})
}

// SyncState 是远端同步的本地状态（单行）。
type SyncState struct {
	RemoteHead    string
	LastPackageID string
	LastSyncTS    int64
}

// GetSyncState 返回同步状态；从未同步过时返回零值。
func (d *DB) GetSyncState() (SyncState, error) {
	var (
		head   sql.NullString
		pkg    sql.NullString
		lastTS sql.NullInt64
	)
	err := d.sql.QueryRow(
		`SELECT remote_head, last_package_id, last_sync_ts FROM sync_state WHERE id = 1`).
		Scan(&head, &pkg, &lastTS)
	if errors.Is(err, sql.ErrNoRows) {
		return SyncState{}, nil
	}
	if err != nil {
		return SyncState{}, err
	}
	return SyncState{
		RemoteHead:    head.String,
		LastPackageID: pkg.String,
		LastSyncTS:    lastTS.Int64,
	}, nil
}

// SetSyncState 覆盖写入同步状态。
func (d *DB) SetSyncState(s SyncState) error {
	return d.withWrite(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO sync_state(id, remote_head, last_package_id, last_sync_ts)
			 VALUES(1,?,?,?)
			 ON CONFLICT(id) DO UPDATE SET
			   remote_head=excluded.remote_head,
			   last_package_id=excluded.last_package_id,
			   last_sync_ts=excluded.last_sync_ts`,
			nullable(s.RemoteHead), nullable(s.LastPackageID), s.LastSyncTS)
		return err
	})
}

// SetRemoteHead 只更新远端 head 与同步时间。
func (d *DB) SetRemoteHead(head string, ts int64) error {
	st, err := d.GetSyncState()
	if err != nil {
		return err
	}
	st.RemoteHead = head
	st.LastSyncTS = ts
	return d.SetSyncState(st)
}

func scanPackage(sc interface{ Scan(...any) error }) (*PackageRow, error) {
	var (
		p      PackageRow
		base   sql.NullString
		local  sql.NullString
		epoch  sql.NullInt64
		upload int
	)
	if err := sc.Scan(&p.ID, &p.Kind, &base, &p.ResultVersion, &epoch, &p.CreatedTS,
		&p.ObjectCount, &p.ByteSize, &local, &upload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotExist
		}
		return nil, err
	}
	p.BaseVersion = base.String
	p.LocalPath = local.String
	p.Epoch = uint64(epoch.Int64)
	p.Uploaded = upload != 0
	return &p, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// NowMS 返回当前毫秒时间戳（供同步层复用）。
func NowMS() int64 { return time.Now().UnixMilli() }
