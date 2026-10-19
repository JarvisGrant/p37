package cas

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"

	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/tree"
	"github.com/chenyiwen/p37/internal/version"
)

// MaxObjectRawSize 是单个对象解压后的字节上限（§13：防解压炸弹）。
const MaxObjectRawSize = 64 << 20

// zstdEnc 是共享的编码器。EncodeAll 可并发调用。
var zstdEnc, _ = zstd.NewWriter(nil,
	zstd.WithEncoderLevel(zstd.SpeedDefault),
	zstd.WithEncoderConcurrency(1))

// zstdDec 是共享的解码器。
var zstdDec, _ = zstd.NewReader(nil,
	zstd.WithDecoderConcurrency(1),
	zstd.WithDecoderMaxMemory(MaxObjectRawSize*2))

// contentETag 返回内容的 BLAKE3 十六进制（用于非对象 key，如 refs）。
func contentETag(b []byte) string { return hash.Sum(b).String() }

// ObjectIndex 记录本地存在哪些对象及其类型。
//
// 它**只是缓存**：对象在存储中的形态自带类型（见 Store 的存储格式说明），
// 因此索引可以随时由 objects/ 前缀重建（§8.3、§10）。
type ObjectIndex interface {
	Has(id hash.ID) (bool, error)
	Type(id hash.ID) (uint8, error)
	Add(id hash.ID, typ uint8, size, rawSize int64) error
	Remove(id hash.ID) error
	Iterate(fn func(id hash.ID, typ uint8, size, rawSize int64) error) error
}

// Store 是带类型的对象存储，在 BlobStore 之上实现：
//
//	id      = BLAKE3_256(raw content)                ← 内容寻址，与压缩无关
//	key     = "objects/" + hex(id)                   ← S3 语义的不可变 key
//	stored  = 0x01 || type || zstd(raw)              ← 自描述，索引可重建
//
// 存储形态逐字节确定，因此同一对象在任何端产生同一份字节；type 前缀使
// ObjectIndex 成为纯缓存，崩溃后可由扫描 objects/ 重建 —— 与「ops 表可重建」
// 同一条设计哲学（§1.2、§5.4）。
type Store struct {
	bs  BlobStore
	idx ObjectIndex
}

// NewStore 组合 BlobStore 与 ObjectIndex。
func NewStore(bs BlobStore, idx ObjectIndex) *Store {
	return &Store{bs: bs, idx: idx}
}

// BlobStore 返回底层后端。
func (s *Store) BlobStore() BlobStore { return s.bs }

// Flush 确保此前写入的全部对象已持久化。
//
// 调用方**必须**在推进主干 head 之前调用它（§7.3）。
func (s *Store) Flush() error { return s.bs.Flush() }

// keyFor 返回对象的 BlobStore key。
func keyFor(id hash.ID) string { return "objects/" + id.String() }

// storedHeader 是对象存储的前缀：0x01 || type。
const storedFormat = 0x01

// encodeStored 把 raw 编成自描述的存储字节。
func encodeStored(typ uint8, raw []byte) []byte {
	comp := zstdEnc.EncodeAll(raw, make([]byte, 0, len(raw)/2+16))
	out := make([]byte, 0, len(comp)+2)
	out = append(out, storedFormat, typ)
	return append(out, comp...)
}

// decodeStored 解开存储字节并返回 (type, raw)。
func decodeStored(b []byte) (uint8, []byte, error) {
	if len(b) < 3 {
		return 0, nil, fmt.Errorf("cas: 对象字节过短 (%d)", len(b))
	}
	if b[0] != storedFormat {
		return 0, nil, fmt.Errorf("cas: 未知存储格式标记 0x%02x", b[0])
	}
	typ := b[1]
	if typ < codec.TypeBlob || typ > codec.TypeVersion {
		return 0, nil, fmt.Errorf("cas: 未知对象类型 0x%02x", typ)
	}
	raw, err := zstdDec.DecodeAll(b[2:], make([]byte, 0, 4096))
	if err != nil {
		return 0, nil, fmt.Errorf("cas: 解压失败: %w", err)
	}
	if len(raw) > MaxObjectRawSize {
		return 0, nil, fmt.Errorf("cas: 解压后 %d 字节超过上限 %d", len(raw), MaxObjectRawSize)
	}
	return typ, raw, nil
}

// put 写入一个对象并登记索引，返回其 id。
func (s *Store) put(typ uint8, raw []byte) (hash.ID, error) {
	id := hash.Sum(raw)

	if ok, err := s.idx.Has(id); err != nil {
		return hash.ID{}, err
	} else if ok {
		return id, nil // 去重快路径
	}

	stored := encodeStored(typ, raw)
	key := keyFor(id)
	err := s.bs.PutIfAbsent(key, bytes.NewReader(stored), int64(len(stored)))
	if err != nil && !errors.Is(err, ErrExists) {
		return hash.ID{}, err
	}
	// 索引在对象落盘之后登记；即使此处崩溃，索引也可由扫描重建。
	if err := s.idx.Add(id, typ, int64(len(stored)), int64(len(raw))); err != nil {
		return hash.ID{}, err
	}
	return id, nil
}

// PutBlob 写入一个文件分片的**原文**（未压缩）。
//
// 注意：id 由原文算出，压缩参数不参与 —— 修改 zstd 级别不构成格式破坏
// （附录 H.6 不变量 3）。
func (s *Store) PutBlob(raw []byte) (hash.ID, error) {
	return s.put(codec.TypeBlob, raw)
}

// PutFileContent 写入 FileContent 对象。
func (s *Store) PutFileContent(fc *tree.FileContent) (hash.ID, error) {
	enc := fc.Encode()
	return s.put(codec.TypeFileContent, enc)
}

// PutTree 写入 Tree 对象。
func (s *Store) PutTree(t *tree.Tree) (hash.ID, error) {
	enc := t.Encode()
	return s.put(codec.TypeTree, enc)
}

// PutVersion 写入 Version 对象。
func (s *Store) PutVersion(v *version.Version) (hash.ID, error) {
	enc := v.Encode()
	return s.put(codec.TypeVersion, enc)
}

// Has 报告对象是否已在本地。
func (s *Store) Has(id hash.ID) (bool, error) { return s.idx.Has(id) }

// Get 读取对象原文与类型。对象缺失或校验失败返回 ErrNotExist / ErrCorrupt。
func (s *Store) Get(id hash.ID) (uint8, []byte, error) {
	rc, _, err := s.bs.Get(keyFor(id))
	if err != nil {
		if errors.Is(err, ErrNotExist) {
			return 0, nil, ErrNotExist
		}
		return 0, nil, err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, MaxObjectRawSize*2))
	if err != nil {
		return 0, nil, err
	}
	typ, raw, err := decodeStored(b)
	if err != nil {
		return 0, nil, errors.Join(ErrCorrupt, err)
	}
	// 读时校验：内容寻址的对象必须与 key 一致（§8.3）。
	if got := hash.Sum(raw); got != id {
		return 0, nil, errors.Join(ErrCorrupt,
			fmt.Errorf("cas: 对象 %s 内容哈希为 %s", id, got))
	}
	return typ, raw, nil
}

// ErrCorrupt 表示对象内容与其 key 不符或无法解码。
var ErrCorrupt = errors.New("cas: 对象损坏")

// GetBlob 读取一个分片的原文。
func (s *Store) GetBlob(id hash.ID) ([]byte, error) {
	typ, raw, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if typ != codec.TypeBlob {
		return nil, fmt.Errorf("cas: 对象 %s 是类型 %d，不是 Blob", id, typ)
	}
	return raw, nil
}

// GetFileContent 读取并解析 FileContent。
func (s *Store) GetFileContent(id hash.ID) (*tree.FileContent, error) {
	typ, raw, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if typ != codec.TypeFileContent {
		return nil, fmt.Errorf("cas: 对象 %s 是类型 %d，不是 FileContent", id, typ)
	}
	return tree.DecodeFileContent(raw)
}

// GetTree 读取并解析 Tree。
func (s *Store) GetTree(id hash.ID) (*tree.Tree, error) {
	typ, raw, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if typ != codec.TypeTree {
		return nil, fmt.Errorf("cas: 对象 %s 是类型 %d，不是 Tree", id, typ)
	}
	return tree.DecodeTree(raw)
}

// GetVersion 读取并解析 Version。
func (s *Store) GetVersion(id hash.ID) (*version.Version, error) {
	typ, raw, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if typ != codec.TypeVersion {
		return nil, fmt.Errorf("cas: 对象 %s 是类型 %d，不是 Version", id, typ)
	}
	return version.Decode(raw)
}

// ReadBlob 读取一个 Blob 的原文。
//
// 符号链接的条目 id 指向 Blob（链接目标字节），因此物化符号链接必须走这里，
// 而不是 ReadFileData（后者要求对象是 FileContent）。
func (s *Store) ReadBlob(id hash.ID) ([]byte, error) { return s.GetBlob(id) }

// ReadFileData 把 FileContent 的全部分片拼成文件原始字节。
//
// 它是 op.ContentStore 的一部分，供重放引擎读取内容。
func (s *Store) ReadFileData(id hash.ID) ([]byte, error) {
	fc, err := s.GetFileContent(id)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, fc.TotalSize)
	for _, cid := range fc.Chunks {
		b, err := s.GetBlob(cid)
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
	}
	if int64(len(out)) != fc.TotalSize {
		return nil, fmt.Errorf("cas: 文件 %s 长度 %d != 声明 %d", id, len(out), fc.TotalSize)
	}
	return out, nil
}

// PutFileData 对内容分块、写入 Blob 与 FileContent，返回 FileContent id。
//
// EOL 属性由内容判定（§3.5）。空文件产生 0 个 chunk（附录 H.5）。
func (s *Store) PutFileData(data []byte) (hash.ID, error) {
	fc := &tree.FileContent{
		TotalSize: int64(len(data)),
		EOL:       tree.DetectEOL(data),
	}
	prev := 0
	for _, end := range Split(data) {
		id, err := s.PutBlob(data[prev:end])
		if err != nil {
			return hash.ID{}, err
		}
		fc.Chunks = append(fc.Chunks, id)
		prev = end
	}
	return s.PutFileContent(fc)
}

// RebuildIndex 扫描 objects/ 前缀并重建对象索引（§8.3、§10）。
//
// 因为存储形态自带类型，这一步总是可行的，因此 ObjectIndex 只是缓存。
func (s *Store) RebuildIndex() (int, error) {
	stats, err := s.bs.ListPrefix("objects/")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, st := range stats {
		id, err := hash.Parse(st.Key[len("objects/"):])
		if err != nil {
			continue // 非对象文件，跳过
		}
		rc, _, err := s.bs.Get(st.Key)
		if err != nil {
			return n, err
		}
		b, err := io.ReadAll(io.LimitReader(rc, MaxObjectRawSize*2))
		_ = rc.Close()
		if err != nil {
			return n, err
		}
		typ, raw, err := decodeStored(b)
		if err != nil {
			return n, errors.Join(ErrCorrupt, fmt.Errorf("cas: %s: %w", st.Key, err))
		}
		if hash.Sum(raw) != id {
			return n, errors.Join(ErrCorrupt, fmt.Errorf("cas: %s 内容不符", st.Key))
		}
		if err := s.idx.Add(id, typ, int64(len(b)), int64(len(raw))); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
