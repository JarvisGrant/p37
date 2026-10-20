package health_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyiwen/p37/internal/health"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/repo"
)

func initRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	r, err := repo.Init(root, repo.Options{Materialize: mountmgr.ModeCopy})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCheckHealthyRepo(t *testing.T) {
	root := initRepo(t)
	rep := health.Check(root, health.Options{})
	if !rep.OK {
		for _, c := range rep.Checks {
			t.Logf("%s: %s %s", c.Status, c.Name, c.Detail)
		}
		t.Fatal("健康仓库不应有 FAIL")
	}
	if rep.Repo != root || rep.FormatVer == 0 {
		t.Fatalf("报告不完整: %+v", rep)
	}
}

func TestCheckMissingRepo(t *testing.T) {
	rep := health.Check(filepath.Join(t.TempDir(), "missing"), health.Options{})
	if rep.OK {
		t.Fatal("不存在的仓库不应通过")
	}
}

func TestCheckWritableIsReal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 会绕过权限位")
	}
	root := initRepo(t)
	tmp := filepath.Join(root, ".p37", "tmp")
	if err := os.Chmod(tmp, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tmp, 0o755) })

	rep := health.Check(root, health.Options{})
	if rep.OK {
		t.Fatal("不可写的 .p37/tmp 应导致 FAIL")
	}
}
