package repo_test

import (
	"errors"
	"testing"

	"github.com/chenyiwen/p37/internal/repo"
)

// TestBlameAttributesLinesToIntroducingVersion 是 §12.2 的核心用例。
//
// 历史：V1 建 a.txt（3 行）→ V2 改第 3 行 → V3 改第 1 行。
// 期望：第 1 行归 V3、第 2 行归 V1（诞生）、第 3 行归 V2。
//
// 这个例子专门用来卡「反向回溯」的错误实现：反向回溯在从 V3 往 V2 走时会把
// 未变更的行一律交给 parent，走到 V1 时第 1 行的归属就被抹成 V1 了。
func TestBlameAttributesLinesToIntroducingVersion(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "L1\nL2\nL3\n")
	v1, err := e.r.Import(e.src)
	if err != nil {
		t.Fatal(err)
	}

	w1 := e.open("", "改第3行")
	e.write(w1.ID, "a.txt", "L1\nL2\nL3-V2\n")
	r2, err := e.r.Land(w1.ID)
	if err != nil {
		t.Fatal(err)
	}

	w2 := e.open("", "改第1行")
	e.write(w2.ID, "a.txt", "L1-V3\nL2\nL3-V2\n")
	r3, err := e.r.Land(w2.ID)
	if err != nil {
		t.Fatal(err)
	}

	recs, err := e.r.Blame("a.txt", repo.BlameOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("记录数 = %d，期望 3: %+v", len(recs), recs)
	}

	want := []struct {
		line    int
		version string
		task    string
		born    bool
	}{
		{1, r3.VersionID.String(), "改第1行", false},
		{2, v1.String(), "", true},
		{3, r2.VersionID.String(), "改第3行", false},
	}
	for i, w := range want {
		got := recs[i]
		if got.FromLine != w.line || got.ToLine != w.line {
			t.Fatalf("第 %d 条区间 = %d-%d，期望第 %d 行", i, got.FromLine, got.ToLine, w.line)
		}
		if got.VersionID != w.version {
			t.Errorf("第 %d 行归属 = %s，期望 %s", w.line, got.VersionID, w.version)
		}
		if got.Originated != w.born {
			t.Errorf("第 %d 行 originated = %v，期望 %v", w.line, got.Originated, w.born)
		}
		if w.task != "" && got.Task != w.task {
			t.Errorf("第 %d 行 task = %q，期望 %q（blame 必须直接给出任务描述）", w.line, got.Task, w.task)
		}
	}
	// 首版本的行没有 journal（import 不写 journal），必须标记为不可得而不是报错。
	if !recs[1].Available {
		t.Log("首版本无 journal：Available=false 是允许的")
	}
}

// TestBlameFileBornLater 覆盖文件在中间某版才诞生的情形。
func TestBlameFileBornLater(t *testing.T) {
	e := newEnv(t)
	e.seed("keep.txt", "k\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	w := e.open("", "新增文件")
	e.write(w.ID, "new.txt", "n1\nn2\n")
	res, err := e.r.Land(w.ID)
	if err != nil {
		t.Fatal(err)
	}

	recs, err := e.r.Blame("new.txt", repo.BlameOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("记录数 = %d，期望 1: %+v", len(recs), recs)
	}
	if recs[0].FromLine != 1 || recs[0].ToLine != 2 {
		t.Fatalf("区间 = %d-%d，期望 1-2", recs[0].FromLine, recs[0].ToLine)
	}
	if recs[0].VersionID != res.VersionID.String() {
		t.Fatalf("归属 = %s，期望 %s", recs[0].VersionID, res.VersionID)
	}
	if !recs[0].Originated {
		t.Fatal("文件诞生版本的行必须标记为 originated")
	}
}

// TestBlameUnchangedFileAcrossVersions 覆盖「某版本完全没碰这个文件」。
func TestBlameUnchangedFileAcrossVersions(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "L1\nL2\n")
	e.seed("b.txt", "B\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	// 改 b.txt —— a.txt 完全没动。
	w := e.open("", "只改 b")
	e.write(w.ID, "b.txt", "B2\n")
	if _, err := e.r.Land(w.ID); err != nil {
		t.Fatal(err)
	}

	recs, err := e.r.Blame("a.txt", repo.BlameOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].FromLine != 1 || recs[0].ToLine != 2 {
		t.Fatalf("a.txt 应整段归首版本: %+v", recs)
	}
	if !recs[0].Originated {
		t.Error("两行都应标记为 originated")
	}
}

// TestBlameRangeAndErrors 覆盖区间裁剪与错误路径。
func TestBlameRangeAndErrors(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "1\n2\n3\n4\n5\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}

	recs, err := e.r.Blame("a.txt", repo.BlameOptions{From: 2, To: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].FromLine != 2 || recs[0].ToLine != 4 {
		t.Fatalf("区间裁剪结果 = %+v，期望 2-4", recs)
	}

	if _, err := e.r.Blame("nope.txt", repo.BlameOptions{}); !errors.Is(err, repo.ErrPathNotFound) {
		t.Fatalf("不存在的路径应返回 ErrPathNotFound，得到 %v", err)
	}
	if _, err := e.r.Blame("a.txt", repo.BlameOptions{From: 99}); err == nil {
		t.Fatal("越界起始行应报错")
	}
	if _, err := e.r.Blame("a.txt", repo.BlameOptions{Granularity: "bogus"}); err == nil {
		t.Fatal("非法 granularity 应报错")
	}
}

// TestBlameCacheIsConsistent 断言缓存命中与不命中的结果一致。
func TestBlameCacheIsConsistent(t *testing.T) {
	e := newEnv(t)
	e.seed("a.txt", "a\nb\nc\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	w := e.open("", "改 b")
	e.write(w.ID, "a.txt", "a\nB\nc\n")
	if _, err := e.r.Land(w.ID); err != nil {
		t.Fatal(err)
	}

	cached, err := e.r.Blame("a.txt", repo.BlameOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := e.r.Blame("a.txt", repo.BlameOptions{NoCache: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(cached) != len(fresh) {
		t.Fatalf("缓存与重算记录数不同: %d vs %d", len(cached), len(fresh))
	}
	for i := range cached {
		if cached[i] != fresh[i] {
			t.Fatalf("第 %d 条不一致:\n 缓存: %+v\n 重算: %+v", i, cached[i], fresh[i])
		}
	}
	if len(cached) != 3 {
		t.Fatalf("期望 3 段归属，得到 %d: %+v", len(cached), cached)
	}
}
