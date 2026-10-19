package remotesync_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/remotesync"
	"github.com/chenyiwen/p37/internal/repo"
	"github.com/chenyiwen/p37/internal/workspace"
)

func newRepo(t *testing.T) (*repo.Repo, string) {
	t.Helper()
	root := t.TempDir()
	r, err := repo.Init(root, repo.Options{Materialize: mountmgr.ModeCopy})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, root
}

func importFiles(t *testing.T, r *repo.Repo, files map[string]string) {
	t.Helper()
	src := t.TempDir()
	for rel, content := range files {
		writeFile(t, filepath.Join(src, rel), content)
	}
	if _, err := r.Import(src); err != nil {
		t.Fatal(err)
	}
}

func land(t *testing.T, r *repo.Repo, task string, files map[string]string) hash.ID {
	t.Helper()
	w, err := r.WS.Open(workspace.OpenRequest{TaskDesc: task})
	if err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		writeFile(t, filepath.Join(r.WS.Dir(w.ID), rel), content)
	}
	if err := r.WS.Sync(w.ID); err != nil {
		t.Fatal(err)
	}
	res, err := r.Land(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	return res.VersionID
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readAt(t *testing.T, r *repo.Repo, ver hash.ID, rel string) string {
	t.Helper()
	v, err := r.DB.GetVersion(ver)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := r.Store.GetTree(v.Tree)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := tr.Get(rel)
	if !ok {
		t.Fatalf("版本 %s 缺少 %s", ver, rel)
	}
	b, err := r.Store.ReadFileData(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func opts(t *testing.T) remotesync.Options {
	return remotesync.Options{
		Outbox:          filepath.Join(t.TempDir(), "outbox"),
		MinVersions:     1,
		MinBytes:        1,
		CompressLevel:   9,
		MaxPackageBytes: 1 << 20,
	}
}

// TestManifestCorruption 覆盖清单/分片被篡改。
func TestManifestCorruption(t *testing.T) {
	r, _ := newRepo(t)
	importFiles(t, r, map[string]string{"a.txt": "hello\n"})
	o := opts(t)
	m, err := remotesync.Prepare(r, o, true, false)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(o.Outbox, m.ID)
	if _, err := remotesync.VerifyDir(dir); err != nil {
		t.Fatalf("合法包校验失败: %v", err)
	}

	// 篡改 payload。
	p := filepath.Join(dir, m.Parts[0].Name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)/2] ^= 0xff
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := remotesync.VerifyDir(dir); !errors.Is(err, remotesync.ErrCorruptPackage) {
		t.Fatalf("篡改 payload 应报 ErrCorruptPackage，得到 %v", err)
	}
}

// TestCheckpointPushFetch 覆盖首次 checkpoint 推送 + 空仓库拉取。
func TestCheckpointPushFetch(t *testing.T) {
	a, _ := newRepo(t)
	importFiles(t, a, map[string]string{
		"a.txt":    "v0\n",
		"sub/b.go": "package b\n",
	})
	for i := 0; i < 3; i++ {
		land(t, a, fmt.Sprintf("改 %d", i), map[string]string{
			"a.txt": fmt.Sprintf("v%d\n", i+1),
		})
	}
	o := opts(t)
	m, err := remotesync.Prepare(a, o, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != "checkpoint" {
		t.Fatalf("首次推送应为 checkpoint，得到 %s", m.Kind)
	}
	if m.BaseVersion != hash.Zero.String() {
		t.Fatalf("checkpoint base 应为全零，得到 %s", m.BaseVersion)
	}

	remoteDir := t.TempDir()
	remote, err := remotesync.NewDirRemote(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remotesync.Push(context.Background(), a, remote, nil); err != nil {
		t.Fatal(err)
	}
	ref, err := remote.GetRef(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ref.Head != m.ResultVersion {
		t.Fatalf("远端 head = %s，期望 %s", ref.Head, m.ResultVersion)
	}

	// 空仓库拉取。
	b, _ := newRepo(t)
	res, err := remotesync.Pull(context.Background(), b, remote, opts(t))
	if err != nil {
		t.Fatal(err)
	}
	if !res.FastForward {
		t.Fatalf("应快进: %+v", res)
	}
	head, _, _ := b.Head()
	if head.String() != m.ResultVersion {
		t.Fatalf("拉取后 head = %s", head)
	}
	if got := readAt(t, b, head, "a.txt"); got != "v3\n" {
		t.Fatalf("a.txt = %q", got)
	}
	// 历史与 blame 可用。
	chain, err := b.DB.VersionChain(head)
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) < 4 {
		t.Fatalf("版本链长度 = %d，期望 ≥4", len(chain))
	}
	// 空仓库的 p37-init 不应残留在历史上。
	logs, err := b.Log(20, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range logs {
		if v.Agent == "p37-init" {
			t.Fatalf("拉取后仍残留 init 版本: %+v", v)
		}
	}
	if _, err := b.Blame("a.txt", repo.BlameOptions{Granularity: "line"}); err != nil {
		t.Fatalf("blame: %v", err)
	}
}

// TestDeltaPushAndDivergence 覆盖后续 delta 推送与分叉处理。
func TestDeltaPushAndDivergence(t *testing.T) {
	a, _ := newRepo(t)
	importFiles(t, a, map[string]string{"a.txt": "0\n"})
	land(t, a, "v1", map[string]string{"a.txt": "1\n"})

	o := opts(t)
	remote, err := remotesync.NewDirRemote(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := remotesync.Prepare(a, o, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remotesync.Push(context.Background(), a, remote, nil); err != nil {
		t.Fatal(err)
	}

	b, _ := newRepo(t)
	if _, err := remotesync.Pull(context.Background(), b, remote, opts(t)); err != nil {
		t.Fatal(err)
	}

	// A 再走两步 -> delta。
	land(t, a, "v2", map[string]string{"a.txt": "2\n"})
	land(t, a, "v3", map[string]string{"b.txt": "new\n"})
	second, err := remotesync.Prepare(a, o, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if second.Kind != "delta" {
		t.Fatalf("第二次应为 delta，得到 %s", second.Kind)
	}
	if second.BaseVersion != first.ResultVersion {
		t.Fatalf("delta base = %s，期望 %s", second.BaseVersion, first.ResultVersion)
	}
	if _, err := remotesync.Push(context.Background(), a, remote, nil); err != nil {
		t.Fatal(err)
	}
	res, err := remotesync.Pull(context.Background(), b, remote, opts(t))
	if err != nil {
		t.Fatal(err)
	}
	if !res.FastForward {
		t.Fatalf("应快进: %+v", res)
	}
	if got := readAt(t, b, mustHead(t, b), "b.txt"); got != "new\n" {
		t.Fatalf("b.txt = %q", got)
	}

	// 制造分叉：B 本地 land，A 再 land 并推送。
	land(t, b, "B 本地", map[string]string{"b.txt": "from B\n"})
	land(t, a, "A 再改", map[string]string{"a.txt": "3\n"})
	if _, err := remotesync.Prepare(a, o, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := remotesync.Push(context.Background(), a, remote, nil); err != nil {
		t.Fatal(err)
	}
	before := mustHead(t, b)
	res, err = remotesync.Pull(context.Background(), b, remote, opts(t))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Diverged {
		t.Fatalf("应检测到分叉: %+v", res)
	}
	if mustHead(t, b) != before {
		t.Fatal("分叉时不应移动本地 head")
	}
	if res.Behind == 0 || res.Ahead == 0 {
		t.Fatalf("ahead/behind 异常: %+v", res)
	}
}

// TestPushRejectsMovedRemote 覆盖远端被别人推进时的拒绝。
func TestPushRejectsMovedRemote(t *testing.T) {
	a, _ := newRepo(t)
	importFiles(t, a, map[string]string{"a.txt": "0\n"})
	land(t, a, "v1", map[string]string{"a.txt": "1\n"})
	o := opts(t)
	remote, _ := remotesync.NewDirRemote(t.TempDir())
	if _, err := remotesync.Prepare(a, o, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := remotesync.Push(context.Background(), a, remote, nil); err != nil {
		t.Fatal(err)
	}

	// 另一个仓库往同一远端推了一把（模拟并发写者）。
	b, _ := newRepo(t)
	if _, err := remotesync.Pull(context.Background(), b, remote, opts(t)); err != nil {
		t.Fatal(err)
	}
	land(t, b, "b1", map[string]string{"b.txt": "b\n"})
	if _, err := remotesync.Prepare(b, o, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := remotesync.Push(context.Background(), b, remote, nil); err != nil {
		t.Fatal(err)
	}

	// A 基于旧的远端 head 打包 -> push 必须被拒。
	land(t, a, "a2", map[string]string{"a.txt": "2\n"})
	if _, err := remotesync.Prepare(a, o, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := remotesync.Push(context.Background(), a, remote, nil); !errors.Is(err, remotesync.ErrRemoteMoved) {
		t.Fatalf("期望 ErrRemoteMoved，得到 %v", err)
	}
}

// TestCompressionAndSplit 覆盖重压与分片。
func TestCompressionAndSplit(t *testing.T) {
	a, _ := newRepo(t)
	files := map[string]string{}
	for i := 0; i < 200; i++ {
		files[fmt.Sprintf("d/f%03d.txt", i)] = strings.Repeat(fmt.Sprintf("line %d\n", i), 50)
	}
	importFiles(t, a, files)
	o := opts(t)
	o.MaxPackageBytes = 2 << 10 // 强制切分
	m, err := remotesync.Prepare(a, o, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Parts) < 2 {
		t.Fatalf("应切成多个分片，得到 %d", len(m.Parts))
	}
	if m.PayloadBytes >= m.RawBytes {
		t.Fatalf("压缩后不应更大: payload=%d raw=%d", m.PayloadBytes, m.RawBytes)
	}
	if _, err := remotesync.VerifyDir(filepath.Join(o.Outbox, m.ID)); err != nil {
		t.Fatal(err)
	}
}

func mustHead(t *testing.T, r *repo.Repo) hash.ID {
	t.Helper()
	h, ok, err := r.Head()
	if err != nil || !ok {
		t.Fatalf("无 head: %v", err)
	}
	return h
}
