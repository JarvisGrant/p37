package mcpsrv_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRepoExportSyncAndAsync(t *testing.T) {
	h := newHarness(t)
	h.seed("a.txt", "A\n")
	h.seed("src/app.js", "console.log(1)\n")
	if _, err := h.r.Import(h.srcDir); err != nil {
		t.Fatal(err)
	}

	head, ok, err := h.r.Head()
	if err != nil || !ok {
		t.Fatalf("head: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "sync")
	data := h.mustOK("repo_export", map[string]any{
		"ref": "version:" + head.String(), "dest_dir": dest,
	})
	if data["exported"] != true {
		t.Fatalf("exported = %v", data["exported"])
	}
	if data["version_id"] != head.String() {
		t.Fatalf("version_id = %v", data["version_id"])
	}
	if data["files"].(float64) != 2 {
		t.Fatalf("files = %v", data["files"])
	}
	if _, err := os.Stat(filepath.Join(dest, "src/app.js")); err != nil {
		t.Fatal(err)
	}

	asyncDest := filepath.Join(t.TempDir(), "async")
	start := h.mustOK("repo_export", map[string]any{
		"ref": "line:main", "dest_dir": asyncDest, "async": true,
	})
	jobID, _ := start["job_id"].(string)
	if jobID == "" {
		t.Fatalf("缺少 job_id: %v", start)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := h.mustOK("repo_export_status", map[string]any{"job_id": jobID})
		state, _ := st["state"].(string)
		if state == "done" {
			if st["version_id"] != head.String() {
				t.Fatalf("async version_id = %v", st["version_id"])
			}
			break
		}
		if state == "failed" {
			t.Fatalf("异步导出失败: %v", st["error"])
		}
		if time.Now().After(deadline) {
			t.Fatalf("异步导出超时，最后状态: %v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(asyncDest, "a.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestRepoExportErrors(t *testing.T) {
	h := newHarness(t)
	h.seed("a.txt", "A\n")
	if _, err := h.r.Import(h.srcDir); err != nil {
		t.Fatal(err)
	}

	h.mustFail("repo_export", "invalid_argument", map[string]any{
		"ref": "mainline", "dest_dir": "relative",
	})
	h.mustFail("repo_export", "version_not_found", map[string]any{
		"ref":      "version:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		"dest_dir": filepath.Join(t.TempDir(), "x"),
	})
	h.mustFail("repo_export", "line_not_found", map[string]any{
		"ref": "line:no-such-line", "dest_dir": filepath.Join(t.TempDir(), "y"),
	})
}
