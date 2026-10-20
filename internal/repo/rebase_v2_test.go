package repo_test

import (
	"testing"

	"github.com/chenyiwen/p37/internal/repo"
)

func TestRebaseToLineAndWSDiff(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "A\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.CreateLine("s", "mainline", false); err != nil {
		t.Fatal(err)
	}
	w := e.open("line:s", "t")
	e.write(w.ID, "b.txt", "B\n")
	e.write(w.ID, "a.txt", "A2\n")
	diff, err := e.r.WSDiff(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff) != 2 {
		t.Fatalf("diff=%+v", diff)
	}
	// advance line s and rebase the workspace onto it
	w2 := e.open("line:s", "t2")
	e.write(w2.ID, "c.txt", "C\n")
	if _, err := e.r.LandTo(w2.ID, repo.LandOptions{Line: "s"}); err != nil {
		t.Fatal(err)
	}
	res, err := e.r.WS.RebaseTo(w.ID, "line:s")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Rebased {
		t.Fatalf("res=%+v", res)
	}
	if got := e.read(w.ID, "c.txt"); got != "C\n" {
		t.Fatalf("rebase missing c.txt: %q", got)
	}
	if got := e.read(w.ID, "b.txt"); got != "B\n" {
		t.Fatalf("rebase lost local change: %q", got)
	}
}
