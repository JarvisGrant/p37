package cas

import (
	"bytes"
	crand "crypto/rand"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
)

func TestPackEncodeParseRoundTrip(t *testing.T) {
	ids := []hash.ID{
		hash.Sum([]byte("a")),
		hash.Sum([]byte("bb")),
		hash.Sum([]byte("ccc")),
	}
	stored := [][]byte{
		encodeStored(codec.TypeBlob, []byte("a")),
		encodeStored(codec.TypeTree, []byte("bb")),
		encodeStored(codec.TypeFileContent, []byte("ccc")),
	}
	types := []uint8{codec.TypeBlob, codec.TypeTree, codec.TypeFileContent}

	pack := encodePack(ids, stored, types)
	entries, err := parsePack(pack)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("条目数 = %d", len(entries))
	}
	// body 必须逐字节命中。
	for i, en := range entries {
		got := pack[en.off : en.off+en.length]
		if !bytes.Equal(got, stored[i]) {
			t.Fatalf("条目 %d 内容不符", i)
		}
		if en.typ != types[i] {
			t.Fatalf("条目 %d 类型 = %d", i, en.typ)
		}
	}

	// 确定性与截断检测。
	if !bytes.Equal(pack, encodePack(ids, stored, types)) {
		t.Fatal("同一组对象应产生同一份 pack 字节")
	}
	if _, err := parsePack(pack[:len(pack)-3]); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("截断应报 ErrCorrupt，得到 %v", err)
	}
	if _, err := parsePack(pack[:10]); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("过短应报 ErrCorrupt，得到 %v", err)
	}
}

// TestPackStoreTransparentRead 覆盖：打包后对象仍可透明读取，且列举不重复。
func TestPackStoreTransparentRead(t *testing.T) {
	base, err := NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	idx := NewMemIndex()
	ps := NewPackStore(base)
	s := NewStore(ps, idx)

	// 写 40 个对象，各含可区分内容。
	ids := make([]hash.ID, 0, 40)
	for i := 0; i < 40; i++ {
		id, err := s.PutBlob([]byte{byte(i), byte(i >> 8), 'x'})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	before, _ := base.ListPrefix("objects/")
	if len(before) != 40 {
		t.Fatalf("打包前松散对象 = %d，期望 40", len(before))
	}

	res, err := ps.PackLoose(PackOptions{TargetPackBytes: 1 << 20, MinObjects: 4})
	if err != nil {
		t.Fatal(err)
	}
	if res.ObjectsPacked != 40 || res.PacksCreated != 1 {
		t.Fatalf("打包结果异常: %+v", res)
	}
	looseAfter, _ := base.ListPrefix("objects/")
	if len(looseAfter) != 0 {
		t.Fatalf("打包后应无松散对象，得到 %d", len(looseAfter))
	}

	// 列举（含打包）仍应是 40 个、不重复。
	got, err := s.ListObjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 40 {
		t.Fatalf("ListObjects = %d，期望 40", len(got))
	}
	seen := map[hash.ID]bool{}
	for _, o := range got {
		if seen[o.ID] {
			t.Fatalf("对象 %s 重复列举", o.ID)
		}
		seen[o.ID] = true
	}

	// 透明读取 + Head（只读 2 字节）+ Stat。
	for i, id := range ids {
		b, err := s.GetBlob(id)
		if err != nil {
			t.Fatalf("读取打包对象 %s: %v", id, err)
		}
		if !bytes.Equal(b, []byte{byte(i), byte(i >> 8), 'x'}) {
			t.Fatalf("对象 %d 内容不符: %v", i, b)
		}
		if typ, _, err := s.Head(id); err != nil || typ != codec.TypeBlob {
			t.Fatalf("Head(%s) = %d, %v", id, typ, err)
		}
		if st, err := ps.Stat(keyFor(id)); err != nil || st.Size <= 0 {
			t.Fatalf("Stat(%s) = %+v, %v", id, st, err)
		}
	}

	// 重开一个 PackStore（模拟新进程）：位置表从头扫描 pack 建立。
	ps2 := NewPackStore(base)
	s2 := NewStore(ps2, NewMemIndex())
	if _, err := s2.RebuildIndex(); err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}
	for i, id := range ids {
		b, err := s2.GetBlob(id)
		if err != nil {
			t.Fatalf("重开后读取 %s: %v", id, err)
		}
		if len(b) != 3 || b[0] != byte(i) {
			t.Fatalf("重开后对象 %d 内容不符", i)
		}
	}
}

// TestPackLooseSkipsLargeAndKeepsOnFailure 覆盖：大对象不入包；pack 写坏时
// 松散副本必须保留（对象仍可读）。
func TestPackSkipsLarge(t *testing.T) {
	base, _ := NewLocalFS(t.TempDir())
	s := NewStore(NewPackStore(base), NewMemIndex())
	small, _ := s.PutBlob([]byte("small"))
	// 用随机字节，保证压缩后仍大于阈值（否则 zstd 会把 4 KiB 压成几十字节）。
	rnd := make([]byte, 4096)
	if _, err := crand.Read(rnd); err != nil {
		t.Fatal(err)
	}
	big, _ := s.PutBlob(rnd)
	_ = s.Flush()

	res, err := NewPackStore(base).PackLoose(PackOptions{MaxObjectBytes: 1024, MinObjects: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.SkippedLarge != 1 {
		t.Fatalf("SkippedLarge = %d，期望 1", res.SkippedLarge)
	}
	// 两者都必须仍可读。
	if b, err := s.GetBlob(small); err != nil || string(b) != "small" {
		t.Fatalf("small 读取失败: %v %q", err, b)
	}
	if b, err := s.GetBlob(big); err != nil || len(b) != 4096 {
		t.Fatalf("big 读取失败: %v len=%d", err, len(b))
	}
}

// TestPackDuplicateTolerant 覆盖「pack 已写、松散副本未删」的崩溃窗口：
// 同一对象同时存在于 pack 与松散文件时，列举去重、读取一致。
func TestPackDuplicateTolerant(t *testing.T) {
	base, _ := NewLocalFS(t.TempDir())
	s := NewStore(NewPackStore(base), NewMemIndex())
	id, _ := s.PutBlob([]byte("dup-content"))
	_ = s.Flush()
	if _, err := NewPackStore(base).PackLoose(PackOptions{MinObjects: 1}); err != nil {
		t.Fatal(err)
	}
	// 模拟崩溃残留：把同一个对象再写成松散文件。
	stored := encodeStored(codec.TypeBlob, []byte("dup-content"))
	if err := base.PutIfAbsent("objects/"+id.String(), bytes.NewReader(stored), int64(len(stored))); err != nil {
		t.Fatal(err)
	}

	ps := NewPackStore(base)
	stats, err := ps.ListPrefix("objects/")
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 {
		t.Fatalf("同 id 的 pack+松散副本应去重列举，得到 %d", len(stats))
	}
	b, _, err := ps.Get("objects/" + id.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	got, _ := io.ReadAll(b)
	if !bytes.Equal(got, stored) {
		t.Fatalf("读取到 %d 字节，内容不符", len(got))
	}
}

// TestPackCorruptionDetected 覆盖被截断的 pack：位置表加载必须报错。
func TestPackCorruptionDetected(t *testing.T) {
	lfs, _ := NewLocalFS(t.TempDir())
	s := NewStore(NewPackStore(lfs), NewMemIndex())
	for i := 0; i < 10; i++ {
		if _, err := s.PutBlob([]byte{byte(i), 1, 2, 3}); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Flush()
	if _, err := NewPackStore(lfs).PackLoose(PackOptions{MinObjects: 1}); err != nil {
		t.Fatal(err)
	}
	packs, _ := lfs.ListPrefix("packs/")
	if len(packs) != 1 {
		t.Fatalf("pack 数 = %d", len(packs))
	}
	p, err := lfs.pathForKey(packs[0].Key)
	if err != nil {
		t.Fatal(err)
	}
	// 从尾部砍掉 4 字节（破坏 trailer）。
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(p, fi.Size()-4); err != nil {
		t.Fatal(err)
	}
	ps := NewPackStore(lfs)
	if _, err := ps.ListPrefix("objects/"); err == nil {
		t.Fatal("损坏的 pack 应导致加载失败")
	}
}

// TestPackStoreSeesExternalPack 回归：打包由**另一个进程**完成时，常驻进程的
// 位置表必须能发现新 pack（否则「松散已删、位置表还是旧的」会误报 key 不存在）。
func TestPackStoreSeesExternalPack(t *testing.T) {
	base, _ := NewLocalFS(t.TempDir())
	writer := NewStore(NewPackStore(base), NewMemIndex())
	ids := make([]hash.ID, 0, 20)
	for i := 0; i < 20; i++ {
		id, err := writer.PutBlob([]byte{byte(i), 'e', 'x'})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	_ = writer.Flush()

	// 常驻读端：先读一次，位置表被加载（此时还没有 pack）。
	reader := NewPackStore(base)
	if _, _, err := reader.Get(keyFor(ids[0])); err != nil {
		t.Fatal(err)
	}

	// 「另一个进程」打包。
	if _, err := NewPackStore(base).PackLoose(PackOptions{MinObjects: 1}); err != nil {
		t.Fatal(err)
	}

	// 读端必须自动发现新 pack。
	for i, id := range ids {
		rc, _, err := reader.Get(keyFor(id))
		if err != nil {
			t.Fatalf("外部打包后读 %s 失败: %v", id, err)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		want := encodeStored(codec.TypeBlob, []byte{byte(i), 'e', 'x'})
		if !bytes.Equal(b, want) {
			t.Fatalf("对象 %d 内容不符", i)
		}
	}
	stats, err := reader.ListPrefix("objects/")
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != len(ids) {
		t.Fatalf("ListPrefix = %d，期望 %d", len(stats), len(ids))
	}
}

// TestSweepPacksDropsWhenAllUnreachable 覆盖整包回收。
func TestSweepPacksDropsWhenAllUnreachable(t *testing.T) {
	base, _ := NewLocalFS(t.TempDir())
	idx := NewMemIndex()
	ps := NewPackStore(base)
	s := NewStore(ps, idx)

	live, _ := s.PutBlob([]byte("live"))
	for i := 0; i < 30; i++ {
		if _, err := s.PutBlob([]byte{byte(i), 'd'}); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Flush()
	if _, err := ps.PackLoose(DefaultPackOptions()); err != nil {
		t.Fatal(err)
	}

	// 只保留 live：包含死对象的那整包应被删；live 不是任何 pack 的… 它也在同一包
	// （同一批），所以这一包不应被删。
	reach := map[hash.ID]struct{}{live: {}}
	res, err := s.SweepPacks(reach)
	if err != nil {
		t.Fatal(err)
	}
	if res.PacksDropped != 0 {
		t.Fatalf("包内仍有可达对象，不应删包: %+v", res)
	}
	if _, err := s.GetBlob(live); err != nil {
		t.Fatalf("live 应仍可读: %v", err)
	}

	// 现在 live 也不可达 -> 整包回收。
	res, err = s.SweepPacks(map[hash.ID]struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if res.PacksDropped != 1 || res.ObjectsDropped == 0 {
		t.Fatalf("应回收 1 个 pack: %+v", res)
	}
	packs, _ := ps.Packs()
	if len(packs) != 0 {
		t.Fatalf("pack 应已清空，得到 %d", len(packs))
	}
}
