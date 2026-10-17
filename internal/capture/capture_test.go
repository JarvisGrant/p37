package capture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyiwen/p37/internal/cas"
	"github.com/chenyiwen/p37/internal/op"
	"github.com/chenyiwen/p37/internal/tree"
)

// env 是端到端采集环境的测试脚手架。
type env struct {
	t     *testing.T
	dir   string
	store *cas.Store
	ow    ObjectWriter
	stat  *MemStat
	ig    *tree.Ignore
	opt   Options
}

func newEnv(t *testing.T) *env {
	t.Helper()
	bs, err := cas.NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ig, err := tree.NewIgnore(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := cas.NewStore(bs, cas.NewMemIndex())
	return &env{
		t: t, dir: t.TempDir(), store: store, ow: store,
		stat: NewMemStat(), ig: ig, opt: DefaultOptions(),
	}
}

func (e *env) write(rel, content string) {
	e.t.Helper()
	p := filepath.Join(e.dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) remove(rel string) {
	e.t.Helper()
	if err := os.RemoveAll(filepath.Join(e.dir, rel)); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) build() *tree.Tree {
	e.t.Helper()
	tr, err := Build("ws_1", e.dir, e.ig, e.ow, e.stat, true, e.opt)
	if err != nil {
		e.t.Fatal(err)
	}
	return tr
}

// TestBuildBasic 覆盖目录构建、忽略规则与符号链接（§3.4、附录 L.2 #3）。
func TestBuildBasic(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "a\n")
	e.write("src/b.go", "package main\n")
	e.write("node_modules/dep/index.js", "// should be ignored\n")
	e.write(".DS_Store", "ignored\n")
	if err := os.Symlink("a.txt", filepath.Join(e.dir, "link")); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}

	tr := e.build()

	if _, ok := tr.Get("node_modules"); ok {
		t.Fatal("node_modules 未被忽略")
	}
	if _, ok := tr.Get(".DS_Store"); ok {
		t.Fatal(".DS_Store 未被忽略")
	}
	_, hasA := tr.Get("a.txt")
	_, hasSrc := tr.Get("src")
	link, hasLink := tr.Get("link")
	if !hasA || !hasSrc || !hasLink {
		t.Fatalf("缺少条目: a=%v src=%v link=%v", hasA, hasSrc, hasLink)
	}
	if link.Kind != 2 { // codec.EntrySymlink
		t.Fatalf("符号链接 kind = %d", link.Kind)
	}

	// src 的子条目应存在。
	sub, err := e.store.GetTree(mustEntry(t, tr, "src").ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sub.Get("b.go"); !ok {
		t.Fatal("src/b.go 缺失")
	}
}

func mustEntry(t *testing.T, tr *tree.Tree, name string) tree.Entry {
	t.Helper()
	e, ok := tr.Get(name)
	if !ok {
		t.Fatalf("条目 %s 不存在", name)
	}
	return e
}

// TestBuildSubtreeReuse 断言未变更的子树会被复用（不重新写 CAS）（§5.5）。
func TestBuildSubtreeReuse(t *testing.T) {
	e := newEnv(t)
	e.write("src/a.txt", "a\n")
	e.write("src/deep/b.txt", "b\n")
	e.write("other/c.txt", "c\n")

	first := e.build()
	deepID := mustEntry(t, first, "src").ID

	// 只改 other/，src 子树应完全复用。
	e.write("other/c.txt", "c2\n")
	second := e.build()

	if mustEntry(t, second, "src").ID != deepID {
		t.Fatal("未变更的 src 子树没有被复用（Tree id 变了）")
	}
	if mustEntry(t, second, "other").ID == mustEntry(t, first, "other").ID {
		t.Fatal("已变更的 other 子树 Tree id 未变")
	}
}

// TestDiffRoundTrip 是 M2 的核心闭环：
//
//	baseline 目录 --Build--> T0
//	修改目录      --Build--> T1
//	Diff(T0,T1)   --Replay--> T2
//	断言 T2 == T1（逐条目）
func TestDiffRoundTrip(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "l0\nl1\nl2\nl3\nl4\nl5\n")
	e.write("src/x.go", "package x\n\nfunc X() {}\n")
	e.write("del.txt", "to be deleted\n")
	e.write("big.bin", "\x00\x01\x02binary")
	e.write("old/name.txt", "content that will move\n")

	t0 := e.build()

	// 一组覆盖各 kind 的改动。
	e.write("a.txt", "l0\nl1\nMODIFIED\nl3\nl4\nl5\n") // modify
	e.write("new.txt", "brand new\n")                  // create
	e.remove("del.txt")                                // delete
	e.remove("old/name.txt")
	e.write("moved/name.txt", "content that will move\n")   // rename
	e.write("big.bin", "\x00\x01\x02BINARY")                // blob_put
	e.write("src/x.go", "package x\r\n\r\nfunc X() {}\r\n") // attr（纯 EOL）

	t1 := e.build()

	ops, err := Diff("ws_1", t0, t1, e.ow, e.opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) == 0 {
		t.Fatal("没有推导出任何 op")
	}

	// 用 op 重放 t0，应得到 t1。
	cached := e.store
	res, err := op.Replay(t0, ops, cached, op.Options{
		WsID: "ws_1", MaxMatches: op.DefaultMaxMatches, Resolver: op.NewMemResolver(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 0 {
		t.Fatalf("不应有冲突: %+v", res.Conflicts)
	}
	if res.Tree.ID() != t1.ID() {
		t.Fatalf("重放结果 != t1\n got=%s\nwant=%s", res.Tree.ID(), t1.ID())
	}

	// 检查 op 种类覆盖。
	kinds := map[op.Kind]int{}
	for _, o := range ops {
		kinds[o.Kind]++
	}
	t.Logf("op 种类统计: modify=%d create=%d delete=%d rename=%d attr=%d blob_put=%d",
		kinds[op.KindModify], kinds[op.KindCreate], kinds[op.KindDelete],
		kinds[op.KindRename], kinds[op.KindAttr], kinds[op.KindBlobPut])

	if kinds[op.KindModify] < 1 {
		t.Error("未识别出 modify")
	}
	if kinds[op.KindRename] != 1 {
		t.Errorf("rename 数 = %d，期望 1", kinds[op.KindRename])
	}
	if kinds[op.KindAttr] != 1 {
		t.Errorf("attr 数 = %d，期望 1（纯 EOL 变化）", kinds[op.KindAttr])
	}
	if kinds[op.KindBlobPut] < 1 {
		t.Error("未识别出 blob_put")
	}
	if kinds[op.KindDelete] != 1 {
		t.Errorf("delete 数 = %d，期望 1（del.txt）", kinds[op.KindDelete])
	}
}

// TestDiffDeterministic 断言同一对树总是给出同一 op 序列。
func TestDiffDeterministic(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "1\n")
	e.write("b.txt", "2\n")
	e.write("c/d.txt", "3\n")
	t0 := e.build()
	e.write("a.txt", "1x\n")
	e.write("e.txt", "5\n")
	t1 := e.build()

	first, err := Diff("ws_1", t0, t1, e.ow, e.opt)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		got, err := Diff("ws_1", t0, t1, e.ow, e.opt)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(first) {
			t.Fatalf("op 数不同: %d vs %d", len(got), len(first))
		}
		for j := range got {
			if got[j].Kind != first[j].Kind || got[j].Path != first[j].Path ||
				got[j].Seq != first[j].Seq {
				t.Fatalf("第 %d 个 op 不同: %+v vs %+v", j, got[j], first[j])
			}
		}
	}
}

// TestCaptureStatFastPath 断言未变更文件不触发重新读取（§4.7 快路径）。
func TestCaptureStatFastPath(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "same\n")
	_ = e.build()

	before, _ := e.store.BlobStore().ListPrefix("objects/")

	// 再采集一次，内容未变 → 不应产生新对象。
	_ = e.build()
	after, _ := e.store.BlobStore().ListPrefix("objects/")

	if len(after) != len(before) {
		t.Fatalf("未变更的文件被重新写入：对象数 %d -> %d", len(before), len(after))
	}
}

// TestIgnoreMatch 覆盖 §3.4 的忽略语义与 §附录 L.2 #3。
func TestIgnoreMatch(t *testing.T) {
	ig, err := tree.NewIgnore(true, []string{"*.log", "!/keep.log", "docs/**/tmp/"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"node_modules/x.js", false, true},
		{"node_modules", true, true},
		{"a/b/node_modules/c", false, true},
		{".git/config", false, true},
		{"src/main.go", false, false},
		{"a.log", false, true},
		{"deep/a.log", false, true},
		{"keep.log", false, false}, // 取反
		{"docs/tmp", true, true},
		{"docs/a/b/tmp", true, true},
		{"docs/tmp/x.md", false, true}, // dir-only 规则覆盖其下的一切
	}
	for _, c := range cases {
		if got := ig.Match(c.path, c.isDir); got != c.want {
			t.Errorf("Match(%q, dir=%v) = %v，期望 %v", c.path, c.isDir, got, c.want)
		}
	}
}

// TestIgnoreNewIgnore 断言仓库级追加规则生效。
func TestIgnoreNewIgnore(t *testing.T) {
	ig, err := tree.NewIgnore(false, []string{"secret.env"})
	if err != nil {
		t.Fatal(err)
	}
	if !ig.Match("a/b/secret.env", false) {
		t.Fatal("追加规则未生效")
	}
	if ig.Match("node_modules/x", false) {
		t.Fatal("builtin=false 时不应有内置规则")
	}
}
