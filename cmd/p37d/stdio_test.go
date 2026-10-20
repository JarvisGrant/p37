package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStdioDoesNotLockRepo 确认 --stdio 只是桥：daemon 不在时报错退出，
// 但不会创建/持有 .p37/lock，也不会自动拉起 daemon。
func TestStdioDoesNotLockRepo(t *testing.T) {
	root := initRepo(t)
	var out, errw bytes.Buffer
	err := runStdio(root, 250*time.Millisecond, strings.NewReader(""), &out, &errw)
	if err == nil {
		t.Fatal("daemon 不在时 runStdio 应返回错误")
	}
	if out.Len() != 0 {
		t.Fatalf("stdout 不应有 MCP 之外的内容: %q", out.String())
	}
	if _, serr := os.Stat(filepath.Join(root, ".p37", "lock")); serr == nil {
		t.Fatal("--stdio 不应创建仓库锁文件")
	}
	if !strings.Contains(errw.String(), "等待 daemon socket") {
		t.Fatalf("stderr 没有重试提示: %q", errw.String())
	}
}
