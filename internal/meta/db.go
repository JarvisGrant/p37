// Package meta 实现本地元数据库（SQLite，附录 J）。
//
// 元数据库是**缓存与索引层**：文件系统与 CAS 对象才是真相源。ops 表、
// stat_cache、object_index 都可以由真相源重建（§1.2、§5.4、§8.3）。
package meta

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 驱动，无需 cgo

	"github.com/chenyiwen/p37/internal/hash"
)

// FormatVer 是当前的元数据库格式版本（§2.2）。
//
// 任何改变 DDL 或对象编码的行为都必须升此值，并提供迁移函数与迁移测试。
const FormatVer = 2

// DB 是元数据库句柄。
//
// 并发模型（§11.1）：SQLite WAL 下多读单写。写操作经 wmu 串行化；读操作直接
// 走连接池。
type DB struct {
	sql  *sql.DB
	path string

	wmu    chan struct{} // 容量 1 的写锁
	closed atomic.Bool
}

// Open 打开（或创建）仓库的元数据库。
func Open(repoRoot string) (*DB, error) {
	path := filepath.Join(repoRoot, ".p37", "p37.db")
	return openPath(path)
}

func openPath(path string) (*DB, error) {
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(ON)"

	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("meta: 打开 %s: %w", path, err)
	}
	// 纯 Go 驱动在高并发下用单连接更稳；读并发由 WAL + 连接池提供。
	sdb.SetMaxOpenConns(4)
	sdb.SetMaxIdleConns(4)
	sdb.SetConnMaxLifetime(0)

	db := &DB{sql: sdb, path: path, wmu: make(chan struct{}, 1)}
	db.wmu <- struct{}{}

	if err := sdb.Ping(); err != nil {
		_ = sdb.Close()
		return nil, fmt.Errorf("meta: ping: %w", err)
	}

	// 权限收紧（A3）：元数据库含全部版本元数据与血缘，只允许属主读写。
	// WAL/SHM 是 SQLite 自己创建的，一并收紧。
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Chmod(path+suffix, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = sdb.Close()
			return nil, fmt.Errorf("meta: 收紧 %s 权限失败: %w", path+suffix, err)
		}
	}
	return db, nil
}

// Path 返回数据库文件路径。
func (d *DB) Path() string { return d.path }

// Close 关闭数据库。
func (d *DB) Close() error {
	if d.closed.Swap(true) {
		return nil
	}
	return d.sql.Close()
}

// SQL 返回底层句柄，供其他子包构造查询。
func (d *DB) SQL() *sql.DB { return d.sql }

// withWrite 串行化写事务。
func (d *DB) withWrite(fn func(tx *sql.Tx) error) error {
	<-d.wmu
	defer func() { d.wmu <- struct{}{} }()

	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Init 建表并把 repo 行初始化（幂等）。
func (d *DB) Init(root string) error {
	return d.withWrite(func(tx *sql.Tx) error {
		for _, stmt := range ddl {
			if _, err := tx.Exec(stmt); err != nil {
				return fmt.Errorf("meta: 建表失败: %w\n语句: %s", err, stmt)
			}
		}
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM repo`).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := tx.Exec(
				`INSERT INTO repo(id, root, created_ts, format_ver, version_count, epoch)
				 VALUES(1, ?, ?, ?, 0, 0)`,
				root, time.Now().UnixMilli(), FormatVer); err != nil {
				return err
			}
		}
		if err := ensureColumnsTx(tx); err != nil {
			return err
		}

		// 记录格式版本，供 daemon 启动时校验（§2.2）。
		_, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, FormatVer))
		return err
	})
}

// EnsureSchema 把已有仓库的库结构补到当前版本（幂等）。
//
// 与 Init 不同，它**不建表、不写 repo 行**，只补齐后来新增的列。repo.Open
// 会调用它，因此老仓库打开一次就自动升级，不需要额外执行 p37 migrate。
func (d *DB) EnsureSchema() error {
	return d.withWrite(ensureColumnsTx)
}

// ensureColumnsTx 幂等地补齐后续版本新增的列与表。
func ensureColumnsTx(tx *sql.Tx) error {
	add := func(table, col, decl string) error {
		var exists int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).
			Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return nil // 老库/裁剪库可能没有这张表；对应表由 Init/ddl 负责。
		}
		var has int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, col).
			Scan(&has); err != nil {
			return err
		}
		if has == 0 {
			_, err := tx.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, col, decl))
			return err
		}
		return nil
	}
	// v1 -> v2 列。
	for _, c := range []struct{ table, col, decl string }{
		{"repo", "default_line", "TEXT NOT NULL DEFAULT 'main'"},
		{"version", "idempotency_key", "TEXT"},
		{"version", "idempotency_tool", "TEXT"},
		{"version", "idempotency_ts", "INTEGER"},
		{"workspace", "materialized_ts", "INTEGER NOT NULL DEFAULT 0"},
		{"workspace", "base_line", "TEXT NOT NULL DEFAULT ''"},
		{"workspace", "hidden", "INTEGER NOT NULL DEFAULT 0 CHECK (hidden IN (0,1))"},
		{"workspace", "kind", "TEXT NOT NULL DEFAULT 'user'"},
		{"workspace", "landed_ts", "INTEGER NOT NULL DEFAULT 0"},
		{"landed_ws", "target_kind", "TEXT NOT NULL DEFAULT ''"},
		{"landed_ws", "target_id", "TEXT NOT NULL DEFAULT ''"},
		{"op", "mode_before", "INTEGER CHECK (mode_before IN (0,1))"},
		{"op", "mode_after", "INTEGER CHECK (mode_after IN (0,1))"},
	} {
		if err := add(c.table, c.col, c.decl); err != nil {
			return err
		}
	}
	// v2 新表。使用与 ddl 相同的定义（IF NOT EXISTS）。
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS line (
		  name          TEXT PRIMARY KEY,
		  head          TEXT NOT NULL,
		  protected     INTEGER NOT NULL DEFAULT 0 CHECK (protected IN (0,1)),
		  created_from  TEXT,
		  created_ts    INTEGER NOT NULL,
		  updated_ts    INTEGER NOT NULL,
		  deleted_ts    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_line_head ON line(head)`,
		`CREATE TABLE IF NOT EXISTS version_parent (
		  version_id TEXT NOT NULL,
		  parent_id  TEXT NOT NULL,
		  ordinal    INTEGER NOT NULL,
		  PRIMARY KEY (version_id, ordinal)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_version_parent_parent ON version_parent(parent_id)`,
		`CREATE TABLE IF NOT EXISTS idempotency_result (
		  tool        TEXT NOT NULL,
		  key         TEXT NOT NULL,
		  result_json TEXT NOT NULL,
		  created_ts  INTEGER NOT NULL,
		  expires_ts  INTEGER NOT NULL DEFAULT 0,
		  PRIMARY KEY (tool, key)
		)`,
		`CREATE TABLE IF NOT EXISTS line_reset_log (
		  id        INTEGER PRIMARY KEY AUTOINCREMENT,
		  line      TEXT NOT NULL,
		  from_head TEXT NOT NULL,
		  to_head   TEXT NOT NULL,
		  reason    TEXT NOT NULL DEFAULT '',
		  forced    INTEGER NOT NULL DEFAULT 0 CHECK (forced IN (0,1)),
		  ts        INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS land_into_journal (
		  id             INTEGER PRIMARY KEY AUTOINCREMENT,
		  ws_id          TEXT NOT NULL,
		  parent_ws_id   TEXT NOT NULL,
		  before_tree    TEXT NOT NULL,
		  result_tree    TEXT NOT NULL,
		  state          TEXT NOT NULL,
		  created_ts     INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS sync_state (
		  id              INTEGER PRIMARY KEY CHECK (id = 1),
		  remote_head     TEXT,
		  last_package_id TEXT,
		  last_sync_ts    INTEGER NOT NULL DEFAULT 0
		)`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	// 回填 version_parent（老仓库只有单父）。
	zero := strings.Repeat("0", hash.HexLen)
	var versionExists int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='version'`).Scan(&versionExists); err != nil {
		return err
	}
	if versionExists > 0 {
		if _, err := tx.Exec(
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_version_idem ON version(idempotency_tool, idempotency_key)
			   WHERE idempotency_key IS NOT NULL`); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO version_parent(version_id, parent_id, ordinal)
			 SELECT id, parent, 0 FROM version
			  WHERE parent IS NOT NULL AND parent != '' AND parent != ?`, zero); err != nil {
			return err
		}
	}
	// 回填 main line：以 mainline_head 为 head。没有 head 的仓库等 Init/首次 land。
	var repoExists int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='repo'`).Scan(&repoExists); err != nil {
		return err
	}
	if repoExists == 0 {
		return nil
	}
	var head sql.NullString
	if err := tx.QueryRow(`SELECT mainline_head FROM repo WHERE id = 1`).Scan(&head); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if head.Valid && head.String != "" && head.String != zero {
		if _, err := tx.Exec(
			`INSERT INTO line(name, head, protected, created_ts, updated_ts)
			 VALUES('main', ?, 1, ?, ?)
			 ON CONFLICT(name) DO NOTHING`,
			head.String, time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}

// UserVersion 返回库内的格式版本（PRAGMA user_version）。
func (d *DB) UserVersion() (int, error) {
	var v int
	err := d.sql.QueryRow(`PRAGMA user_version`).Scan(&v)
	return v, err
}

// CheckFormat 校验库格式版本与自身一致（§2.2）。
func (d *DB) CheckFormat() error {
	v, err := d.UserVersion()
	if err != nil {
		return err
	}
	switch {
	case v == FormatVer:
		return nil
	case v == 0:
		return errors.New("meta: 仓库尚未初始化")
	case v < FormatVer:
		return fmt.Errorf("meta: 仓库格式版本 %d 低于本程序支持的 %d，请运行 p37 migrate", v, FormatVer)
	default:
		return fmt.Errorf("meta: 仓库格式版本 %d 高于本程序支持的 %d，请升级 p37", v, FormatVer)
	}
}

// ddl 是附录 J 的完整建表语句。全部使用 IF NOT EXISTS，便于幂等执行。
var ddl = []string{
	`CREATE TABLE IF NOT EXISTS repo (
	  id            INTEGER PRIMARY KEY CHECK (id = 1),
	  root          TEXT    NOT NULL,
	  created_ts    INTEGER NOT NULL,
	  format_ver    INTEGER NOT NULL,
	  mainline_head TEXT,
	  default_line  TEXT    NOT NULL DEFAULT 'main',
	  version_count INTEGER NOT NULL DEFAULT 0,
	  epoch         INTEGER NOT NULL DEFAULT 0
	)`,

	`CREATE TABLE IF NOT EXISTS version (
	  id         TEXT PRIMARY KEY,
	  parent     TEXT,
	  tree       TEXT NOT NULL,
	  epoch      INTEGER NOT NULL,
	  agent      TEXT NOT NULL,
	  task_hash  TEXT NOT NULL,
	  task_desc  TEXT NOT NULL DEFAULT '',
	  ts         INTEGER NOT NULL,
	  msg        TEXT NOT NULL DEFAULT '',
	  ws_id      TEXT,
	  journal_id INTEGER,
	  idempotency_key  TEXT,
	  idempotency_tool TEXT,
	  idempotency_ts   INTEGER
	)`,
	`CREATE INDEX IF NOT EXISTS idx_version_parent ON version(parent)`,
	`CREATE INDEX IF NOT EXISTS idx_version_epoch  ON version(epoch)`,
	`CREATE INDEX IF NOT EXISTS idx_version_ts     ON version(ts)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_version_idem ON version(idempotency_tool, idempotency_key)
	   WHERE idempotency_key IS NOT NULL`,

	`CREATE TABLE IF NOT EXISTS workspace (
	  id           TEXT PRIMARY KEY,
	  name         TEXT NOT NULL DEFAULT '',
	  parent_kind  TEXT NOT NULL CHECK (parent_kind IN ('version','workspace')),
	  parent_id    TEXT NOT NULL,
	  base_tree    TEXT NOT NULL,
	  base_version TEXT NOT NULL,
	  task_desc    TEXT NOT NULL DEFAULT '',
	  state        TEXT NOT NULL CHECK (state IN
	                 ('active','rebasing','conflicted','integrating','broken')),
	  materialize  TEXT NOT NULL CHECK (materialize IN ('overlay','copy')),
	  mounted      INTEGER NOT NULL DEFAULT 0 CHECK (mounted IN (0,1)),
	  created_ts   INTEGER NOT NULL,
	  updated_ts   INTEGER NOT NULL,
	  materialized_ts INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_ws_parent ON workspace(parent_kind, parent_id)`,
	`CREATE INDEX IF NOT EXISTS idx_ws_state  ON workspace(state)`,

	`CREATE TABLE IF NOT EXISTS ws_effective_tree (
	  ws_id     TEXT PRIMARY KEY REFERENCES workspace(id) ON DELETE CASCADE,
	  ops_epoch INTEGER NOT NULL,
	  tree      TEXT NOT NULL
	)`,

	`CREATE TABLE IF NOT EXISTS landed_ws (
	  ws_id       TEXT PRIMARY KEY,
	  version_id  TEXT NOT NULL,
	  target_kind TEXT NOT NULL DEFAULT '',
	  target_id   TEXT NOT NULL DEFAULT '',
	  ts          INTEGER NOT NULL
	)`,

	`CREATE TABLE IF NOT EXISTS lease (
	  id         INTEGER PRIMARY KEY AUTOINCREMENT,
	  ws_id      TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
	  path       TEXT NOT NULL,
	  mode       TEXT NOT NULL CHECK (mode IN ('shared','exclusive')),
	  created_ts INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_lease_ws   ON lease(ws_id)`,
	`CREATE INDEX IF NOT EXISTS idx_lease_path ON lease(path)`,

	`CREATE TABLE IF NOT EXISTS op (
	  id          TEXT PRIMARY KEY,
	  ws_id       TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
	  seq         INTEGER NOT NULL,
	  kind        TEXT NOT NULL CHECK (kind IN
	                ('create','modify','delete','rename','attr','blob_put')),
	  path        TEXT NOT NULL,
	  hunks_json  TEXT NOT NULL DEFAULT '[]',
	  attr_json   TEXT,
	  eol         INTEGER CHECK (eol IN (0,1,2)),
	  dest_path   TEXT,
	  entry_kind  INTEGER NOT NULL DEFAULT 0 CHECK (entry_kind IN (0,2)),
	  mode_before INTEGER CHECK (mode_before IN (0,1)),
	  mode_after  INTEGER CHECK (mode_after  IN (0,1)),
	  blob_before TEXT,
	  blob_after  TEXT,
	  captured_ts INTEGER NOT NULL,
	  captured_by TEXT NOT NULL,
	  derived     INTEGER NOT NULL DEFAULT 1 CHECK (derived IN (0,1))
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_op_ws_seq  ON op(ws_id, seq)`,
	`CREATE INDEX        IF NOT EXISTS idx_op_ws_path ON op(ws_id, path)`,

	`CREATE TABLE IF NOT EXISTS stat_cache (
	  ws_id        TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
	  path         TEXT NOT NULL,
	  kind         INTEGER NOT NULL CHECK (kind IN (0,1,2)),
	  mtime_ns     INTEGER NOT NULL,
	  size         INTEGER NOT NULL,
	  inode        INTEGER NOT NULL,
	  mode         INTEGER NOT NULL CHECK (mode IN (0,1)),
	  content_hash TEXT,
	  object_id    TEXT,
	  entries_sig  TEXT,
	  PRIMARY KEY (ws_id, path)
	)`,

	`CREATE TABLE IF NOT EXISTS conflict (
	  key             TEXT PRIMARY KEY,
	  ws_id           TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
	  op_id           TEXT NOT NULL,
	  hunk_index      INTEGER NOT NULL,
	  path            TEXT NOT NULL,
	  kind            TEXT NOT NULL CHECK (kind IN
	                    ('overlapping_edit','delete_vs_edit','rename_vs_edit',
	                     'binary','missing_base','attr_conflict')),
	  state           TEXT NOT NULL CHECK (state IN ('open','resolved','stale')),
	  base_json       TEXT NOT NULL,
	  lower_json      TEXT NOT NULL,
	  upper_json      TEXT NOT NULL,
	  candidates_json TEXT NOT NULL DEFAULT '[]',
	  created_ts      INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_conflict_ws ON conflict(ws_id, state)`,

	`CREATE TABLE IF NOT EXISTS resolution (
	  key          TEXT PRIMARY KEY,
	  ws_id        TEXT NOT NULL,
	  path         TEXT NOT NULL,
	  lower_hash   TEXT NOT NULL,
	  upper_hash   TEXT NOT NULL,
	  kind         TEXT NOT NULL CHECK (kind IN
	                 ('take_lower','take_upper','both','custom')),
	  payload_json TEXT NOT NULL DEFAULT '{}',
	  created_ts   INTEGER NOT NULL,
	  created_by   TEXT NOT NULL
	)`,

	`CREATE TABLE IF NOT EXISTS journal (
	  id         INTEGER PRIMARY KEY AUTOINCREMENT,
	  version_id TEXT NOT NULL,
	  ws_id      TEXT NOT NULL,
	  agent      TEXT NOT NULL,
	  task_desc  TEXT NOT NULL,
	  ts         INTEGER NOT NULL,
	  ops_json   TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_journal_version ON journal(version_id)`,

	`CREATE TABLE IF NOT EXISTS blame_cache (
	  version_id   TEXT NOT NULL,
	  path         TEXT NOT NULL,
	  mapping_json TEXT NOT NULL,
	  created_ts   INTEGER NOT NULL,
	  PRIMARY KEY (version_id, path)
	)`,

	`CREATE TABLE IF NOT EXISTS package (
	  id             TEXT PRIMARY KEY,
	  kind           TEXT NOT NULL CHECK (kind IN ('checkpoint','delta')),
	  base_version   TEXT,
	  result_version TEXT NOT NULL,
	  epoch          INTEGER NOT NULL,
	  created_ts     INTEGER NOT NULL,
	  object_count   INTEGER NOT NULL,
	  byte_size      INTEGER NOT NULL,
	  local_path     TEXT,
	  uploaded       INTEGER NOT NULL DEFAULT 0 CHECK (uploaded IN (0,1))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_package_epoch ON package(epoch)`,

	`CREATE TABLE IF NOT EXISTS sync_state (
	  id              INTEGER PRIMARY KEY CHECK (id = 1),
	  remote_head     TEXT,
	  last_package_id TEXT,
	  last_sync_ts    INTEGER NOT NULL DEFAULT 0
	)`,

	`CREATE TABLE IF NOT EXISTS line (
	  name          TEXT PRIMARY KEY,
	  head          TEXT NOT NULL,
	  protected     INTEGER NOT NULL DEFAULT 0 CHECK (protected IN (0,1)),
	  created_from  TEXT,
	  created_ts    INTEGER NOT NULL,
	  updated_ts    INTEGER NOT NULL,
	  deleted_ts    INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_line_head ON line(head)`,

	`CREATE TABLE IF NOT EXISTS version_parent (
	  version_id TEXT NOT NULL,
	  parent_id  TEXT NOT NULL,
	  ordinal    INTEGER NOT NULL,
	  PRIMARY KEY (version_id, ordinal)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_version_parent_parent ON version_parent(parent_id)`,

	`CREATE TABLE IF NOT EXISTS idempotency_result (
	  tool        TEXT NOT NULL,
	  key         TEXT NOT NULL,
	  result_json TEXT NOT NULL,
	  created_ts  INTEGER NOT NULL,
	  expires_ts  INTEGER NOT NULL DEFAULT 0,
	  PRIMARY KEY (tool, key)
	)`,

	`CREATE TABLE IF NOT EXISTS line_reset_log (
	  id        INTEGER PRIMARY KEY AUTOINCREMENT,
	  line      TEXT NOT NULL,
	  from_head TEXT NOT NULL,
	  to_head   TEXT NOT NULL,
	  reason    TEXT NOT NULL DEFAULT '',
	  forced    INTEGER NOT NULL DEFAULT 0 CHECK (forced IN (0,1)),
	  ts        INTEGER NOT NULL
	)`,

	`CREATE TABLE IF NOT EXISTS land_into_journal (
	  id             INTEGER PRIMARY KEY AUTOINCREMENT,
	  ws_id          TEXT NOT NULL,
	  parent_ws_id   TEXT NOT NULL,
	  before_tree    TEXT NOT NULL,
	  result_tree    TEXT NOT NULL,
	  state          TEXT NOT NULL,
	  created_ts     INTEGER NOT NULL
	)`,

	`CREATE TABLE IF NOT EXISTS object_index (
	  id       TEXT PRIMARY KEY,
	  type     INTEGER NOT NULL CHECK (type IN (1,2,3,4)),
	  size     INTEGER NOT NULL,
	  raw_size INTEGER NOT NULL
	)`,
}
