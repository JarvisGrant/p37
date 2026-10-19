package repo_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/repo"
	"github.com/chenyiwen/p37/internal/workspace"
)

// TestRedactTaskDesc 回归：journal.redact_task_desc 为真时，血缘不落任务描述
// 原文（此前该配置声明了但从未被读取）。
func TestRedactTaskDesc(t *testing.T) {
	root := t.TempDir()
	src := t.TempDir()
	r, err := repo.Init(root, repo.Options{
		Materialize:    mountmgr.ModeAuto,
		RedactTaskDesc: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })

	mustWrite(t, filepath.Join(src, "a.txt"), "v0\n")
	if _, err := r.Import(src); err != nil {
		t.Fatal(err)
	}

	const secret = "机密：重写支付逻辑"
	w, err := r.WS.Open(workspace.OpenRequest{TaskDesc: secret})
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(r.WS.Dir(w.ID), "a.txt"), "v1\n")
	if err := r.WS.Sync(w.ID); err != nil {
		t.Fatal(err)
	}
	res, err := r.Land(w.ID)
	if err != nil {
		t.Fatal(err)
	}

	j, err := r.DB.GetJournal(res.VersionID.String())
	if err != nil {
		t.Fatal(err)
	}
	if j.TaskDesc != "" {
		t.Fatalf("血缘仍含任务描述原文: %q", j.TaskDesc)
	}

	// blame 仍可用（任务显示为空），但血缘记录本身不能丢、更不能泄漏原文。
	recs, err := r.Blame("a.txt", repo.BlameOptions{Granularity: "line"})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) == 0 {
		t.Fatal("blame 结果为空")
	}
	for _, rec := range recs {
		if rec.Task == secret {
			t.Fatalf("blame 泄漏了任务描述: %+v", rec)
		}
	}
}

func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
