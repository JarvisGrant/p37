package mcpsrv_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenyiwen/p37/internal/mcpsrv"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/repo"
)

// TestMetricsEndpoint 回归：daemon.metrics_http 未接线时 /metrics 不存在；
// 现在必须给出 Prometheus 文本。
func TestMetricsEndpoint(t *testing.T) {
	root := t.TempDir()
	src := t.TempDir()
	r, err := repo.Init(root, repo.Options{Materialize: mountmgr.ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })

	if err := os.MkdirAll(filepath.Dir(filepath.Join(src, "a.txt")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Import(src); err != nil {
		t.Fatal(err)
	}

	srv := mcpsrv.New(r)
	h := mcpsrv.MetricsHandler(srv)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics 状态码 = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"p37_build_info{", "p37_versions_total ", "p37_workspaces_total ",
		"p37_mainline_head_present ", "# TYPE p37_versions_total gauge",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/metrics 缺少 %q\n%s", want, body)
		}
	}

	// 只读端点：拒绝写方法。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /metrics 状态码 = %d，期望 405", rec.Code)
	}
}
