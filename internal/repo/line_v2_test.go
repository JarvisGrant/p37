package repo_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyiwen/p37/internal/repo"
	"github.com/chenyiwen/p37/internal/workspace"
)

func TestLineSessionLoopAndMerge(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "A\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	mainHead, _, _ := e.r.Head()

	// release/1.0 from mainline
	rel, err := e.r.CreateLine("release/1.0", "mainline", false)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Head != mainHead {
		t.Fatal("line head 应为 main head")
	}

	w := e.open("line:release/1.0", "release work")
	e.write(w.ID, "rel.txt", "R\n")
	res, err := e.r.LandTo(w.ID, repo.LandOptions{Line: "release/1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if res.VersionID.IsZero() {
		t.Fatal("missing version")
	}
	rel2, _ := e.r.Line("release/1.0")
	if rel2.Head != res.VersionID {
		t.Fatal("line head 未推进")
	}
	if h, _, _ := e.r.Head(); h != mainHead {
		t.Fatal("main 不应变化")
	}

	// reopen from line head
	w2 := e.open("line:release/1.0", "release turn 2")
	if got := e.read(w2.ID, "rel.txt"); got != "R\n" {
		t.Fatalf("重开看不到成果: %q", got)
	}
	e.write(w2.ID, "rel2.txt", "R2\n")
	if _, err := e.r.LandTo(w2.ID, repo.LandOptions{Line: "release/1.0"}); err != nil {
		t.Fatal(err)
	}
	rel3, _ := e.r.Line("release/1.0")
	if n, _ := e.r.LineLog("release/1.0", 10, 0, ""); len(n) < 2 {
		t.Fatalf("line log = %d", len(n))
	}

	// merge release -> main
	m, err := e.r.MergeLines("release/1.0", "main", repo.MergeOptions{NoFF: true})
	if err != nil {
		t.Fatal(err)
	}
	if m.VersionID.IsZero() {
		t.Fatal("merge 未产生版本")
	}
	mv, err := e.r.DB.GetVersion(m.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mv.Parents) != 2 {
		t.Fatalf("merge parents = %d, want 2", len(mv.Parents))
	}
	main2, _, _ := e.r.Head()
	if main2 != m.VersionID {
		t.Fatal("main head 未推进")
	}
	if rel3.Head == main2 {
		t.Fatal("source line 不应被合并操作移动")
	}
}

func TestLandIntoWorkspace(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "A\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	mainHead, _, _ := e.r.Head()

	parent := e.open("", "parent")
	e.write(parent.ID, "p.txt", "P\n")
	child := e.open(parent.ID, "child")
	e.write(child.ID, "c.txt", "C\n")
	preview, err := e.r.PreviewInto(child.ID, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !preview.CanLand {
		t.Fatalf("preview conflicts: %+v", preview.Conflicts)
	}
	if _, err := e.r.LandInto(child.ID, parent.ID, preview.TargetHead.String(), preview.PreviewID, ""); err != nil {
		t.Fatal(err)
	}
	if got := e.read(parent.ID, "c.txt"); got != "C\n" {
		t.Fatalf("父工作区没有子成果: %q", got)
	}
	if h, _, _ := e.r.Head(); h != mainHead {
		t.Fatal("land_into 不应推进 main")
	}
	// parent can later land
	if _, err := e.r.LandTo(parent.ID, repo.LandOptions{Line: "main"}); err != nil {
		t.Fatal(err)
	}
}

func TestVersionReadAPIs(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "hello\n")
	e.seed("bin.dat", "x\x00y")
	e.seed("dir/n.txt", "n\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	head, _, _ := e.r.Head()
	ref := "version:" + head.String()

	items, _, err := e.r.VersionTree(ref, "", true, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) < 3 {
		t.Fatalf("tree items = %d", len(items))
	}
	f, err := e.r.VersionFile(ref, "a.txt", 0)
	if err != nil {
		t.Fatal(err)
	}
	if f.ContentBase64 == "" || f.Size != 6 {
		t.Fatalf("file=%+v", f)
	}
	bin, err := e.r.VersionFile(ref, "bin.dat", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bin.Binary {
		t.Fatal("binary 未识别")
	}
	ar, err := e.r.VersionArchive(ref, "", "tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if ar.Size == 0 {
		t.Fatal("archive empty")
	}
	defer os.Remove(ar.Path)
	if _, _, err := e.r.VersionTree(ref, "../x", false, 0); err == nil {
		t.Fatal("路径穿越未被拒绝")
	}
}

func TestResetAndProtect(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "A\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	v1, _, _ := e.r.Head()
	_, err := e.r.CreateLine("work", "mainline", false)
	if err != nil {
		t.Fatal(err)
	}
	w := e.open("line:work", "work")
	e.write(w.ID, "b.txt", "B\n")
	res, err := e.r.LandTo(w.ID, repo.LandOptions{Line: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.ResetLine("work", v1.String(), false, "", ""); err != nil {
		t.Fatal(err)
	}
	l2, _ := e.r.Line("work")
	if l2.Head != v1 {
		t.Fatal("reset 未移动 head")
	}
	// 非祖先 reset 需要 force
	if _, err := e.r.ResetLine("work", res.VersionID.String(), false, "", ""); !errors.Is(err, repo.ErrTargetNotAncestor) {
		t.Fatalf("got %v", err)
	}
	if _, err := e.r.ResetLine("work", res.VersionID.String(), true, "test", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.r.ProtectLine("work", true, false); err != nil {
		t.Fatal(err)
	}
	if err := e.r.DeleteLine("work", false); !errors.Is(err, repo.ErrLineProtected) {
		t.Fatalf("got %v", err)
	}
}

var _ = workspace.ErrNotFound
var _ = filepath.Join

func TestLineCompareAndGraph(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "A\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.CreateLine("release", "mainline", false); err != nil {
		t.Fatal(err)
	}
	w := e.open("line:release", "r")
	e.write(w.ID, "r.txt", "R\n")
	if _, err := e.r.LandTo(w.ID, repo.LandOptions{Line: "release"}); err != nil {
		t.Fatal(err)
	}
	c, err := e.r.CompareLines("release", "main")
	if err != nil {
		t.Fatal(err)
	}
	if c.Ahead != 1 || c.Behind != 0 {
		t.Fatalf("compare=%+v", c)
	}
	if c.MergeBase.IsZero() {
		t.Fatal("missing merge base")
	}
	g, err := e.r.LineGraph([]string{"release", "main"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if g["nodes"] == nil {
		t.Fatalf("graph=%v", g)
	}
}
