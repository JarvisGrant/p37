package cas

import (
	"bytes"
	"fmt"
	"testing"
)

// BenchmarkPutBlob 量化「每个对象 Flush 一次」的成本（旧行为）。
func BenchmarkPutBlob(b *testing.B) {
	bs, err := NewLocalFS(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	s := NewStore(bs, NewMemIndex())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		raw := []byte(fmt.Sprintf("object-%d", i))
		if _, err := s.PutBlob(raw); err != nil {
			b.Fatal(err)
		}
		if err := bs.Flush(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPutBlobBatch 量化批量写入（用于对照优化后的实现）。
func BenchmarkPutBlobBatch(b *testing.B) {
	bs, err := NewLocalFS(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	s := NewStore(bs, NewMemIndex())
	const batch = 200
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < batch; j++ {
			raw := []byte(fmt.Sprintf("batch-%d-%d", i, j))
			if _, err := s.PutBlob(raw); err != nil {
				b.Fatal(err)
			}
		}
		if err := bs.Flush(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPutBlobNoSync 去掉 fsync 的对照，确认瓶颈来源。
func BenchmarkPutBlobNoSync(b *testing.B) {
	bs, err := NewLocalFS(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	bs.DisableSyncForTest()
	s := NewStore(bs, NewMemIndex())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		raw := []byte(fmt.Sprintf("nosync-%d", i))
		if _, err := s.PutBlob(raw); err != nil {
			b.Fatal(err)
		}
	}
}

var _ = bytes.NewReader
