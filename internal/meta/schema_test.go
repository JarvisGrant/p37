package meta

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEnsureSchemaAddsLaterColumns 回归：老仓库（缺后续新增列）在打开时必须
// 自动升级，否则插入 op 会报 "table op has no column named mode_before"。
func TestEnsureSchemaAddsLaterColumns(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".p37"), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	// 造一个「老版本」的表结构：workspace 没有 materialized_ts，op 没有权限位。
	stmts := []string{
		`CREATE TABLE workspace (id TEXT PRIMARY KEY)`,
		`CREATE TABLE op (id TEXT PRIMARY KEY, ws_id TEXT NOT NULL, seq INTEGER NOT NULL,
			kind TEXT NOT NULL, path TEXT NOT NULL, hunks_json TEXT NOT NULL DEFAULT '[]',
			attr_json TEXT, eol INTEGER, dest_path TEXT,
			entry_kind INTEGER NOT NULL DEFAULT 0, blob_before TEXT, blob_after TEXT,
			captured_ts INTEGER NOT NULL, captured_by TEXT NOT NULL,
			derived INTEGER NOT NULL DEFAULT 1)`,
	}
	for _, q := range stmts {
		if _, err := db.SQL().Exec(q); err != nil {
			t.Fatal(err)
		}
	}

	// 连续调用两次：第二次必须是幂等的空操作。
	for i := 0; i < 2; i++ {
		if err := db.EnsureSchema(); err != nil {
			t.Fatalf("EnsureSchema 第 %d 次失败: %v", i+1, err)
		}
	}

	for _, tc := range []struct{ table, col string }{
		{"workspace", "materialized_ts"},
		{"op", "mode_before"},
		{"op", "mode_after"},
	} {
		var n int
		if err := db.SQL().QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, tc.table, tc.col).
			Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("%s.%s 未补齐", tc.table, tc.col)
		}
	}
}
