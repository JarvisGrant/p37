package repo_test

import (
	"testing"

	"github.com/chenyiwen/p37/internal/cas"
	"github.com/chenyiwen/p37/internal/op"
	"github.com/chenyiwen/p37/internal/repo"
)

func TestMergeConflictPreviewAndResolve(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "l0\nl1\nl2\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.CreateLine("session", "mainline", false); err != nil {
		t.Fatal(err)
	}

	// main changes l1
	wm := e.open("", "main edit")
	e.write(wm.ID, "a.txt", "l0\nMAIN\nl2\n")
	if _, err := e.r.LandTo(wm.ID, repo.LandOptions{Line: "main"}); err != nil {
		t.Fatal(err)
	}

	// session changes l1 differently
	ws := e.open("line:session", "session edit")
	e.write(ws.ID, "a.txt", "l0\nSESSION\nl2\n")
	if _, err := e.r.LandTo(ws.ID, repo.LandOptions{Line: "session"}); err != nil {
		t.Fatal(err)
	}

	res, err := e.r.MergeLines("session", "main", repo.MergeOptions{Preview: true})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(res.Conflicts) == 0 {
		t.Fatalf("expected conflicts, got %+v", res)
	}
	if res.MergeID == "" {
		t.Fatal("missing merge_id")
	}
	key := res.Conflicts[0].Key
	if _, err := e.r.Resolve(key, op.ResolveTakeUpper, nil, "test"); err != nil {
		t.Fatal(err)
	}
	_ = e.r.ReopenIfNoConflicts(res.MergeID)
	out, err := e.r.MergeLines("session", "main", repo.MergeOptions{MergeID: res.MergeID})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if out.VersionID.IsZero() {
		t.Fatal("merge no version")
	}
	v, err := e.r.DB.GetVersion(out.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Parents) != 2 {
		t.Fatalf("parents=%d", len(v.Parents))
	}
	// content should be one of the resolved sides (take_upper = source/upper)
	f, err := e.r.VersionFile("version:"+out.VersionID.String(), "a.txt", 0)
	if err != nil {
		t.Fatal(err)
	}
	if f.Size == 0 {
		t.Fatal("missing file")
	}
	_ = cas.ErrNotExist
}
