package repo_test

import (
	"testing"

	"github.com/chenyiwen/p37/internal/repo"
)

func TestLandToIdempotent(t *testing.T) {
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
	res, err := e.r.LandTo(w.ID, repo.LandOptions{Line: "s", IdempotencyKey: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.VersionID.IsZero() {
		t.Fatal("no version")
	}
	again, err := e.r.LandTo(w.ID, repo.LandOptions{Line: "s", IdempotencyKey: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	if !again.AlreadyLanded || again.VersionID != res.VersionID {
		t.Fatalf("again=%+v first=%+v", again, res)
	}
	// 同一 key 从另一个工作区重试也应返回原版本
	w2 := e.open("line:s", "t2")
	e.write(w2.ID, "c.txt", "C\n")
	k, err := e.r.LandTo(w2.ID, repo.LandOptions{Line: "s", IdempotencyKey: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	if k.VersionID != res.VersionID {
		t.Fatalf("key retry version=%s want %s", k.VersionID, res.VersionID)
	}
}

func TestLineMergeIdempotent(t *testing.T) {
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
	if _, err := e.r.LandTo(w.ID, repo.LandOptions{Line: "s"}); err != nil {
		t.Fatal(err)
	}
	m1, err := e.r.MergeLines("s", "main", repo.MergeOptions{NoFF: true, IdempotencyKey: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	m2, err := e.r.MergeLines("s", "main", repo.MergeOptions{NoFF: true, IdempotencyKey: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	if m1.VersionID.IsZero() || m2.VersionID != m1.VersionID {
		t.Fatalf("m1=%+v m2=%+v", m1, m2)
	}
}
