package repolock_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/chenyiwen/p37/internal/repolock"
)

func TestTryExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")

	l1, err := repolock.TryExclusive(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repolock.TryExclusive(path); !errors.Is(err, repolock.ErrBusy) {
		t.Fatalf("第二次取锁应返回 ErrBusy，得到 %v", err)
	}
	if err := l1.Release(); err != nil {
		t.Fatal(err)
	}

	l2, err := repolock.TryExclusive(path)
	if err != nil {
		t.Fatalf("释放后应能重新取锁: %v", err)
	}
	_ = l2.Release()
}
