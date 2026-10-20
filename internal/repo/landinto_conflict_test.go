package repo_test

import (
	"testing"

	"github.com/chenyiwen/p37/internal/op"
	"github.com/chenyiwen/p37/internal/repo"
)

func TestLandIntoDeleteVsEditConflict(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "A\n")
	e.seed("keep.txt", "K\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	parent := e.open("", "parent")
	child := e.open(parent.ID, "child")
	// child modifies a.txt; parent deletes it.
	e.write(child.ID, "a.txt", "CHILD\n")
	e.rm(parent.ID, "a.txt")
	p, err := e.r.PreviewInto(child.ID, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Conflicts) == 0 {
		t.Fatalf("expected conflict, got %+v", p)
	}
	if p.PreviewID == "" {
		t.Fatal("missing preview_id")
	}
	if _, err := e.r.Resolve(p.Conflicts[0].Key, op.ResolveTakeUpper, nil, "test"); err != nil {
		t.Fatal(err)
	}
	_ = e.r.ReopenIfNoConflicts(p.PreviewID)
	if _, err := e.r.LandInto(child.ID, parent.ID, p.TargetHead.String(), p.PreviewID, ""); err != nil {
		t.Fatal(err)
	}
	_ = repo.ErrLandConflict
}
