package remotesync_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/remotesync"
)

// testManifest 造一个除分片名外都合法的清单（Verify 只校验这些字段）。
func testManifest(names ...string) *remotesync.Manifest {
	const partBytes = 10
	parts := make([]remotesync.Part, 0, len(names))
	for i, n := range names {
		parts = append(parts, remotesync.Part{
			Name:    n,
			Hash:    strings.Repeat(string(rune('a'+i)), 64),
			Bytes:   partBytes,
			Objects: 1,
		})
	}
	m := &remotesync.Manifest{
		Format:        remotesync.FormatVersion,
		Kind:          "delta",
		BaseVersion:   strings.Repeat("0", 64),
		ResultVersion: strings.Repeat("1", 64),
		MetaHash:      strings.Repeat("2", 64),
		MetaBytes:     1,
		PayloadBytes:  partBytes * int64(len(names)),
		ObjectCount:   len(names),
		Parts:         parts,
	}
	m.ID = remotesync.ComputeID(m)
	return m
}

// TestManifestAcceptsFlatPartNames 断言本地生成的分片名（无子目录）都通过。
func TestManifestAcceptsFlatPartNames(t *testing.T) {
	m := testManifest("payload.pack", "payload.part000", "payload.part001")
	if err := m.Verify(); err != nil {
		t.Fatalf("合法清单被拒绝: %v", err)
	}
}

// TestManifestRejectsUnsafePartNames 是 CWE-22 的回归：分片名来自远端清单，
// 任何能跳出包目录的名字都必须在 Verify 阶段被拒绝。
func TestManifestRejectsUnsafePartNames(t *testing.T) {
	bad := []string{
		"",
		".",
		"..",
		"../escape",
		"../../../../etc/cron.d/x",
		"a/b",
		"sub/../../x",
		`a\b`,
		"/abs/path",
	}
	for _, name := range bad {
		t.Run(name, func(t *testing.T) {
			m := testManifest(name)
			if err := m.Verify(); !errors.Is(err, remotesync.ErrCorruptPackage) {
				t.Fatalf("Verify 错误 = %v，期望 ErrCorruptPackage", err)
			}
			// 解码路径（pull 实际走的入口）同样必须拒绝。
			enc, err := m.Encode()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := remotesync.DecodeManifest(enc); !errors.Is(err, remotesync.ErrCorruptPackage) {
				t.Fatalf("DecodeManifest 错误 = %v，期望 ErrCorruptPackage", err)
			}
		})
	}
}

// TestVerifyDirDoesNotFollowPartTraversal 断言校验包目录时不会读取包外的文件。
func TestVerifyDirDoesNotFollowPartTraversal(t *testing.T) {
	base := t.TempDir()
	pkgDir := filepath.Join(base, "pkg")
	if err := os.MkdirAll(pkgDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// 包目录之外放一个内容与清单哈希一致的文件。
	payload := []byte("outside-payload")
	outside := filepath.Join(base, "outside.pack")
	if err := os.WriteFile(outside, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	m := testManifest("../outside.pack")
	m.Parts[0].Hash = hash.Sum(payload).String()
	m.Parts[0].Bytes = int64(len(payload))
	m.PayloadBytes = int64(len(payload))
	m.ID = remotesync.ComputeID(m)

	enc, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, remotesync.ManifestName), enc, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := remotesync.VerifyDir(pkgDir); !errors.Is(err, remotesync.ErrCorruptPackage) {
		t.Fatalf("VerifyDir 错误 = %v，期望 ErrCorruptPackage", err)
	}
}
