package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/repo"
	"github.com/chenyiwen/p37/internal/repolock"
)

func TestMain(m *testing.M) {
	// 开发机/CI 的 temp 目录不是挂载点；生产校验仍默认开启。
	_ = os.Setenv("P37_ALLOW_NONPERSISTENT", "1")
	os.Exit(m.Run())
}

func runCLI(args ...string) (int, string, string) {
	var out, errw bytes.Buffer
	code := run(args, &out, &errw)
	return code, out.String(), errw.String()
}

func TestInitIfMissingDoesNotOverwriteConfig(t *testing.T) {
	root := t.TempDir()
	if code, _, errs := runCLI("init", root); code != 0 {
		t.Fatalf("init 失败: %d %s", code, errs)
	}
	cfgPath := config.Path(root)
	original := []byte("# user modified\n[workspace]\nbase_cache_bytes = 12345\nmax_active = 3\n")
	if err := os.WriteFile(cfgPath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	code, out, errs := runCLI("init", root, "--if-missing")
	if code != 0 {
		t.Fatalf("--if-missing 应返回 0，得到 %d: %s", code, errs)
	}
	if !strings.Contains(out, "跳过初始化") {
		t.Fatalf("输出没有说明跳过: %q", out)
	}
	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("--if-missing 覆盖了 config.toml\nwant: %s\ngot:  %s", original, got)
	}
}

func TestInitConfigTemplate(t *testing.T) {
	root := t.TempDir()
	tpl := filepath.Join(t.TempDir(), "p36.toml")
	if err := os.WriteFile(tpl, []byte(`
[workspace]
materialize = "copy"
base_cache_bytes = 4567
warn_disk_bytes = 8901
max_active = 2
`), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, errs := runCLI("init", root, "--config", tpl)
	if code != 0 {
		t.Fatalf("init --config 失败: %d %s", code, errs)
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workspace.BaseCacheBytes != 4567 ||
		cfg.Workspace.WarnDiskBytes != 8901 ||
		cfg.Workspace.MaxActive != 2 {
		t.Fatalf("模板未生效: %+v", cfg.Workspace)
	}
	if cfg.MountMode() != "copy" {
		t.Fatalf("materialize 未生效: %q", cfg.Workspace.Materialize)
	}
}

func TestInitIfMissingRefusesPartialRepo(t *testing.T) {
	root := t.TempDir()
	cfgPath := config.Path(root)
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	original := []byte("[workspace]\nmax_active = 3\n")
	if err := os.WriteFile(cfgPath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, errs := runCLI("init", root, "--if-missing")
	if code == 0 {
		t.Fatal("半初始化仓库不应返回 0")
	}
	if !strings.Contains(errs, "仓库不完整") {
		t.Fatalf("错误信息不清楚: %q", errs)
	}
	got, _ := os.ReadFile(cfgPath)
	if !bytes.Equal(got, original) {
		t.Fatal("半初始化仓库的 config 被覆盖")
	}
}

func TestGCRefusesWhenDaemonLockHeld(t *testing.T) {
	root := t.TempDir()
	if code, _, errs := runCLI("init", root); code != 0 {
		t.Fatalf("init 失败: %s", errs)
	}
	lock, err := repolock.TryExclusive(filepath.Join(root, ".p37", "lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()

	code, out, errs := runCLI("gc", root, "--prune")
	if code == 0 {
		t.Fatal("daemon 持锁时 gc 应拒绝")
	}
	combined := out + errs
	if !strings.Contains(combined, "GC 已拒绝") ||
		!strings.Contains(combined, "--force") {
		t.Fatalf("拒绝信息不清楚: %q", combined)
	}
}

func TestGCForceBypassesDaemonLock(t *testing.T) {
	root := t.TempDir()
	if code, _, errs := runCLI("init", root); code != 0 {
		t.Fatalf("init 失败: %s", errs)
	}
	lock, err := repolock.TryExclusive(filepath.Join(root, ".p37", "lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()

	code, _, errs := runCLI("gc", root, "--force")
	if code != 0 {
		t.Fatalf("--force 应允许继续，得到 %d: %s", code, errs)
	}
	if !strings.Contains(errs, "警告") {
		t.Fatalf("--force 应打印警告: %q", errs)
	}
}

func TestRepoFormatV2(t *testing.T) {
	root := t.TempDir()
	if code, _, errs := runCLI("init", root); code != 0 {
		t.Fatalf("init 失败: %s", errs)
	}
	db, err := meta.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	v, err := db.UserVersion()
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if v != 2 {
		t.Fatalf("仓库格式版本 = %d，期望 2", v)
	}
	if code, _, errs := runCLI("fsck", root); code != 0 {
		t.Fatalf("fsck 失败: %d %s", code, errs)
	}
}

func TestVersionIsParseable(t *testing.T) {
	code, out, errs := runCLI("--version")
	if code != 0 {
		t.Fatalf("--version 失败: %s", errs)
	}
	for _, key := range []string{"version=", "commit=", "buildTime=", "goVersion=", "formatVer="} {
		if !strings.Contains(out, key) {
			t.Errorf("版本输出缺少 %s: %q", key, out)
		}
	}
}

func TestInitImportAndRepairFlags(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := runCLI("init", root); code == 0 {
		t.Fatalf("非空目录 mode=empty 应失败: %s", errs)
	}
	if code, _, errs := runCLI("init", root, "--mode=import"); code != 0 {
		t.Fatalf("mode=import 失败: %s", errs)
	}
	if code, _, errs := runCLI("init", root, "--strict"); code != 0 {
		t.Fatalf("完整仓库 --strict 失败: %s", errs)
	}

	// 人为制造半成品：删 line 与 refs。
	db, err := meta.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteLine("main", 1); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, ".p37", "refs", "mainline")); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runCLI("init", root, "--strict"); code == 0 {
		t.Fatal("--strict 不应修复半成品")
	}
	if code, _, _ := runCLI("init", root, "--no-repair"); code == 0 {
		t.Fatal("--no-repair 不应修复半成品")
	}
	// 半成品 + 用户文件：empty/repair 不能静默修；必须显式 import。
	if code, _, _ := runCLI("init", root, "--if-missing"); code == 0 {
		t.Fatal("半成品 + 用户文件时 --if-missing 应返回 repo_not_empty")
	}
	if code, _, errs := runCLI("init", root, "--mode=import"); code != 0 {
		t.Fatalf("--mode=import 应修复/对齐 v0: %s", errs)
	}
	in, err := repo.CheckIntegrity(root)
	if err != nil || !in.Initialized {
		t.Fatalf("repair 后 integrity=%+v err=%v", in, err)
	}
}

func TestInitAcceptsPrecreatedEmptyControlDir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".p37"), 0o700); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := runCLI("init", root); code != 0 {
		t.Fatalf("旧 P36 预建空 .p37 应可初始化: %s", errs)
	}
}
