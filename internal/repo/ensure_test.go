package repo

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chenyiwen/p37/internal/cas"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/tree"
	"github.com/chenyiwen/p37/internal/version"
	"github.com/chenyiwen/p37/internal/workspace"
)

func ensureOpt(mode InitMode) EnsureOptions {
	return EnsureOptions{Mode: mode, Name: "app", DefaultLine: "main", EnforcePersistent: false}
}

func TestEnsureEmptyIgnoresLostAndFound(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "lost+found"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Ensure(root, ensureOpt(InitEmpty))
	if err != nil {
		t.Fatal(err)
	}
	if res.Repo.VersionCount != 1 || res.Repo.MainlineHead == "" {
		t.Fatalf("res=%+v", res)
	}
	in, err := CheckIntegrity(root)
	if err != nil || !in.Initialized {
		t.Fatalf("integrity=%+v err=%v", in, err)
	}
	if !in.DefaultLineReady || !in.RefsOK || !in.RootOK {
		t.Fatalf("integrity=%+v", in)
	}
	// 不应把 lost+found 当成用户文件。
	if user := CountUserFiles(root); user != 0 {
		t.Fatalf("user files=%d", user)
	}
}

func TestEnsureIdempotent(t *testing.T) {
	root := t.TempDir()
	first, err := Ensure(root, ensureOpt(InitEmpty))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Ensure(root, ensureOpt(InitEmpty))
	if err != nil {
		t.Fatal(err)
	}
	if first.Repo.MainlineHead != second.Repo.MainlineHead {
		t.Fatalf("head changed: %s -> %s", first.Repo.MainlineHead, second.Repo.MainlineHead)
	}
	db, err := meta.OpenReadOnly(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if n, _ := db.CountVersions(); n != 1 {
		t.Fatalf("version count=%d", n)
	}
	lines, _ := db.ListLines()
	if len(lines) != 1 || lines[0].Name != "main" {
		t.Fatalf("lines=%+v", lines)
	}
}

func TestEnsureConcurrent(t *testing.T) {
	root := t.TempDir()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = Ensure(root, ensureOpt(InitEmpty))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("ensure[%d]=%v", i, err)
		}
	}
	db, err := meta.OpenReadOnly(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if n, _ := db.CountVersions(); n != 1 {
		t.Fatalf("version count=%d", n)
	}
	lines, _ := db.ListLines()
	if len(lines) != 1 {
		t.Fatalf("lines=%+v", lines)
	}
}

func TestEnsureRepairsOrphanRootWithoutSecondRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".p37"), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := meta.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Init(root); err != nil {
		t.Fatal(err)
	}
	// 模拟 bad.db：repo 行 + 一个 root v0，但没有 head/line/refs。
	bs, err := testCS(root, db)
	if err != nil {
		t.Fatal(err)
	}
	tid, err := bs.PutTree(&tree.Tree{})
	if err != nil {
		t.Fatal(err)
	}
	v := version.New(hash.Zero, tid, 0, "p37-init", "init", time.Now().UnixMilli(), "init")
	if err := db.InsertVersion(v); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	res, err := Ensure(root, ensureOpt(InitRepair))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Repaired {
		t.Fatalf("expected repaired result: %+v", res)
	}
	db, err = meta.OpenReadOnly(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	st, err := db.RootState()
	if err != nil {
		t.Fatal(err)
	}
	if st.VersionCount != 1 {
		t.Fatalf("version count=%d", st.VersionCount)
	}
	if len(st.Roots) != 1 || st.Roots[0] != v.ID() {
		t.Fatalf("roots=%v want %s", st.Roots, v.ID())
	}
	if st.Repo.MainlineHead != v.ID() {
		t.Fatalf("head=%s want %s", st.Repo.MainlineHead, v.ID())
	}
	if len(st.Lines) != 1 || st.Lines[0].Name != "main" || st.Lines[0].Head != v.ID() {
		t.Fatalf("lines=%+v", st.Lines)
	}
	ref, err := os.ReadFile(filepath.Join(root, ".p37", "refs", "mainline"))
	if err != nil {
		t.Fatal(err)
	}
	if string(ref) != v.ID().String()+"\n" {
		t.Fatalf("refs=%q", ref)
	}
}

func TestEnsureNonEmptyEmptyMode(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "user.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Ensure(root, ensureOpt(InitEmpty))
	var ne *NotEmptyError
	if !errors.As(err, &ne) {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, ".p37")); !os.IsNotExist(statErr) {
		t.Fatalf(".p37 不应存在: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, "user.txt")); statErr != nil {
		t.Fatalf("用户文件被改动: %v", statErr)
	}
}

func TestEnsureImport(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "lost+found"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "workspaces"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Ensure(root, ensureOpt(InitImport))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Imported {
		t.Fatalf("res=%+v", res)
	}
	db, err := meta.OpenReadOnly(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	v, err := db.GetVersion(stFromRes(t, res))
	if err != nil {
		t.Fatal(err)
	}
	bs, err := testCS(root, db)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := bs.GetTree(v.Tree)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range tr.Entries {
		if e.Name == "a.txt" {
			found = true
		}
		if e.Name == "lost+found" || e.Name == "workspaces" || e.Name == ".p37" {
			t.Fatalf("不应导入控制目录/系统目录: %+v", e)
		}
	}
	if !found {
		t.Fatalf("a.txt 未导入: %+v", tr.Entries)
	}
}

func TestStrictDoesNotModifyHalfRepo(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".p37"), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := meta.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Init(root); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	before, err := CheckIntegrity(root)
	if err != nil {
		t.Fatal(err)
	}
	if before.Initialized {
		t.Fatal("half repo 不应 initialized")
	}
	// CheckIntegrity 是 --strict 的实现；它不能补行。
	after, err := CheckIntegrity(root)
	if err != nil {
		t.Fatal(err)
	}
	if after.VersionCount != 0 || after.DefaultLineReady {
		t.Fatalf("strict 修改了仓库: %+v", after)
	}
}

func TestImportFailureLeavesOldHead(t *testing.T) {
	root := t.TempDir()
	if _, err := Ensure(root, ensureOpt(InitEmpty)); err != nil {
		t.Fatal(err)
	}
	r, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	head, _, _ := r.DB.Head()
	_, err = r.ImportWithOptions(filepath.Join(root, "does-not-exist"), ImportOptions{})
	if err == nil {
		t.Fatal("不存在的目录不应 import 成功")
	}
	st, err := r.DB.GetRepo()
	if err != nil {
		t.Fatal(err)
	}
	if st.MainlineHead != head {
		t.Fatalf("head changed on failed import: %s -> %s", head, st.MainlineHead)
	}
}

func testCS(root string, db *meta.DB) (*cas.Store, error) {
	bs, err := cas.NewLocalFS(filepath.Join(root, ".p37"))
	if err != nil {
		return nil, err
	}
	return cas.NewStore(cas.NewPackStore(bs), db.ObjectIndex()), nil
}

func stFromRes(t *testing.T, res *EnsureResult) hash.ID {
	t.Helper()
	id, err := hash.Parse(res.Version.ID)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestEnsureFaultInjectionRetry(t *testing.T) {
	steps := []string{"after_db", "after_objects", "after_v0", "after_refs", "before_rename", "before_parent_fsync"}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("P37_INIT_FAULT_AT", step)
			if _, err := Ensure(root, ensureOpt(InitEmpty)); err == nil {
				t.Fatalf("fault %s 未触发", step)
			}
			t.Setenv("P37_INIT_FAULT_AT", "")
			res, err := Ensure(root, ensureOpt(InitEmpty))
			if err != nil {
				t.Fatalf("重试失败: %v", err)
			}
			if res.Repo.VersionCount != 1 {
				t.Fatalf("res=%+v", res)
			}
			db, err := meta.OpenReadOnly(root)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			st, err := db.RootState()
			if err != nil {
				t.Fatal(err)
			}
			if st.VersionCount != 1 || len(st.Roots) != 1 || len(st.Lines) != 1 {
				t.Fatalf("after %s: %+v", step, st)
			}
		})
	}
}

func TestImportFaultBeforeSwapKeepsHead(t *testing.T) {
	root := t.TempDir()
	if _, err := Ensure(root, ensureOpt(InitEmpty)); err != nil {
		t.Fatal(err)
	}
	r, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	head, _, _ := r.DB.Head()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("P37_IMPORT_FAULT_AT", "before_swap")
	if _, err := r.ImportWithOptions(src, ImportOptions{Force: true}); err == nil {
		t.Fatal("fault 未触发")
	}
	t.Setenv("P37_IMPORT_FAULT_AT", "")
	st, err := r.DB.GetRepo()
	if err != nil {
		t.Fatal(err)
	}
	if st.MainlineHead != head || st.VersionCount != 1 {
		t.Fatalf("failed import changed repo: head=%s want=%s count=%d", st.MainlineHead, head, st.VersionCount)
	}
	if _, err := r.DB.GetVersion(head); err != nil {
		t.Fatalf("old head version missing: %v", err)
	}
}

func TestImportRefusesWhenNamedLineExists(t *testing.T) {
	root := t.TempDir()
	if _, err := Ensure(root, ensureOpt(InitEmpty)); err != nil {
		t.Fatal(err)
	}
	r, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if _, err := r.CreateLine("s", "mainline", false); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ImportWithOptions(src, ImportOptions{Force: true}); err == nil {
		t.Fatal("有命名 line 时不应允许覆盖 v0")
	}
	// 旧 head 仍然可用。
	head, _, _ := r.DB.Head()
	if _, err := r.DB.GetVersion(head); err != nil {
		t.Fatalf("旧 head 丢失: %v", err)
	}
}

func breakMetadata(t *testing.T, root string) {
	t.Helper()
	db, err := meta.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteLine("main", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if err := os.Remove(filepath.Join(root, ".p37", "refs", "mainline")); err != nil {
		t.Fatal(err)
	}
}

func repoHead(t *testing.T, root string) (hash.ID, int64) {
	t.Helper()
	db, err := meta.OpenReadOnly(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	st, err := db.RootState()
	if err != nil {
		t.Fatal(err)
	}
	return st.Repo.MainlineHead, st.VersionCount
}

func treeHas(t *testing.T, root string, id hash.ID, name string) bool {
	t.Helper()
	db, err := meta.OpenReadOnly(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	bs, err := cas.NewLocalFS(filepath.Join(root, ".p37"))
	if err != nil {
		t.Fatal(err)
	}
	store := cas.NewStore(cas.NewPackStore(bs), db.ObjectIndex())
	v, err := db.GetVersion(id)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := store.GetTree(v.Tree)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range tr.Entries {
		if e.Name == name {
			return true
		}
	}
	return false
}

func TestEnsureHalfUserFilesRefusesEmptyRepair(t *testing.T) {
	root := t.TempDir()
	if _, err := Ensure(root, ensureOpt(InitEmpty)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "user.txt"), []byte("U\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, beforeCount := repoHead(t, root)
	breakMetadata(t, root)
	for _, mode := range []InitMode{InitEmpty, InitRepair} {
		_, err := Ensure(root, ensureOpt(mode))
		var ne *NotEmptyError
		if !errors.As(err, &ne) || ne.NextAction != "repo_init_import" {
			t.Fatalf("mode=%s err=%v", mode, err)
		}
		head, count := repoHead(t, root)
		if head != before || count != beforeCount {
			t.Fatalf("mode=%s changed DB: head=%s count=%d", mode, head, count)
		}
		if b, _ := os.ReadFile(filepath.Join(root, "user.txt")); string(b) != "U\n" {
			t.Fatalf("用户文件被改动: %q", b)
		}
	}
}

func TestEnsureImportHalfEmptyV0(t *testing.T) {
	root := t.TempDir()
	if _, err := Ensure(root, ensureOpt(InitEmpty)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "user.txt"), []byte("U\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	breakMetadata(t, root)
	res, err := Ensure(root, ensureOpt(InitImport))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Imported || res.Repo.VersionCount != 1 {
		t.Fatalf("res=%+v", res)
	}
	head, _ := repoHead(t, root)
	if !treeHas(t, root, head, "user.txt") {
		t.Fatal("user.txt 未进入 v0")
	}
}

func importedHalf(t *testing.T, root, worktree string) hash.ID {
	t.Helper()
	if _, err := Ensure(root, ensureOpt(InitEmpty)); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Import(src); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	if worktree != "" {
		if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte(worktree), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	head, _ := repoHead(t, root)
	breakMetadata(t, root)
	return head
}

func TestEnsureImportHalfNonEmptySameRepairsOnly(t *testing.T) {
	root := t.TempDir()
	old := importedHalf(t, root, "A\n")
	res, err := Ensure(root, ensureOpt(InitImport))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Repaired || res.Imported {
		t.Fatalf("res=%+v", res)
	}
	head, count := repoHead(t, root)
	if head != old || count != 1 {
		t.Fatalf("head=%s want=%s count=%d", head, old, count)
	}
}

func TestEnsureImportHalfNonEmptyDifferentForce(t *testing.T) {
	root := t.TempDir()
	old := importedHalf(t, root, "B\n")
	_, err := Ensure(root, ensureOpt(InitImport))
	var ne *NotEmptyError
	if !errors.As(err, &ne) || ne.NextAction != "repo_init_import_force" {
		t.Fatalf("err=%v", err)
	}
	head, count := repoHead(t, root)
	if head != old || count != 1 {
		t.Fatalf("force=false changed DB: head=%s count=%d", head, count)
	}
	opt := ensureOpt(InitImport)
	opt.Force = true
	res, err := Ensure(root, opt)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Imported {
		t.Fatalf("res=%+v", res)
	}
	head, count = repoHead(t, root)
	if head == old || count != 1 {
		t.Fatalf("force=true head=%s old=%s count=%d", head, old, count)
	}
}

func TestEnsureImportHalfWorktreeEmptyForce(t *testing.T) {
	root := t.TempDir()
	old := importedHalf(t, root, "")
	_, err := Ensure(root, ensureOpt(InitImport))
	var ne *NotEmptyError
	if !errors.As(err, &ne) || ne.NextAction != "repo_init_import_force" {
		t.Fatalf("err=%v", err)
	}
	opt := ensureOpt(InitImport)
	opt.Force = true
	res, err := Ensure(root, opt)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Imported {
		t.Fatalf("res=%+v", res)
	}
	head, _ := repoHead(t, root)
	if head == old {
		t.Fatal("空 worktree 未替换 v0")
	}
	if treeHas(t, root, head, "a.txt") {
		t.Fatal("v0 仍包含 a.txt")
	}
}

func TestImportV0RefusesHistory(t *testing.T) {
	root := t.TempDir()
	if _, err := Ensure(root, ensureOpt(InitEmpty)); err != nil {
		t.Fatal(err)
	}
	r, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateLine("s", "mainline", false); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = Ensure(root, ensureOpt(InitImport))
	var he *HistoryNotEmptyError
	if !errors.As(err, &he) {
		t.Fatalf("err=%v", err)
	}
}

func TestImportV0RefusesBusy(t *testing.T) {
	root := t.TempDir()
	if _, err := Ensure(root, ensureOpt(InitEmpty)); err != nil {
		t.Fatal(err)
	}
	r, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WS.Open(workspace.OpenRequest{Parent: "mainline", TaskDesc: "busy"}); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	if err := os.WriteFile(filepath.Join(root, "u.txt"), []byte("U\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	breakMetadata(t, root)
	_, err = Ensure(root, ensureOpt(InitImport))
	var be *BusyError
	if !errors.As(err, &be) || len(be.Workspaces) == 0 {
		t.Fatalf("err=%v", err)
	}
}

func TestInitIdempotencyKeyConflict(t *testing.T) {
	root := t.TempDir()
	opt := ensureOpt(InitEmpty)
	opt.IdempotencyKey = "k1"
	if _, err := Ensure(root, opt); err != nil {
		t.Fatal(err)
	}
	head, count := repoHead(t, root)
	if _, err := Ensure(root, opt); err != nil {
		t.Fatalf("same request should be idempotent: %v", err)
	}
	head2, count2 := repoHead(t, root)
	if head2 != head || count2 != count {
		t.Fatalf("idempotent changed repo: %s/%d -> %s/%d", head, count, head2, count2)
	}
	opt.Mode = InitImport
	_, err := Ensure(root, opt)
	var ie *IdemConflictError
	if !errors.As(err, &ie) || ie.Field != "mode" {
		t.Fatalf("err=%v", err)
	}
}

func preparedImportSwap(t *testing.T) (string, hash.ID) {
	t.Helper()
	root := t.TempDir()
	if _, err := Ensure(root, ensureOpt(InitEmpty)); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Import(src); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	head, _ := repoHead(t, root)
	return root, head
}

func TestImportFaultPreCommitRollsBack(t *testing.T) {
	for _, step := range []string{"swap_after_delete", "swap_after_insert", "swap_before_commit"} {
		t.Run(step, func(t *testing.T) {
			root, old := preparedImportSwap(t)
			r, err := Open(root, Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("P37_IMPORT_FAULT_AT", step)
			if _, err := r.ImportV0(root, true); err == nil {
				t.Fatalf("%s 未触发", step)
			}
			t.Setenv("P37_IMPORT_FAULT_AT", "")
			_ = r.Close()
			head, count := repoHead(t, root)
			if head != old || count != 1 {
				t.Fatalf("rollback failed: head=%s want=%s count=%d", head, old, count)
			}
			if b, _ := os.ReadFile(filepath.Join(root, "a.txt")); string(b) != "B\n" {
				t.Fatalf("用户文件被改动: %q", b)
			}
			ref, err := os.ReadFile(filepath.Join(root, ".p37", "refs", "mainline"))
			if err != nil || string(ref) != old.String()+"\n" {
				t.Fatalf("refs=%q err=%v", ref, err)
			}
		})
	}
}

func TestImportFaultAfterCommitBeforeRefsRepair(t *testing.T) {
	root, old := preparedImportSwap(t)
	r, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("P37_IMPORT_FAULT_AT", "after_commit_before_refs")
	if _, err := r.ImportV0(root, true); err == nil {
		t.Fatal("fault 未触发")
	}
	t.Setenv("P37_IMPORT_FAULT_AT", "")
	_ = r.Close()
	head, count := repoHead(t, root)
	if head == old || count != 1 {
		t.Fatalf("DB head 未推进: head=%s old=%s count=%d", head, old, count)
	}
	ref, _ := os.ReadFile(filepath.Join(root, ".p37", "refs", "mainline"))
	if string(ref) != old.String()+"\n" {
		t.Fatalf("refs 应保持旧值: %q", ref)
	}
	in, _ := CheckIntegrity(root)
	if in.Initialized || !in.DataOK || in.NextAction != "repo_init_repair" {
		t.Fatalf("integrity=%+v", in)
	}
	if _, err := Ensure(root, ensureOpt(InitRepair)); err != nil {
		t.Fatalf("repair refs: %v", err)
	}
	in, _ = CheckIntegrity(root)
	if !in.Initialized || !in.RefsMatch {
		t.Fatalf("repair 后 integrity=%+v", in)
	}
}

func TestDefaultLineLandUpdatesRefs(t *testing.T) {
	root := t.TempDir()
	if _, err := Ensure(root, ensureOpt(InitEmpty)); err != nil {
		t.Fatal(err)
	}
	r, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	w, err := r.WS.Open(workspace.OpenRequest{Parent: "mainline", TaskDesc: "refs"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.WS.Dir(w.ID), "a.txt"), []byte("A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := r.LandTo(w.ID, LandOptions{Line: "main"})
	if err != nil {
		t.Fatal(err)
	}
	head, _, _ := r.DB.Head()
	ref, err := os.ReadFile(filepath.Join(root, ".p37", "refs", "mainline"))
	if err != nil {
		t.Fatal(err)
	}
	if string(ref) != head.String()+"\n" {
		t.Fatalf("refs=%q head=%s res=%+v", ref, head, res)
	}
	in := IntegrityOpen(r)
	if !in.Initialized || !in.RefsMatch {
		t.Fatalf("integrity=%+v", in)
	}
}
