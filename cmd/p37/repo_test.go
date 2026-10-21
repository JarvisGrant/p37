package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCLIMultiRepo(t *testing.T) {
	root := t.TempDir()
	code, out, errs := runCLI("--root", root, "repo", "create", "app", "--json")
	if code != 0 {
		t.Fatalf("repo create: %d %s", code, errs)
	}
	var e struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(out), &e); err != nil {
		t.Fatalf("create json: %v\n%s", err, out)
	}
	if e.ID == "" || e.Name != "app" || e.Path == "" {
		t.Fatalf("create=%+v", e)
	}

	code, out, errs = runCLI("--root", root, "repo", "list", "--json")
	if code != 0 || !strings.Contains(out, `"name": "app"`) {
		t.Fatalf("repo list: %d %s %s", code, out, errs)
	}

	code, out, errs = runCLI("--root", root, "repo", "info", "app", "--json")
	if code != 0 || !strings.Contains(out, e.ID) {
		t.Fatalf("repo info: %d %s %s", code, out, errs)
	}

	code, _, errs = runCLI("--root", root, "--repo", "app", "status")
	if code != 0 {
		t.Fatalf("status via --root: %d %s", code, errs)
	}

	code, _, errs = runCLI("--root", root, "--repo", "missing", "status")
	if code == 0 {
		t.Fatalf("missing repo 应失败: %s", errs)
	}

	code, _, errs = runCLI("--root", root, "repo", "delete", "app", "--force")
	if code != 0 {
		t.Fatalf("repo delete: %d %s", code, errs)
	}
}
