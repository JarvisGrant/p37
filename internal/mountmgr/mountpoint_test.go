package mountmgr_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyiwen/p37/internal/mountmgr"
)

func TestIsMountPointNonExistentAndFile(t *testing.T) {
	dir := t.TempDir()
	if mountmgr.IsMountPoint(filepath.Join(dir, "missing")) {
		t.Fatal("不存在的路径不应是挂载点")
	}
	f := filepath.Join(dir, "file")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if mountmgr.IsMountPoint(f) {
		t.Fatal("普通文件不应是挂载点")
	}
}
