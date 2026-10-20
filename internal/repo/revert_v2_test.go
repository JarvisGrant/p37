package repo_test

import (
	"testing"

	"github.com/chenyiwen/p37/internal/repo"
)

func TestRevertToLine(t *testing.T) {
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
	res, err := e.r.LandTo(w.ID, repo.LandOptions{Line: "s"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.r.RevertTo(res.VersionID.String(), "s", "", "")
	if err != nil {
		t.Fatal(err)
	}
	v, err := e.r.VersionFile("version:"+out.String(), "b.txt", 0)
	if err == nil && v.Size > 0 {
		t.Fatal("revert 未删除 b.txt")
	}
}
