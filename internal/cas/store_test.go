package cas

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/tree"
	"github.com/chenyiwen/p37/internal/version"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	bs, err := NewLocalFS(root)
	if err != nil {
		t.Fatal(err)
	}
	return NewStore(bs, NewMemIndex())
}

// TestBlobStoreContract 覆盖 BlobStore 的 S3 语义子集（§8.1）。
func TestBlobStoreContract(t *testing.T) {
	bs, err := NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// PutIfAbsent：首次成功，二次 ErrExists，内容不变。
	if err := bs.PutIfAbsent("objects/aa/bb/x", strings.NewReader("hello"), 5); err != nil {
		t.Fatalf("首次 PutIfAbsent 失败: %v", err)
	}
	if err := bs.PutIfAbsent("objects/aa/bb/x", strings.NewReader("WORLD"), 5); !errors.Is(err, ErrExists) {
		t.Fatalf("二次 PutIfAbsent 期望 ErrExists，得到 %v", err)
	}

	rc, st, err := bs.Get("objects/aa/bb/x")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "hello" {
		t.Fatalf("内容被覆盖: %q", got)
	}
	if st.Size != 5 {
		t.Fatalf("Size = %d", st.Size)
	}

	// PutIfMatch：etag 不匹配必须失败。
	if _, err := bs.PutIfMatch("refs/mainline", "deadbeef", []byte("v1")); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("错误 etag 期望 ErrPreconditionFailed，得到 %v", err)
	}
	etag, err := bs.PutIfMatch("refs/mainline", "", []byte("v1"))
	if err != nil {
		t.Fatalf("etag 为空且不存在时应创建: %v", err)
	}
	if etag != contentETag([]byte("v1")) {
		t.Fatalf("etag = %q", etag)
	}
	if _, err := bs.PutIfMatch("refs/mainline", etag, []byte("v2")); err != nil {
		t.Fatalf("etag 匹配时应成功: %v", err)
	}

	// Delete 不存在 → ErrNotExist。
	if err := bs.Delete("objects/zz/zz/none"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("删除不存在 key 期望 ErrNotExist，得到 %v", err)
	}

	// 非法 key 必须被拒绝（防路径遍历，§13）。
	for _, k := range []string{"", "/abs", "a/../b", "a//b", "a/"} {
		if _, err := bs.Stat(k); err == nil {
			t.Fatalf("非法 key %q 未被拒绝", k)
		}
	}
}

// TestShardingRoundTrip 断言对象目录分片后 ListPrefix 仍返回 S3 形式的 key。
func TestShardingRoundTrip(t *testing.T) {
	bs, err := NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore(bs, NewMemIndex())

	id, err := s.PutBlob([]byte("some content"))
	if err != nil {
		t.Fatal(err)
	}
	stats, err := bs.ListPrefix("objects/")
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 {
		t.Fatalf("ListPrefix 返回 %d 项，期望 1", len(stats))
	}
	if stats[0].Key != "objects/"+id.String() {
		t.Fatalf("key 未还原为 S3 形式: %q", stats[0].Key)
	}
}

// TestPutGetAllTypes 覆盖四种对象的写入与读回。
func TestPutGetAllTypes(t *testing.T) {
	s := newTestStore(t)

	blobRaw := []byte("chunk-bytes")
	blobID, err := s.PutBlob(blobRaw)
	if err != nil {
		t.Fatal(err)
	}
	if blobID != hash.Sum(blobRaw) {
		t.Fatal("Blob id 不是原文哈希")
	}
	got, err := s.GetBlob(blobID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, blobRaw) {
		t.Fatal("Blob 内容不符")
	}

	fc := &tree.FileContent{TotalSize: 11, EOL: codec.EOLlf, Chunks: []hash.ID{blobID}}
	fcID, err := s.PutFileContent(fc)
	if err != nil {
		t.Fatal(err)
	}
	fc2, err := s.GetFileContent(fcID)
	if err != nil {
		t.Fatal(err)
	}
	if fc2.TotalSize != fc.TotalSize || fc2.EOL != fc.EOL || len(fc2.Chunks) != 1 || fc2.Chunks[0] != blobID {
		t.Fatalf("FileContent 往返不符: %+v", fc2)
	}

	tr := &tree.Tree{Entries: []tree.Entry{
		{Name: "b.txt", Mode: codec.ModeNormal, Kind: codec.EntryFile, ID: fcID},
		{Name: "a", Mode: codec.ModeNormal, Kind: codec.EntryTree, ID: fcID},
	}}
	tid, err := s.PutTree(tr)
	if err != nil {
		t.Fatal(err)
	}
	tr2, err := s.GetTree(tid)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr2.Entries) != 2 || tr2.Entries[0].Name != "a" || tr2.Entries[1].Name != "b.txt" {
		t.Fatalf("Tree 未按字节序排序: %+v", tr2.Entries)
	}

	v := version.New(hash.Zero, tid, 0, "agent-1", "task", 1700000000000, "msg")
	vid, err := s.PutVersion(v)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := s.GetVersion(vid)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Tree != tid || v2.Agent != "agent-1" || v2.TaskHash != hash.Sum([]byte("task")) {
		t.Fatalf("Version 往返不符: %+v", v2)
	}
	if v2.ID() != vid {
		t.Fatal("Version id 不稳定")
	}

	// 类型不匹配必须报错。
	if _, err := s.GetTree(blobID); err == nil {
		t.Fatal("用 Blob id 取 Tree 应报错")
	}
}

// TestDedupNoSecondWrite 断言相同内容不会被写第二次。
func TestDedupNoSecondWrite(t *testing.T) {
	s := newTestStore(t)
	raw := []byte("duplicated content")

	id1, err := s.PutBlob(raw)
	if err != nil {
		t.Fatal(err)
	}
	stats, _ := s.BlobStore().ListPrefix("objects/")
	if len(stats) != 1 {
		t.Fatalf("写入后对象数 %d", len(stats))
	}
	id2, err := s.PutBlob(raw)
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatal("相同内容产生不同 id")
	}
	stats, _ = s.BlobStore().ListPrefix("objects/")
	if len(stats) != 1 {
		t.Fatalf("去重失败：对象数变为 %d", len(stats))
	}
}

// TestEmptyBlob 覆盖空内容边界。
func TestEmptyBlob(t *testing.T) {
	s := newTestStore(t)
	id, err := s.PutBlob(nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBlob(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("空 Blob 读回 %d 字节", len(got))
	}
	if id != hash.Sum(nil) {
		t.Fatal("空 Blob id 不符")
	}
}

// TestCorruptionDetected 断言篡改对象字节后读取会报 ErrCorrupt（§附录 L.1 #9）。
func TestCorruptionDetected(t *testing.T) {
	root := t.TempDir()
	bs, err := NewLocalFS(root)
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore(bs, NewMemIndex())

	id, err := s.PutBlob([]byte("original"))
	if err != nil {
		t.Fatal(err)
	}

	// 直接改写底层文件。
	stats, _ := bs.ListPrefix("objects/")
	if len(stats) != 1 {
		t.Fatal("找不到对象文件")
	}
	if err := bs.Delete(stats[0].Key); err != nil {
		t.Fatal(err)
	}
	if err := bs.PutIfAbsent(stats[0].Key, bytes.NewReader(encodeStored(codec.TypeBlob, []byte("tampered"))), 0); err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetBlob(id); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("篡改后期望 ErrCorrupt，得到 %v", err)
	}
}

// TestStreamFileData 断言大文件可以按分块流式写出，并且前缀读取不会
// 读取尾部数据。两者是 repo_export / version_file 避免整文件进内存的基础。
func TestStreamFileData(t *testing.T) {
	s := newTestStore(t)
	data := bytes.Repeat([]byte("0123456789abcdef"), 1024*64) // 1 MiB
	id, err := s.PutFileData(data)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	n, err := s.WriteFileData(id, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(data)) || !bytes.Equal(buf.Bytes(), data) {
		t.Fatalf("流式写出不一致: n=%d, len=%d", n, buf.Len())
	}

	prefix, total, err := s.ReadFilePrefix(id, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if total != int64(len(data)) {
		t.Fatalf("总长度 = %d，期望 %d", total, len(data))
	}
	if !bytes.Equal(prefix, data[:1024]) {
		t.Fatalf("前缀内容不一致: len=%d", len(prefix))
	}

	all, _, err := s.ReadFilePrefix(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(all, data) {
		t.Fatal("max<=0 应读取全部内容")
	}
}

// TestMissingObject 断言对象缺失返回 ErrNotExist。
func TestMissingObject(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.Get(hash.Sum([]byte("nope"))); !errors.Is(err, ErrNotExist) {
		t.Fatalf("期望 ErrNotExist，得到 %v", err)
	}
}

// TestRebuildIndex 断言索引可由 objects/ 前缀重建（§8.3）。
func TestRebuildIndex(t *testing.T) {
	root := t.TempDir()
	bs, err := NewLocalFS(root)
	if err != nil {
		t.Fatal(err)
	}

	s1 := NewStore(bs, NewMemIndex())
	if _, err := s1.PutBlob([]byte("a")); err != nil {
		t.Fatal(err)
	}
	fcID, err := s1.PutFileContent(&tree.FileContent{TotalSize: 1, EOL: codec.EOLlf, Chunks: []hash.ID{hash.Sum([]byte("a"))}})
	if err != nil {
		t.Fatal(err)
	}

	// 换一个空索引，模拟索引丢失后重建。
	idx2 := NewMemIndex()
	s2 := NewStore(bs, idx2)
	if n, err := s2.RebuildIndex(); err != nil || n != 2 {
		t.Fatalf("RebuildIndex 返回 (%d, %v)，期望 (2, nil)", n, err)
	}
	got, err := s2.GetFileContent(fcID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalSize != 1 {
		t.Fatalf("重建后读取失败: %+v", got)
	}
}

// TestCompressionDoesNotAffectID 断言压缩不参与对象 id（附录 H.6 不变量 3）。
func TestCompressionDoesNotAffectID(t *testing.T) {
	raw := bytes.Repeat([]byte("compressible "), 1000)
	if hash.Sum(raw) != hash.Sum(raw) {
		t.Fatal("哈希不稳定")
	}
	s := newTestStore(t)
	id, err := s.PutBlob(raw)
	if err != nil {
		t.Fatal(err)
	}
	if id != hash.Sum(raw) {
		t.Fatal("id 受压缩影响")
	}
	// 存储体量应显著小于原文（证明确实压缩了）。
	stats, _ := s.BlobStore().ListPrefix("objects/")
	if len(stats) != 1 {
		t.Fatal("对象数不符")
	}
	if stats[0].Size >= int64(len(raw)) {
		t.Fatalf("存储大小 %d 未小于原文 %d，压缩未生效", stats[0].Size, len(raw))
	}
}
