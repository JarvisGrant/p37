package mountmgr_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/mountmgr"
)

func TestNewAdoptsExistingBases(t *testing.T) {
	root := t.TempDir()
	bases := filepath.Join(root, ".p37", "overlay", "bases")
	id := hash.Sum([]byte("base"))
	dir := filepath.Join(bases, id.String())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 同时造一个 .tmp 残留。
	if err := os.MkdirAll(dir+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}

	m, err := mountmgr.New(root, mountmgr.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.BaseCacheSize(); got < 5 {
		t.Fatalf("BaseCacheSize() = %d，期望已登记磁盘上的基底", got)
	}
}

func TestPruneBasesKeepsReferencedAndRemovesOrphans(t *testing.T) {
	root := t.TempDir()
	bases := filepath.Join(root, ".p37", "overlay", "bases")
	keep := hash.Sum([]byte("keep"))
	drop := hash.Sum([]byte("drop"))
	for _, id := range []hash.ID{keep, drop} {
		dir := filepath.Join(bases, id.String())
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte("0123456789"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	staleTmp := filepath.Join(bases, hash.Sum([]byte("tmp")).String()+".tmp")
	if err := os.MkdirAll(staleTmp, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(staleTmp, old, old); err != nil {
		t.Fatal(err)
	}

	m, err := mountmgr.New(root, mountmgr.Options{})
	if err != nil {
		t.Fatal(err)
	}
	removed, bytes, err := m.PruneBases(map[hash.ID]bool{keep: true})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 { // drop + stale tmp
		t.Fatalf("removed = %d，期望 2", removed)
	}
	if bytes <= 0 {
		t.Fatalf("removedBytes = %d，期望 > 0", bytes)
	}
	if _, err := os.Stat(filepath.Join(bases, keep.String())); err != nil {
		t.Fatalf("被引用的基底被误删: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bases, drop.String())); !os.IsNotExist(err) {
		t.Fatalf("孤儿基底未删除: %v", err)
	}
	if _, err := os.Stat(staleTmp); !os.IsNotExist(err) {
		t.Fatalf(".tmp 残留未删除: %v", err)
	}
}

func TestPruneBasesNilKeepRemovesAll(t *testing.T) {
	root := t.TempDir()
	bases := filepath.Join(root, ".p37", "overlay", "bases")
	id := hash.Sum([]byte("only"))
	if err := os.MkdirAll(filepath.Join(bases, id.String()), 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := mountmgr.New(root, mountmgr.Options{})
	if err != nil {
		t.Fatal(err)
	}
	removed, _, err := m.PruneBases(nil)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d，期望 1", removed)
	}
}
