package meta

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyiwen/p37/internal/cas"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/version"
)

func newTestDB(t *testing.T) (*DB, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".p37"), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Init(root); err != nil {
		t.Fatal(err)
	}
	return db, root
}

// TestDDLExecutes 对应 §附录 L.1 #10：DDL 在空库上执行成功且格式版本正确。
func TestDDLExecutes(t *testing.T) {
	db, root := newTestDB(t)

	if err := db.CheckFormat(); err != nil {
		t.Fatalf("CheckFormat: %v", err)
	}
	v, err := db.UserVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != FormatVer {
		t.Fatalf("user_version = %d，期望 %d", v, FormatVer)
	}

	r, err := db.GetRepo()
	if err != nil {
		t.Fatal(err)
	}
	if r.Root != root {
		t.Fatalf("repo.root = %q，期望 %q", r.Root, root)
	}
	if !r.MainlineHead.IsZero() {
		t.Fatal("新仓库不应有 head")
	}

	// 所有表都必须存在。
	for _, tbl := range []string{
		"repo", "version", "workspace", "ws_effective_tree", "landed_ws", "lease",
		"op", "stat_cache", "conflict", "resolution", "journal", "blame_cache",
		"package", "object_index",
	} {
		var n int
		err := db.SQL().QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&n)
		if err != nil || n != 1 {
			t.Fatalf("表 %s 不存在 (n=%d, err=%v)", tbl, n, err)
		}
	}
}

// TestInitIdempotent 断言重复 Init 不会重复插入 repo 行。
func TestInitIdempotent(t *testing.T) {
	db, root := newTestDB(t)
	if err := db.Init(root); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM repo`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("repo 行数 = %d，期望 1", n)
	}
}

// TestFormatVersionChecks 覆盖 §2.2 的启动校验分支。
func TestFormatVersionChecks(t *testing.T) {
	db, _ := newTestDB(t)

	if _, err := db.SQL().Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	if err := db.CheckFormat(); err == nil {
		t.Fatal("user_version=0 应报未初始化")
	}

	if _, err := db.SQL().Exec(`PRAGMA user_version = ` + itoa(FormatVer+1)); err != nil {
		t.Fatal(err)
	}
	err := db.CheckFormat()
	if err == nil {
		t.Fatal("更高格式版本应拒绝启动")
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

// TestVersionRoundTrip 覆盖 version 的写入与读回，并校验 id 一致性。
func TestVersionRoundTrip(t *testing.T) {
	db, _ := newTestDB(t)

	treeID := hash.Sum([]byte("tree"))
	v := version.New(hash.Zero, treeID, 0, "agent-1", "实现登录接口", 1700000000000, "init")
	if err := db.InsertVersion(v); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetVersion(v.ID())
	if err != nil {
		t.Fatal(err)
	}
	if got.Tree != treeID || got.Agent != "agent-1" {
		t.Fatalf("读回不符: %+v", got)
	}
	if got.TaskDesc != "实现登录接口" {
		t.Fatalf("task_desc 未保存: %q", got.TaskDesc)
	}
	if got.ID() != v.ID() {
		t.Fatal("id 不稳定")
	}

	// 首版本判定。
	if !got.IsFirst() {
		t.Fatal("parent 为全零时应为首版本")
	}

	// 不存在的版本。
	if _, err := db.GetVersion(hash.Sum([]byte("nope"))); !errors.Is(err, ErrNotExist) {
		t.Fatalf("期望 ErrNotExist，得到 %v", err)
	}
}

// TestHeadAndCounts 覆盖 head 与计数的更新。
func TestHeadAndCounts(t *testing.T) {
	db, _ := newTestDB(t)

	if _, ok, err := db.Head(); err != nil || ok {
		t.Fatalf("新仓库不应有 head: ok=%v err=%v", ok, err)
	}

	id := hash.Sum([]byte("v1"))
	if err := db.SetMainlineHead(id, 1, 0); err != nil {
		t.Fatal(err)
	}
	got, ok, err := db.Head()
	if err != nil || !ok || got != id {
		t.Fatalf("head 读取失败: %v %v %v", got, ok, err)
	}
	r, _ := db.GetRepo()
	if r.VersionCount != 1 {
		t.Fatalf("version_count = %d", r.VersionCount)
	}
}

// TestListVersions 覆盖 version_log 的分页基础。
func TestListVersions(t *testing.T) {
	db, _ := newTestDB(t)

	parent := hash.Zero
	for i := 0; i < 5; i++ {
		v := version.New(parent, hash.Sum([]byte{byte(i)}), 0, "a", "t", int64(1000+i), "")
		if err := db.InsertVersion(v); err != nil {
			t.Fatal(err)
		}
		parent = v.ID()
	}
	rows, err := db.ListVersions(3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("limit=3 返回 %d 行", len(rows))
	}
	// 倒序：最新的在最前。
	if rows[0].TS != 1004 || rows[1].TS != 1003 {
		t.Fatalf("未按 ts 倒序: %+v", rows)
	}
}

// TestObjectIndexAdapter 断言 meta 的适配器能与 cas.Store 组合工作。
func TestObjectIndexAdapter(t *testing.T) {
	db, root := newTestDB(t)

	bs, err := cas.NewLocalFS(filepath.Join(root, ".p37"))
	if err != nil {
		t.Fatal(err)
	}
	store := cas.NewStore(bs, db.ObjectIndex())

	id, err := store.PutBlob([]byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	ok, err := db.ObjectIndex().Has(id)
	if err != nil || !ok {
		t.Fatalf("索引未登记: ok=%v err=%v", ok, err)
	}
	got, err := store.GetBlob(id)
	if err != nil || string(got) != "payload" {
		t.Fatalf("读回失败: %q %v", got, err)
	}
	n, err := db.ObjectIndex().Count()
	if err != nil || n != 1 {
		t.Fatalf("Count = %d, %v", n, err)
	}

	// 清空索引后应由 RebuildIndex 重建（索引是纯缓存）。
	if err := db.ObjectIndex().Clear(); err != nil {
		t.Fatal(err)
	}
	if n, err := store.RebuildIndex(); err != nil || n != 1 {
		t.Fatalf("RebuildIndex = (%d, %v)", n, err)
	}
	if ok, _ := db.ObjectIndex().Has(id); !ok {
		t.Fatal("重建后索引仍缺少对象")
	}
}

// TestForeignKeyCascade 断言 worksapce 删除会级联清理 op / lease。
func TestForeignKeyCascade(t *testing.T) {
	db, _ := newTestDB(t)

	_, err := db.SQL().Exec(
		`INSERT INTO workspace(id, name, parent_kind, parent_id, base_tree, base_version,
		                       task_desc, state, materialize, mounted, created_ts, updated_ts)
		 VALUES('ws_1','', 'version', ?, ?, ?, '', 'active', 'copy', 0, 1, 1)`,
		hash.Zero.String(), hash.Sum([]byte("t")).String(), hash.Sum([]byte("v")).String())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.SQL().Exec(
		`INSERT INTO op(id, ws_id, seq, kind, path, captured_ts, captured_by)
		 VALUES('op_1','ws_1', 1, 'create', 'a.txt', 1, 'test')`)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db.SQL().Exec(`DELETE FROM workspace WHERE id='ws_1'`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM op WHERE ws_id='ws_1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("级联删除失败：op 还剩 %d 行", n)
	}
}
