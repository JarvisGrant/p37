package main

import (
	"bytes"
	"encoding/json"
	"strings"
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

func TestVersionIsParseable(t *testing.T) {
	var out bytes.Buffer
	printVersion(&out)
	for _, key := range []string{"version=", "commit=", "buildTime=", "goVersion="} {
		if !strings.Contains(out.String(), key) {
			t.Errorf("版本输出缺少 %s: %q", key, out.String())
		}
	}
}

func TestCheckValidRepo(t *testing.T) {
	root := initRepo(t)
	var out, errw bytes.Buffer
	code := runCheck(root, false, false, &out, &errw)
	if code != 0 {
		t.Fatalf("健康仓库应返回 0，得到 %d\n%s\n%s", code, out.String(), errw.String())
	}
	if !strings.Contains(out.String(), "健康检查通过") {
		t.Fatalf("输出异常: %s", out.String())
	}
}

func TestCheckMissingRepo(t *testing.T) {
	var out, errw bytes.Buffer
	code := runCheck(t.TempDir()+"/not-a-repo", false, false, &out, &errw)
	if code != 1 {
		t.Fatalf("不存在的仓库应返回 1，得到 %d", code)
	}
	if !strings.Contains(out.String(), "FAIL") {
		t.Fatalf("输出没有失败项: %s", out.String())
	}
}

func TestCheckJSONMatchesExitCode(t *testing.T) {
	root := initRepo(t)
	var out, errw bytes.Buffer
	code := runCheck(root, true, false, &out, &errw)
	if code != 0 {
		t.Fatalf("JSON 检查失败: %d %s", code, errw.String())
	}
	var rep health.Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("JSON 不可解析: %v\n%s", err, out.String())
	}
	if !rep.OK || rep.Repo != root || rep.FormatVer == 0 {
		t.Fatalf("报告不完整: %+v", rep)
	}
	if len(rep.Checks) == 0 {
		t.Fatal("报告没有检查项")
	}
}

func TestCheckStrictMatchesReport(t *testing.T) {
	root := initRepo(t)
	var out, errw bytes.Buffer
	code := runCheck(root, true, true, &out, &errw)
	var rep health.Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("JSON 不可解析: %v", err)
	}
	want := 0
	if !rep.OK {
		want = 1
	}
	if code != want {
		t.Fatalf("--strict 退出码 = %d，报告 ok=%v", code, rep.OK)
	}
}
