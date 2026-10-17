package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/tree"
)

// fakeSource 是最小的 treeContentSource：内存里的一张 id -> 内容 表。
type fakeSource struct {
	trees map[hash.ID]*tree.Tree
	files map[hash.ID][]byte
}

func newFakeSource() *fakeSource {
	return &fakeSource{trees: map[hash.ID]*tree.Tree{}, files: map[hash.ID][]byte{}}
}

func (f *fakeSource) putTree(t *tree.Tree) hash.ID {
	t.Sort()
	id := t.ID()
	f.trees[id] = t
	return id
}

func (f *fakeSource) putFile(b []byte) hash.ID {
	id := hash.Sum(b)
	f.files[id] = b
	return id
}

func (f *fakeSource) GetTree(id hash.ID) (*tree.Tree, error) {
	t, ok := f.trees[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	return t, nil
}

func (f *fakeSource) ReadFileData(id hash.ID) ([]byte, error) {
	b, ok := f.files[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	return b, nil
}

// ReadBlob 与 ReadFileData 同源：测试里两者共用同一张表。
func (f *fakeSource) ReadBlob(id hash.ID) ([]byte, error) { return f.ReadFileData(id) }

// TestWriteTreeIntoDoesNotFollowSymlinks 是 A2 的精确回归：
// 直接对物化函数下毒，断言写入不会穿出 root。
func TestWriteTreeIntoDoesNotFollowSymlinks(t *testing.T) {
	src := newFakeSource()
	fileID := src.putFile([]byte("PAYLOAD"))
	innerID := src.putFile([]byte("inner"))
	deepID := src.putFile([]byte("deep"))

	sub := src.putTree(&tree.Tree{Entries: []tree.Entry{
		{Name: "inner.txt", Kind: codec.EntryFile, ID: innerID},
	}})
	target := src.putTree(&tree.Tree{Entries: []tree.Entry{
		{Name: "a.txt", Kind: codec.EntryFile, ID: fileID},
		{Name: "sub", Kind: codec.EntryTree, ID: sub},
		{Name: "deep", Kind: codec.EntryTree, ID: src.putTree(&tree.Tree{Entries: []tree.Entry{
			{Name: "x.txt", Kind: codec.EntryFile, ID: deepID},
		}})},
	}})

	root := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("ORIGINAL"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 在工作区里预置三种恶意条目：
	//   1. a.txt 是指向外部文件的符号链接（目标树说它是常规文件）
	//   2. sub   是指向外部目录的符号链接（目标树说它是目录）
	//   3. deep  是常规文件（目标树说它是目录）
	if err := os.Symlink(victim, filepath.Join(root, "a.txt")); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "deep"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writeTreeInto(root, mustTree(t, src, target), src); err != nil {
		t.Fatal(err)
	}

	// 1. 外部文件必须原封不动。
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ORIGINAL" {
		t.Fatalf("越权写！受害文件 = %q", got)
	}

	// 2. 外部目录里不得出现任何仓库文件。
	des, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range des {
		if d.Name() != "victim.txt" {
			t.Fatalf("越权写入外部目录: %s", d.Name())
		}
	}

	// 3. root 内的条目必须是真实类型。
	for name, wantDir := range map[string]bool{"a.txt": false, "sub": true, "deep": true} {
		fi, err := os.Lstat(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("%s 仍是符号链接", name)
		}
		if fi.IsDir() != wantDir {
			t.Fatalf("%s IsDir = %v，期望 %v", name, fi.IsDir(), wantDir)
		}
	}

	// 4. 内容正确。
	b, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "PAYLOAD" {
		t.Fatalf("a.txt 内容 = %q", b)
	}
	if b, err := os.ReadFile(filepath.Join(root, "sub", "inner.txt")); err != nil || string(b) != "inner" {
		t.Fatalf("sub/inner.txt = %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "deep", "x.txt")); err != nil || string(b) != "deep" {
		t.Fatalf("deep/x.txt = %q, %v", b, err)
	}
}

// TestWriteTreeIntoReplacesSymlinkEntry 覆盖「目标树里就是符号链接」的情形。
func TestWriteTreeIntoReplacesSymlinkEntry(t *testing.T) {
	src := newFakeSource()
	linkTargetID := src.putFile([]byte("/etc/passwd"))
	target := src.putTree(&tree.Tree{Entries: []tree.Entry{
		{Name: "l", Kind: codec.EntrySymlink, ID: linkTargetID},
	}})

	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "l")); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}

	if err := writeTreeInto(root, mustTree(t, src, target), src); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(root, "l"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("l 应为符号链接")
	}
	if got, _ := os.Readlink(filepath.Join(root, "l")); got != "/etc/passwd" {
		t.Fatalf("链接目标 = %q", got)
	}
	// 外部目录必须未被触碰。
	if des, _ := os.ReadDir(outside); len(des) != 0 {
		t.Fatalf("外部目录被写入: %v", des)
	}
}

func mustTree(t *testing.T, src *fakeSource, id hash.ID) *tree.Tree {
	t.Helper()
	tr, err := src.GetTree(id)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// TestValidWSIDGuard 覆盖 mountmgr 侧的 wsID 校验。
func TestValidWSIDGuard(t *testing.T) {
	valid := []string{"ws_0123456789abcdef", "ws_ffffffffffffffff"}
	for _, v := range valid {
		if !mountmgr.ValidWSID(v) {
			t.Errorf("%q 应合法", v)
		}
	}
	bad := []string{
		"", "ws_", "ws_0123", "ws_0123456789abcdef0",
		"ws_0123456789ABCDEF", "ws_0123456789abcde-",
		"../../etc", "ws_0123456789abcdef/../x",
	}
	for _, b := range bad {
		if mountmgr.ValidWSID(b) {
			t.Errorf("%q 应非法", b)
		}
	}
}
