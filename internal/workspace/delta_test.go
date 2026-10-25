package workspace

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyiwen/p37/internal/capture"
	"github.com/chenyiwen/p37/internal/cas"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/tree"
)

// deltaEnv 起一个最小仓库 + 工作区，用于直接测试增量重挂的差异应用逻辑。
type deltaEnv struct {
	t     *testing.T
	root  string
	src   string
	db    *meta.DB
	store *cas.Store
	m     *Manager
	mm    *mountmgr.Manager
}

func newDeltaEnv(t *testing.T) *deltaEnv {
	t.Helper()
	root := t.TempDir()
	src := t.TempDir()

	// 直接搭出 meta + cas + mountmgr，避免绕 repo 包。
	if err := os.MkdirAll(filepath.Join(root, ".p37"), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := meta.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Init(root); err != nil { // 建表（附录 J）
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	bs, err := cas.NewLocalFS(filepath.Join(root, ".p37"))
	if err != nil {
		t.Fatal(err)
	}
	store := cas.NewStore(bs, db.ObjectIndex())
	mm, err := mountmgr.New(root, mountmgr.Options{})
	if err != nil {
		t.Fatal(err)
	}
	m := New(root, db, store, mm, Options{
		Capture:            capture.DefaultOptions(),
		MaterializeDefault: mountmgr.ModeCopy,
	})
	return &deltaEnv{t: t, root: root, src: src, db: db, store: store, m: m, mm: mm}
}

// buildTree 从目录构建一棵树（不走工作区）。
func (e *deltaEnv) buildTree(dir string) *tree.Tree {
	e.t.Helper()
	ig, err := tree.NewIgnore(true, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	tr, err := capture.Build("", dir, ig, e.store, nil, capture.InodeReliable(false),
		capture.DefaultOptions())
	if err != nil {
		e.t.Fatal(err)
	}
	return tr
}

func (e *deltaEnv) writeDir(dir, rel, content string) {
	e.t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// TestApplyDeltaWritesOnlyDifferences 是增量重挂的核心用例。
//
// applyDelta 只应写入「新基准 -> 目标树」的差异：
//   - 两边一致的文件：**一个字节都不碰**
//   - 只在一侧存在的：写 / 删
//   - 嵌套目录同样正确
//
// 这是 rebase 不再把整棵树灌进 upper 的关键。
func TestApplyDeltaWritesOnlyDifferences(t *testing.T) {
	e := newDeltaEnv(t)

	// 基准：a, sub/b, sub/deep/c, keep/x
	baseDir := filepath.Join(e.root, "base")
	e.writeDir(baseDir, "a.txt", "a-base\n")
	e.writeDir(baseDir, "sub/b.go", "b\n")
	e.writeDir(baseDir, "sub/deep/c.txt", "c\n")
	e.writeDir(baseDir, "keep/x.txt", "x\n")
	base := e.buildTree(baseDir)

	// 目标：a 改内容、sub/b 不变、sub/deep/c 删除、新增 n.txt、keep 整棵不变
	tgtDir := filepath.Join(e.root, "tgt")
	e.writeDir(tgtDir, "a.txt", "a-CHANGED\n")
	e.writeDir(tgtDir, "sub/b.go", "b\n")
	e.writeDir(tgtDir, "n.txt", "new\n")
	e.writeDir(tgtDir, "keep/x.txt", "x\n")
	target := e.buildTree(tgtDir)

	// 工作区目录先摆成「基准」的样子。
	wsID := "ws_deltatest0001"
	dir := e.mm.WorkspaceDir(wsID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a.txt", "sub/b.go", "sub/deep/c.txt", "keep/x.txt"} {
		b, err := os.ReadFile(filepath.Join(baseDir, p))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 制造一个「未变但 mtime 很新」的文件，用来证明我们**没有**靠 mtime 判断。
	old := filepath.Join(dir, "keep", "x.txt")
	if err := os.Chtimes(old, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	beforeStat, err := os.Stat(old)
	if err != nil {
		t.Fatal(err)
	}

	if err := e.m.applyDelta(wsID, base.ID(), target); err != nil {
		t.Fatal(err)
	}

	// 1. 改动生效。
	if got := readFile(t, filepath.Join(dir, "a.txt")); got != "a-CHANGED\n" {
		t.Fatalf("a.txt = %q", got)
	}
	// 2. 新增出现。
	if got := readFile(t, filepath.Join(dir, "n.txt")); got != "new\n" {
		t.Fatalf("n.txt = %q", got)
	}
	// 3. 删除生效（嵌套）。
	if _, err := os.Stat(filepath.Join(dir, "sub", "deep", "c.txt")); !os.IsNotExist(err) {
		t.Fatalf("sub/deep/c.txt 应当被删除，err=%v", err)
	}
	// 4. 未变文件保持原样（内容与 mtime 都不该被动过）。
	if got := readFile(t, filepath.Join(dir, "sub", "b.go")); got != "b\n" {
		t.Fatalf("sub/b.go = %q", got)
	}
	afterStat, err := os.Stat(old)
	if err != nil {
		t.Fatal(err)
	}
	if !afterStat.ModTime().Equal(beforeStat.ModTime()) {
		t.Fatalf("未变的 keep/x.txt 被重写了: mtime %v -> %v",
			beforeStat.ModTime(), afterStat.ModTime())
	}
}

// TestApplyDeltaNoChangeIsNoop 断言目标与基准完全一致时什么都不做。
func TestApplyDeltaNoChangeIsNoop(t *testing.T) {
	e := newDeltaEnv(t)
	d := filepath.Join(e.root, "same")
	e.writeDir(d, "a.txt", "a\n")
	e.writeDir(d, "sub/b.txt", "b\n")
	base := e.buildTree(d)

	wsID := "ws_deltanoop00001"
	dir := e.mm.WorkspaceDir(wsID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.m.applyDelta(wsID, base.ID(), base); err != nil {
		t.Fatal(err)
	}
	// 未跟踪文件必须保留（§4.8：只动被跟踪的路径）。
	if got := readFile(t, filepath.Join(dir, "marker")); got != "untracked\n" {
		t.Fatalf("未跟踪文件被动了: %q", got)
	}
	// 被跟踪但未变的路径不该被创建出来（它们本来就来自 lower）。
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("未变路径被无谓写入: err=%v", err)
	}
}

// TestFlattenEntriesRoundTrip 断言基准扁平视图与目标树扁平视图对同一棵树
// 给出一致的结果 —— 这是「未变子树整棵跳过」的前提。
func TestFlattenEntriesRoundTrip(t *testing.T) {
	e := newDeltaEnv(t)
	d := filepath.Join(e.root, "round")
	e.writeDir(d, "a.txt", "a\n")
	e.writeDir(d, "sub/b.txt", "b\n")
	e.writeDir(d, "sub/deep/c.txt", "c\n")
	tr := e.buildTree(d)

	fromID, err := e.m.flattenEntries(tr.ID())
	if err != nil {
		t.Fatal(err)
	}
	fromTree := map[string]flatEntry{}
	if err := e.m.collectEntries(tr, "", fromTree); err != nil {
		t.Fatal(err)
	}

	if len(fromID) != len(fromTree) {
		t.Fatalf("条目数不同: %d vs %d", len(fromID), len(fromTree))
	}
	for p, g := range fromID {
		w, ok := fromTree[p]
		if !ok {
			t.Fatalf("路径 %s 只在基准视图里", p)
		}
		if g != w {
			t.Fatalf("路径 %s 不一致: %+v vs %+v", p, g, w)
		}
	}
	for _, p := range []string{"sub", "sub/deep"} {
		if _, ok := fromID[p]; !ok {
			t.Fatalf("目录 %s 未收录", p)
		}
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestVerifyBeforeWriteDetectsConcurrentEdit 覆盖 land_into 写盘前的乐观校验：
// 磁盘内容一旦偏离 before 树，必须返回具体路径，绝不能覆盖。
func TestVerifyBeforeWriteDetectsConcurrentEdit(t *testing.T) {
	e := newDeltaEnv(t)
	src := filepath.Join(e.root, "verify-before")
	e.writeDir(src, "a.txt", "a-v1\n")
	e.writeDir(src, "sub/b.txt", "b\n")
	before := e.buildTree(src)

	wsID := "ws_verifybefore01"
	dir := e.mm.WorkspaceDir(wsID)
	for _, p := range []string{"a.txt", "sub/b.txt"} {
		b, err := os.ReadFile(filepath.Join(src, p))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mism, err := e.m.VerifyBeforeWrite(wsID, before.ID(), []string{"a.txt", "sub/b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(mism) != 0 {
		t.Fatalf("未改动时不应有 mismatch: %v", mism)
	}

	// 模拟 agent 在「读树之后、写盘之前」直接改了文件。
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a-v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mism, err = e.m.VerifyBeforeWrite(wsID, before.ID(), []string{"a.txt", "sub/b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(mism) != 1 || mism[0] != "a.txt" {
		t.Fatalf("并发修改应只报 a.txt，得到 %v", mism)
	}
}
