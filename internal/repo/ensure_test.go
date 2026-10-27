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
