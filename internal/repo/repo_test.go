package repo_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/op"
	"github.com/chenyiwen/p37/internal/repo"
	"github.com/chenyiwen/p37/internal/workspace"
)

type env struct {
	t    *testing.T
	root string
	src  string
	r    *repo.Repo
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	src := t.TempDir()
	r, err := repo.Init(root, repo.Options{Materialize: mountmgr.ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return &env{t: t, root: root, src: src, r: r}
}

func (e *env) seed(rel, content string) {
	e.t.Helper()
	p := filepath.Join(e.src, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) open(parent, task string) *meta.Workspace {
	e.t.Helper()
	w, err := e.r.WS.Open(workspace.OpenRequest{Parent: parent, TaskDesc: task})
	if err != nil {
		e.t.Fatal(err)
	}
	return w
}

func (e *env) write(wsID, rel, content string) {
	e.t.Helper()
	p := filepath.Join(e.r.WS.Dir(wsID), rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) read(wsID, rel string) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.r.WS.Dir(wsID), rel))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func (e *env) rm(wsID, rel string) {
	e.t.Helper()
	if err := os.Remove(filepath.Join(e.r.WS.Dir(wsID), rel)); err != nil {
		e.t.Fatal(err)
	}
}

// TestInitAndImport 覆盖 init + import + 状态。
func TestInitAndImport(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "hello\n")
	e.seed("src/b.go", "package main\n")

	st, err := e.r.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.VersionCount != 1 || !st.HasHead {
		t.Fatalf("init 后状态异常: %+v", st)
	}

	vid, err := e.r.Import(e.src)
	if err != nil {
		t.Fatal(err)
	}
	head, _, _ := e.r.Head()
	if head != vid {
		t.Fatal("import 后 head 未指向导入版本")
	}
	n, _ := e.r.DB.CountVersions()
	if n != 1 {
		t.Fatalf("版本数 = %d，期望 1（初始 v0 应被覆盖）", n)
	}

	// 导入的树应含两个文件。
	v, err := e.r.DB.GetVersion(vid)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := e.r.Store.GetTree(v.Tree)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tr.Get("a.txt"); !ok {
		t.Fatal("导入树缺少 a.txt")
	}
	if _, ok := tr.Get("src"); !ok {
		t.Fatal("导入树缺少 src")
	}
}

// TestOpenEditLand 是 M3 的核心闭环（§14.3.1）。
func TestOpenEditLand(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "l0\nl1\nl2\n")
	e.seed("src/x.go", "package x\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}

	w := e.open("", "实现登录接口")
	if got := e.read(w.ID, "a.txt"); got != "l0\nl1\nl2\n" {
		t.Fatalf("工作区初始内容 = %q", got)
	}

	// 编辑：改一行 + 新增文件。
	e.write(w.ID, "a.txt", "l0\nL1\nl2\n")
	e.write(w.ID, "new.txt", "brand new\n")

	if err := e.r.WS.Sync(w.ID); err != nil {
		t.Fatal(err)
	}
	info, err := e.r.WS.Info(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Dirty {
		t.Fatal("Sync 后应处于 dirty")
	}
	if info.CapturedOps < 2 {
		t.Fatalf("op 数 = %d，期望 ≥2", info.CapturedOps)
	}

	// 预演必须是纯只读的（D22）。
	before := dbFingerprint(t, e.r)
	pv, err := e.r.Preview(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !pv.CanLand {
		t.Fatalf("预演应可落地: %+v", pv.Conflicts)
	}
	after := dbFingerprint(t, e.r)
	if before != after {
		t.Fatal("land_preview 产生了副作用（D22 违规）")
	}

	res, err := e.r.Land(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.VersionID.IsZero() {
		t.Fatal("land 未产生 version")
	}

	// head 应前进，且工作区被焚毁。
	head, _, _ := e.r.Head()
	if head != res.VersionID {
		t.Fatal("head 未推进到新版本")
	}
	if _, err := e.r.WS.Get(w.ID); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("落地后工作区应被焚毁，得到 %v", err)
	}

	// 新版本的内容应与工作区编辑一致。
	v, err := e.r.DB.GetVersion(res.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := e.r.Store.GetTree(v.Tree)
	if err != nil {
		t.Fatal(err)
	}
	fc, ok := tr.Get("a.txt")
	if !ok {
		t.Fatal("新树缺少 a.txt")
	}
	data, err := e.r.Store.ReadFileData(fc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "l0\nL1\nl2\n" {
		t.Fatalf("落地内容 = %q", data)
	}
	if _, ok := tr.Get("new.txt"); !ok {
		t.Fatal("新树缺少 new.txt")
	}

	// 幂等：再次 land 应报工作区不存在。
	if _, err := e.r.Land(w.ID); err == nil {
		t.Fatal("重复 land 应失败")
	}

	// version_count 必须递增。
	st, _ := e.r.Status()
	if st.VersionCount != 2 {
		t.Fatalf("version_count = %d，期望 2", st.VersionCount)
	}
}

// TestConflictThenResolve 覆盖冲突路径（§14.3.2）。
func TestConflictThenResolve(t *testing.T) {
	e := newEnv(t)
	base := "l0\nl1\nl2\nl3\nl4\nl5\n"
	e.seed("a.txt", base)
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}

	// 记下共同基线，A 与 B 都从它分叉（否则 B 会拿到 A 的结果，不构成冲突）。
	base0, _, err := e.r.Head()
	if err != nil {
		t.Fatal(err)
	}

	// A 先落地。
	wa := e.open(base0.String(), "任务A")
	e.write(wa.ID, "a.txt", "l0\nAAA\nl2\nl3\nl4\nl5\n")
	if _, err := e.r.Land(wa.ID); err != nil {
		t.Fatal(err)
	}

	// B 基于同一基线改同一行。
	wb := e.open(base0.String(), "任务B")
	e.write(wb.ID, "a.txt", "l0\nBBB\nl2\nl3\nl4\nl5\n")

	pv, err := e.r.Preview(wb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pv.Conflicts) == 0 {
		t.Fatal("应检测到冲突")
	}

	res, err := e.r.Land(wb.ID)
	if !errors.Is(err, repo.ErrLandConflict) {
		t.Fatalf("期望 ErrLandConflict，得到 %v", err)
	}
	if len(res.Conflicts) == 0 {
		t.Fatal("land 应返回冲突")
	}

	// 冲突应被持久化。
	rows, err := e.r.DB.ListConflicts(wb.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("持久化冲突数 = %d，期望 1", len(rows))
	}
	key := rows[0].Key

	// 工作区应处于 conflicted。
	info, _ := e.r.WS.Info(wb.ID)
	if info.State != meta.StateConflicted {
		t.Fatalf("state = %s", info.State)
	}

	// 记录一条 resolution，再落地。
	created, err := e.r.DB.Resolver().Put(op.Resolution{
		Key:  key,
		WsID: wb.ID,
		Path: rows[0].Path,
		// lower/upper 哈希仅作记录：ConflictKey 已承载它们，Lookup 按 key 匹配。

		Kind: op.ResolveTakeUpper,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("resolution 未写入")
	}

	res2, err := e.r.Land(wb.ID)
	if err != nil {
		t.Fatalf("应用 resolution 后应可落地: %v", err)
	}
	if res2.VersionID.IsZero() {
		t.Fatal("未产生版本")
	}

	v, _ := e.r.DB.GetVersion(res2.VersionID)
	tr, _ := e.r.Store.GetTree(v.Tree)
	fc, _ := tr.Get("a.txt")
	data, _ := e.r.Store.ReadFileData(fc.ID)
	if string(data) != "l0\nBBB\nl2\nl3\nl4\nl5\n" {
		t.Fatalf("take_upper 后内容 = %q", data)
	}
}

// TestRevert 覆盖 §7.6。
func TestRevert(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "v0\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}

	w := e.open("", "改一版")
	e.write(w.ID, "a.txt", "v1\n")
	res, err := e.r.Land(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	v1 := res.VersionID

	rv, err := e.r.Revert(v1)
	if err != nil {
		t.Fatal(err)
	}
	head, _, _ := e.r.Head()
	if head != rv {
		t.Fatal("revert 后 head 未推进")
	}

	// 内容应回到 v0。
	v, _ := e.r.DB.GetVersion(rv)
	tr, _ := e.r.Store.GetTree(v.Tree)
	fc, _ := tr.Get("a.txt")
	data, _ := e.r.Store.ReadFileData(fc.ID)
	if string(data) != "v0\n" {
		t.Fatalf("revert 后内容 = %q，期望 v0", data)
	}

	// 被 revert 的版本仍在历史中（不改写）。
	if _, err := e.r.DB.GetVersion(v1); err != nil {
		t.Fatalf("v1 应仍在历史中: %v", err)
	}
	log, err := e.r.Log(10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) < 2 {
		t.Fatalf("version_log 应至少 2 条，得到 %d", len(log))
	}
}

// TestDeleteAndRename 覆盖删除与重命名经 land 后生效。
func TestDeleteAndRename(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "content\n")
	e.seed("b.txt", "to delete\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}

	w := e.open("", "整理文件")
	if err := os.Rename(filepath.Join(e.r.WS.Dir(w.ID), "a.txt"),
		filepath.Join(e.r.WS.Dir(w.ID), "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	e.rm(w.ID, "b.txt")

	res, err := e.r.Land(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := e.r.DB.GetVersion(res.VersionID)
	tr, _ := e.r.Store.GetTree(v.Tree)

	if _, ok := tr.Get("a.txt"); ok {
		t.Fatal("a.txt 应已被重命名")
	}
	if _, ok := tr.Get("renamed.txt"); !ok {
		t.Fatal("renamed.txt 缺失")
	}
	if _, ok := tr.Get("b.txt"); ok {
		t.Fatal("b.txt 应已被删除")
	}
}

// dbFingerprint 取元数据库内容指纹，用于断言 land_preview 无副作用。
func dbFingerprint(t *testing.T, r *repo.Repo) string {
	t.Helper()
	var nv, nw, no, nc, nr, nj int64
	db := r.DB.SQL()
	qs := []struct {
		q string
		p *int64
	}{
		{`SELECT COUNT(*) FROM version`, &nv},
		{`SELECT COUNT(*) FROM workspace`, &nw},
		{`SELECT COUNT(*) FROM op`, &no},
		{`SELECT COUNT(*) FROM conflict`, &nc},
		{`SELECT COUNT(*) FROM resolution`, &nr},
		{`SELECT COUNT(*) FROM journal`, &nj},
	}
	for _, it := range qs {
		if err := db.QueryRow(it.q).Scan(it.p); err != nil {
			t.Fatal(err)
		}
	}
	return fmt.Sprintf("v=%d w=%d op=%d c=%d r=%d j=%d", nv, nw, no, nc, nr, nj)
}
