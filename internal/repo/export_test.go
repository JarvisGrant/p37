package repo_test

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/repo"
)

func newExportEnv(t *testing.T, opt repo.Options) (*repo.Repo, string, string) {
	t.Helper()
	root := t.TempDir()
	src := t.TempDir()
	if opt.Materialize == "" {
		opt.Materialize = mountmgr.ModeAuto
	}
	r, err := repo.Init(root, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, root, src
}

func seedFile(t *testing.T, src, rel, content string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(src, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func TestExportResultAndMetadata(t *testing.T) {
	r, _, src := newExportEnv(t, repo.Options{})
	seedFile(t, src, "a.txt", "A\n", 0o644)
	seedFile(t, src, "dir/b.txt", "B\n", 0o644)
	seedFile(t, src, "bin/run.sh", "#!/bin/sh\n", 0o755)
	if err := os.Symlink("a.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Import(src); err != nil {
		t.Fatal(err)
	}

	head, ok, err := r.Head()
	if err != nil || !ok {
		t.Fatalf("head: %v, ok=%v", err, ok)
	}
	dest := filepath.Join(t.TempDir(), "export")
	res, err := r.Export("version:"+head.String(), dest)
	if err != nil {
		t.Fatal(err)
	}
	if res.VersionID != head {
		t.Fatalf("version_id = %s，期望 %s", res.VersionID, head)
	}
	if res.Files != 4 {
		t.Fatalf("files = %d，期望 4（3 个文件 + 1 个 symlink）", res.Files)
	}
	if res.Bytes != int64(len("A\n")+len("B\n")+len("#!/bin/sh\n")+len("a.txt")) {
		t.Fatalf("bytes = %d", res.Bytes)
	}
	if res.DestDir != dest {
		t.Fatalf("dest_dir = %q", res.DestDir)
	}
	b, err := os.ReadFile(filepath.Join(dest, "dir/b.txt"))
	if err != nil || string(b) != "B\n" {
		t.Fatalf("content=%q err=%v", b, err)
	}
	fi, err := os.Lstat(filepath.Join(dest, "bin/run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&0o111 == 0 {
		t.Fatal("可执行位未保留")
	}
	target, err := os.Readlink(filepath.Join(dest, "link"))
	if err != nil || target != "a.txt" {
		t.Fatalf("symlink target=%q err=%v", target, err)
	}
	if ws, _ := r.WS.List(); len(ws) != 0 {
		t.Fatalf("repo_export 不应创建 workspace，得到 %d 个", len(ws))
	}
}

func TestExportRejectsInvalidDest(t *testing.T) {
	r, root, src := newExportEnv(t, repo.Options{})
	seedFile(t, src, "a.txt", "A\n", 0o644)
	if _, err := r.Import(src); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Export("mainline", "relative/dir"); !errors.Is(err, repo.ErrInvalidDest) {
		t.Fatalf("相对路径期望 ErrInvalidDest，得到 %v", err)
	}
	if _, err := r.Export("mainline", filepath.Join(root, "inside")); !errors.Is(err, repo.ErrInvalidDest) {
		t.Fatalf("仓库内目录期望 ErrInvalidDest，得到 %v", err)
	}

	nonEmpty := t.TempDir()
	seedFile(t, nonEmpty, "x", "x", 0o644)
	if _, err := r.Export("mainline", nonEmpty); !errors.Is(err, repo.ErrDestNotEmpty) {
		t.Fatalf("非空目录期望 ErrDestNotEmpty，得到 %v", err)
	}
}

func TestVersionFilePrefixDoesNotRequireWholeFile(t *testing.T) {
	r, _, src := newExportEnv(t, repo.Options{})
	big := strings.Repeat("x", 9<<20)
	seedFile(t, src, "big.bin", big, 0o644)
	if _, err := r.Import(src); err != nil {
		t.Fatal(err)
	}
	v, err := r.VersionFile("mainline", "big.bin", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if v.Size != int64(len(big)) {
		t.Fatalf("size = %d，期望 %d", v.Size, len(big))
	}
	if !v.Truncated {
		t.Fatal("应标记 truncated")
	}
	raw, err := base64.StdEncoding.DecodeString(v.ContentBase64)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 1<<20 {
		t.Fatalf("返回内容长度 = %d，期望 %d", len(raw), 1<<20)
	}
	if string(raw) != big[:1<<20] {
		t.Fatal("前缀内容不一致")
	}
}

func TestExportRootRestriction(t *testing.T) {
	exportRoot := t.TempDir()
	r, _, src := newExportEnv(t, repo.Options{ExportRoot: exportRoot})
	seedFile(t, src, "a.txt", "A\n", 0o644)
	if _, err := r.Import(src); err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(t.TempDir(), "outside")
	if _, err := r.Export("mainline", outside); !errors.Is(err, repo.ErrInvalidDest) {
		t.Fatalf("export_root 外期望 ErrInvalidDest，得到 %v", err)
	}
	inside := filepath.Join(exportRoot, "job-1")
	if _, err := r.Export("mainline", inside); err != nil {
		t.Fatalf("export_root 内应成功: %v", err)
	}
	if _, err := os.Stat(filepath.Join(inside, "a.txt")); err != nil {
		t.Fatal(err)
	}
}
