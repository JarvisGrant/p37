package cas

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/klauspost/compress/zstd"

	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/tree"
	"github.com/chenyiwen/p37/internal/version"
)

// MaxObjectRawSize 是单个对象解压后的字节上限（§13：防解压炸弹）。
const MaxObjectRawSize = 64 << 20

// zstd 编解码器用池管理。单个实例内部是容量 1 的 channel，直接共享会
// 把多核并发读写串行化；池让每个 goroutine 拿到独立实例。
var zstdEncPool = sync.Pool{New: func() any {
	w, _ := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(1))
	return w
}}

var zstdDecPool = sync.Pool{New: func() any {
	r, _ := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(MaxObjectRawSize*2))
	return r
}}

// contentETag 返回内容的 BLAKE3 十六进制（用于非对象 key，如 refs）。
func contentETag(b []byte) string { return hash.Sum(b).String() }

// ObjectIndex 记录本地存在哪些对象及其类型。
//
// 它**只是缓存**：对象在存储中的形态自带类型（见 Store 的存储格式说明），
// 因此索引可以随时由 objects/ 前缀重建（§8.3、§10）。
type ObjectIndex interface {
	Has(id hash.ID) (bool, error)
	HasType(id hash.ID, typ uint8) (bool, error)
	Type(id hash.ID) (uint8, error)
	Types(id hash.ID) ([]uint8, error)
	Add(id hash.ID, typ uint8, size, rawSize int64) error
	AddMany(entries []IndexEntry) error
	Remove(id hash.ID) error
	RemoveType(id hash.ID, typ uint8) error
	Iterate(fn func(id hash.ID, typ uint8, size, rawSize int64) error) error
}

// ObjectKey 是「内容 id + 对象类型」的复合标识。
//
// 内容 id 只覆盖 raw；同一份 raw 在极少数情况下可以同时是 Tree / Blob
// （例如空 Tree 的编码恰好等于某文件的两字节内容）。v3 布局用 type 命名空间
// 区分它们，复合 key 是索引与 GC 的最小单位。
type ObjectKey struct {
	ID   hash.ID
	Type uint8
}

// IndexEntry 是批量登记一条对象索引所需的字段。
type IndexEntry struct {
	Key     ObjectKey
	Size    int64
	RawSize int64
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

	// verify 是读时校验策略（§8.3，附录 B 的 gc.verify_on_read）。
	verify VerifyMode
	// reads 用于 sample 模式下的抽样计数。
	reads atomic.Uint64

	// 对象索引批量登记：capture 一次写几千个对象，逐条 Add 会产生同样
	// 数量的 SQLite 写事务。Flush 时一次 AddMany。
	pendingMu sync.Mutex
	pending   map[ObjectKey]IndexEntry
}

// VerifyMode 是读时校验策略。
type VerifyMode string

// 读时校验策略取值。
const (
	// VerifyOff 完全不校验。仅用于明确知道数据可信的场景（例如刚写完读回）。
	VerifyOff VerifyMode = "off"
	// VerifySample 小对象全校验，大对象按比例抽样 —— 大对象解压+哈希的代价
	// 明显，抽样能在保留防线的前提下省下大部分开销。
	VerifySample VerifyMode = "sample"
	// VerifyAlways 每次读取都校验。
	VerifyAlways VerifyMode = "always"
)

// verifyLargeRaw 是 sample 模式下「算大对象」的阈值。
const verifyLargeRaw = 1 << 20

// verifyLargeStride 是 sample 模式下大对象的抽样步长（1/8）。
const verifyLargeStride = 8

// ParseVerifyMode 解析配置值；无法识别时取 sample。
func ParseVerifyMode(s string) VerifyMode {
	switch VerifyMode(s) {
	case VerifyOff, VerifyAlways, VerifySample:
		return VerifyMode(s)
	default:
		return VerifySample
	}
}

// SetVerifyOnRead 设置读时校验策略（§8.3）。
func (s *Store) SetVerifyOnRead(m VerifyMode) {
	if m == "" {
		m = VerifySample
	}
	s.verify = m
}

// shouldVerify 报告本次读取是否需要校验内容哈希。
func (s *Store) shouldVerify(rawLen int) bool {
	switch s.verify {
	case VerifyOff:
		return false
	case VerifyAlways:
		return true
	default:
		if rawLen <= verifyLargeRaw {
			return true
		}
		return s.reads.Add(1)%verifyLargeStride == 0
	}
}

// NewStore 组合 BlobStore 与 ObjectIndex。
func NewStore(bs BlobStore, idx ObjectIndex) *Store {
	return &Store{bs: bs, idx: idx, pending: map[ObjectKey]IndexEntry{}}
}

// BlobStore 返回底层后端。
func (s *Store) BlobStore() BlobStore { return s.bs }

// FlushIndex 把本批次累积的对象索引登记落库，但不 fsync 对象字节。
//
// capture.Build 在写 stat 缓存之前调用：stat_cache 的 ObjectID 会被后续采集
// 直接信任，如果索引还在内存里而进程崩溃，下一次采集就会拿着未登记的 id
// 去读对象，得到「key 不存在」。字节持久化仍由 Flush 负责。
func (s *Store) FlushIndex() error {
	s.pendingMu.Lock()
	if len(s.pending) == 0 {
		s.pendingMu.Unlock()
		return nil
	}
	entries := make([]IndexEntry, 0, len(s.pending))
	for _, e := range s.pending {
		entries = append(entries, e)
	}
	s.pending = map[ObjectKey]IndexEntry{}
	s.pendingMu.Unlock()
	if err := s.idx.AddMany(entries); err != nil {
		// 登记失败时把批次放回去，避免这些对象在内存里彻底丢失；
		// 下一次 Flush 重试，调用方也会收到错误。
		s.pendingMu.Lock()
		for _, e := range entries {
			if _, ok := s.pending[e.Key]; !ok {
				s.pending[e.Key] = e
			}
		}
		s.pendingMu.Unlock()
		return err
	}
	return nil
}

// Flush 确保此前写入的全部对象与索引已持久化。
//
// 调用方**必须**在推进主干 head 之前调用它（§7.3）。
func (s *Store) Flush() error {
	if err := s.FlushIndex(); err != nil {
		return err
	}
	return s.bs.Flush()
}

func (s *Store) addPending(id hash.ID, typ uint8, size, rawSize int64) error {
	s.pendingMu.Lock()
	s.pending[ObjectKey{ID: id, Type: typ}] = IndexEntry{
		Key: ObjectKey{ID: id, Type: typ}, Size: size, RawSize: rawSize,
	}
	s.pendingMu.Unlock()
	return nil
}

// keyFor 返回 v2 legacy 对象的 BlobStore key。
func keyFor(id hash.ID) string { return "objects/" + id.String() }

// typedKeyFor 返回 v3 类型命名空间 key。
func typedKeyFor(typ uint8, id hash.ID) string {
	return fmt.Sprintf("objects/%02x/%s", typ, id)
}

// storedHeader 是对象存储的前缀：0x01 || type。
const storedFormat = 0x01

// encodeStored 把 raw 编成自描述的存储字节。
func encodeStored(typ uint8, raw []byte) []byte {
	enc := zstdEncPool.Get().(*zstd.Encoder)
	comp := enc.EncodeAll(raw, make([]byte, 0, len(raw)/2+16))
	zstdEncPool.Put(enc)
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
	if typ < codec.TypeBlob || typ > codec.TypeVersionMerge {
		return 0, nil, fmt.Errorf("cas: 未知对象类型 0x%02x", typ)
	}
	dec := zstdDecPool.Get().(*zstd.Decoder)
	raw, err := dec.DecodeAll(b[2:], make([]byte, 0, 4096))
	zstdDecPool.Put(dec)
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
	if ok, err := s.idx.HasType(id, typ); err != nil {
		return hash.ID{}, err
	} else if ok {
		return id, nil
	}
	// 兼容 v2 legacy 对象：同一 id/type 已在 objects/<id> 时只补索引，
	// 避免升级后重复写一份 typed 副本。
	if oldType, ok := s.legacyType(id); ok && oldType == typ {
		stored := encodeStored(typ, raw)
		if err := s.addPending(id, typ, int64(len(stored)), int64(len(raw))); err != nil {
			return hash.ID{}, err
		}
		return id, nil
	}
	stored := encodeStored(typ, raw)
	key := typedKeyFor(typ, id)
	if err := s.bs.PutIfAbsent(key, bytes.NewReader(stored), int64(len(stored))); err != nil && !errors.Is(err, ErrExists) {
		return hash.ID{}, err
	}
	// 索引在对象落盘之后登记；即使此处崩溃，索引也可由扫描重建。
	if err := s.addPending(id, typ, int64(len(stored)), int64(len(raw))); err != nil {
		return hash.ID{}, err
	}
	return id, nil
}

// legacyType 读取 v2 objects/<id> 的对象类型。
func (s *Store) legacyType(id hash.ID) (uint8, bool) {
	hdr, err := s.bs.GetRange(keyFor(id), 0, 2)
	if err != nil || len(hdr) != 2 || hdr[0] != storedFormat {
		return 0, false
	}
	if hdr[1] < codec.TypeBlob || hdr[1] > codec.TypeVersionMerge {
		return 0, false
	}
	return hdr[1], true
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
	typ := uint8(codec.TypeVersion)
	if len(v.Parents) > 1 {
		typ = codec.TypeVersionMerge
	}
	return s.put(typ, enc)
}

// Has 报告对象是否已在本地（任意类型）。
func (s *Store) Has(id hash.ID) (bool, error) {
	if ok, err := s.idx.Has(id); err != nil || ok {
		return ok, err
	}
	if _, ok := s.legacyType(id); ok {
		return true, nil
	}
	for typ := uint8(codec.TypeBlob); typ <= codec.TypeVersionMerge; typ++ {
		if _, err := s.bs.Stat(typedKeyFor(typ, id)); err == nil {
			return true, nil
		}
	}
	return false, nil
}

// HasType 报告指定类型的对象是否已在本地。
func (s *Store) HasType(id hash.ID, typ uint8) (bool, error) {
	s.pendingMu.Lock()
	_, pending := s.pending[ObjectKey{ID: id, Type: typ}]
	s.pendingMu.Unlock()
	if pending {
		return true, nil
	}
	if ok, err := s.idx.HasType(id, typ); err != nil || ok {
		return ok, err
	}
	if old, ok := s.legacyType(id); ok && old == typ {
		return true, nil
	}
	if _, err := s.bs.Stat(typedKeyFor(typ, id)); err == nil {
		return true, nil
	}
	return false, nil
}

// typesOf 合并「已 flush 的索引」与「本批次尚未 flush 的 pending 索引」。
func (s *Store) typesOf(id hash.ID) ([]uint8, error) {
	seen := map[uint8]bool{}
	s.pendingMu.Lock()
	for k := range s.pending {
		if k.ID == id {
			seen[k.Type] = true
		}
	}
	s.pendingMu.Unlock()
	ts, err := s.idx.Types(id)
	if err != nil {
		return nil, err
	}
	for _, t := range ts {
		seen[t] = true
	}
	out := make([]uint8, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	return out, nil
}

// Get 读取对象原文与类型。对象缺失或校验失败返回 ErrNotExist / ErrCorrupt。
// 同一 id 有多个类型时返回 ErrAmbiguousType；调用方应使用 GetTyped。
func (s *Store) Get(id hash.ID) (uint8, []byte, error) {
	types, err := s.typesOf(id)
	if err != nil {
		return 0, nil, err
	}
	switch len(types) {
	case 1:
		return s.GetTyped(types[0], id)
	case 0:
		if old, ok := s.legacyType(id); ok {
			return s.GetTyped(old, id)
		}
		for typ := uint8(codec.TypeBlob); typ <= codec.TypeVersionMerge; typ++ {
			if _, err := s.bs.Stat(typedKeyFor(typ, id)); err == nil {
				return s.GetTyped(typ, id)
			}
		}
		return 0, nil, ErrNotExist
	default:
		return 0, nil, fmt.Errorf("%w: %s", ErrAmbiguousType, id)
	}
}

// GetTyped 读取指定类型的对象原文。
func (s *Store) GetTyped(typ uint8, id hash.ID) (uint8, []byte, error) {
	keys := []string{typedKeyFor(typ, id), keyFor(id)}
	for _, key := range keys {
		gotType, raw, err := s.readAt(key, id)
		if err != nil {
			if errors.Is(err, ErrNotExist) {
				continue
			}
			return 0, nil, err
		}
		if gotType != typ {
			continue
		}
		return gotType, raw, nil
	}
	return 0, nil, ErrNotExist
}

func (s *Store) readAt(key string, id hash.ID) (uint8, []byte, error) {
	rc, _, err := s.bs.Get(key)
	if err != nil {
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
	if s.shouldVerify(len(raw)) {
		if got := hash.Sum(raw); got != id {
			return 0, nil, errors.Join(ErrCorrupt,
				fmt.Errorf("cas: 对象 %s 内容哈希为 %s", id, got))
		}
	}
	return typ, raw, nil
}

// ErrCorrupt 表示对象内容与其 key 不符或无法解码。
var ErrCorrupt = errors.New("cas: 对象损坏")

// ErrTypeConflict 表示同一个内容哈希已以另一种对象类型存在，且调用方没有
// 使用 typed key。
var ErrTypeConflict = errors.New("cas: 同一内容已存在不同类型")

// ErrAmbiguousType 表示一个 id 同时存在多个对象类型，通用 Get 无法决定。
var ErrAmbiguousType = errors.New("cas: 同一内容存在多个类型")

// GetBlob 读取一个分片的原文。
func (s *Store) GetBlob(id hash.ID) ([]byte, error) {
	_, raw, err := s.GetTyped(codec.TypeBlob, id)
	return raw, err
}

// GetFileContent 读取并解析 FileContent。
func (s *Store) GetFileContent(id hash.ID) (*tree.FileContent, error) {
	_, raw, err := s.GetTyped(codec.TypeFileContent, id)
	if err != nil {
		return nil, err
	}
	return tree.DecodeFileContent(raw)
}

// GetTree 读取并解析 Tree。
func (s *Store) GetTree(id hash.ID) (*tree.Tree, error) {
	_, raw, err := s.GetTyped(codec.TypeTree, id)
	if err != nil {
		return nil, err
	}
	return tree.DecodeTree(raw)
}

// GetVersion 读取并解析 Version。
func (s *Store) GetVersion(id hash.ID) (*version.Version, error) {
	_, raw, err := s.GetTyped(codec.TypeVersion, id)
	if err != nil {
		_, raw2, err2 := s.GetTyped(codec.TypeVersionMerge, id)
		if err2 != nil {
			return nil, err
		}
		raw = raw2
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
//
// 注意：它会把整个文件读进内存。导出、归档等可能处理大文件的路径应使用
// WriteFileData / ReadFilePrefix，避免在 1 GiB 级别的实例上 OOM。
func (s *Store) ReadFileData(id hash.ID) ([]byte, error) {
	fc, err := s.GetFileContent(id)
	if err != nil {
		return nil, err
	}
	// 不要按声明值一次性预分配：损坏/恶意 FileContent 的 TotalSize 可能极大。
	// 分块读入时 append 自然增长，容量提示只作为小文件的优化。
	capHint := fc.TotalSize
	if capHint > 1<<20 {
		capHint = 1 << 20
	}
	out := make([]byte, 0, capHint)
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

// WriteFileData 按分块顺序把文件原文写到 w，内存占用上界为单个分块
// （MaxSize = 64 KiB），因此可以安全地导出大文件。
func (s *Store) WriteFileData(id hash.ID, w io.Writer) (int64, error) {
	fc, err := s.GetFileContent(id)
	if err != nil {
		return 0, err
	}
	var written int64
	for _, cid := range fc.Chunks {
		b, err := s.GetBlob(cid)
		if err != nil {
			return written, err
		}
		n, err := w.Write(b)
		written += int64(n)
		if err != nil {
			return written, err
		}
		if n != len(b) {
			return written, io.ErrShortWrite
		}
	}
	if written != fc.TotalSize {
		return written, fmt.Errorf("cas: 文件 %s 长度 %d != 声明 %d", id, written, fc.TotalSize)
	}
	return written, nil
}

// ReadFilePrefix 只读取文件前 max 字节，返回内容与文件总长度。
//
// 与 ReadFileData 不同，它不会为尾部数据分配内存；max <= 0 表示读取全部。
// version_file 这类有响应上限的接口应使用它。
func (s *Store) ReadFilePrefix(id hash.ID, max int64) ([]byte, int64, error) {
	fc, err := s.GetFileContent(id)
	if err != nil {
		return nil, 0, err
	}
	if max <= 0 || max > fc.TotalSize {
		max = fc.TotalSize
	}
	capHint := max
	if capHint > 1<<20 {
		capHint = 1 << 20
	}
	out := make([]byte, 0, capHint)
	for _, cid := range fc.Chunks {
		if int64(len(out)) >= max {
			break
		}
		b, err := s.GetBlob(cid)
		if err != nil {
			return nil, fc.TotalSize, err
		}
		need := max - int64(len(out))
		if int64(len(b)) > need {
			b = b[:need]
		}
		out = append(out, b...)
	}
	return out, fc.TotalSize, nil
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
		id, ok := objectIDFromKey(st.Key)
		if !ok {
			continue
		}
		hdr, herr := s.bs.GetRange(st.Key, 0, 2)
		if herr != nil || len(hdr) != 2 || hdr[0] != storedFormat {
			return n, errors.Join(ErrCorrupt, fmt.Errorf("cas: %s 头部不可读", st.Key))
		}
		typ := hdr[1]
		if typ < codec.TypeBlob || typ > codec.TypeVersionMerge {
			return n, errors.Join(ErrCorrupt, fmt.Errorf("cas: %s 未知类型 0x%02x", st.Key, typ))
		}
		b, rerr := s.bs.GetRange(st.Key, 0, st.Size)
		if rerr != nil {
			return n, rerr
		}
		_, raw, derr := decodeStored(b)
		if derr != nil {
			return n, errors.Join(ErrCorrupt, fmt.Errorf("cas: %s: %w", st.Key, derr))
		}
		if hash.Sum(raw) != id {
			return n, errors.Join(ErrCorrupt, fmt.Errorf("cas: %s 内容不符", st.Key))
		}
		if err := s.idx.Add(id, typ, st.Size, int64(len(raw))); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ObjectInfo 是一个 CAS 对象的元信息（不解压内容）。
type ObjectInfo struct {
	ID   hash.ID
	Type uint8
	// StoredSize 是磁盘上的字节数（含 2 字节头与压缩）。
	StoredSize int64
}

// Head 只读取对象的**类型与存储长度**，不解压、不校验内容哈希。
// 同一 id 有多个类型时返回 ErrAmbiguousType；GC 应使用 ListObjects。
func (s *Store) Head(id hash.ID) (uint8, int64, error) {
	types, err := s.typesOf(id)
	if err != nil {
		return 0, 0, err
	}
	if len(types) > 1 {
		return 0, 0, fmt.Errorf("%w: %s", ErrAmbiguousType, id)
	}
	if len(types) == 1 {
		return s.HeadTyped(types[0], id)
	}
	if old, ok := s.legacyType(id); ok {
		return s.HeadTyped(old, id)
	}
	for typ := uint8(codec.TypeBlob); typ <= codec.TypeVersionMerge; typ++ {
		if _, err := s.bs.Stat(typedKeyFor(typ, id)); err == nil {
			return s.HeadTyped(typ, id)
		}
	}
	return 0, 0, ErrNotExist
}

// HeadTyped 只读取指定类型的对象头与存储长度。
func (s *Store) HeadTyped(typ uint8, id hash.ID) (uint8, int64, error) {
	for _, key := range []string{typedKeyFor(typ, id), keyFor(id)} {
		if st, err := s.bs.Stat(key); err == nil {
			hdr, herr := s.bs.GetRange(key, 0, 2)
			if herr != nil || len(hdr) != 2 || hdr[0] != storedFormat {
				return 0, 0, errors.Join(ErrCorrupt, fmt.Errorf("cas: %s 头部不可读", key))
			}
			if hdr[1] != typ {
				continue
			}
			return typ, st.Size, nil
		}
	}
	return 0, 0, ErrNotExist
}

// ListObjects 枚举全部对象（类型 + 存储长度），不解压内容。
func (s *Store) ListObjects() ([]ObjectInfo, error) {
	stats, err := s.bs.ListPrefix("objects/")
	if err != nil {
		return nil, err
	}
	seen := map[ObjectKey]bool{}
	out := make([]ObjectInfo, 0, len(stats))
	for _, st := range stats {
		id, ok := objectIDFromKey(st.Key)
		if !ok {
			continue
		}
		hdr, herr := s.bs.GetRange(st.Key, 0, 2)
		if herr != nil || len(hdr) != 2 || hdr[0] != storedFormat {
			continue
		}
		typ := hdr[1]
		if typ < codec.TypeBlob || typ > codec.TypeVersionMerge {
			continue
		}
		k := ObjectKey{ID: id, Type: typ}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, ObjectInfo{ID: id, Type: typ, StoredSize: st.Size})
	}
	return out, nil
}

// DeleteObjectTyped 删除指定类型并同步从索引中摘除。
func (s *Store) DeleteObjectTyped(typ uint8, id hash.ID) error {
	s.pendingMu.Lock()
	delete(s.pending, ObjectKey{ID: id, Type: typ})
	s.pendingMu.Unlock()
	if err := s.idx.RemoveType(id, typ); err != nil {
		return err
	}
	for _, key := range []string{typedKeyFor(typ, id), keyFor(id)} {
		if key == keyFor(id) {
			if old, ok := s.legacyType(id); !ok || old != typ {
				continue
			}
		}
		if err := s.bs.Delete(key); err != nil && !errors.Is(err, ErrNotExist) {
			return err
		}
	}
	return nil
}

// DeleteObject 删除一个对象的全部类型并同步从索引中摘除。
func (s *Store) DeleteObject(id hash.ID) error {
	s.pendingMu.Lock()
	for k := range s.pending {
		if k.ID == id {
			delete(s.pending, k)
		}
	}
	s.pendingMu.Unlock()
	types, _ := s.idx.Types(id)
	if err := s.idx.Remove(id); err != nil {
		return err
	}
	for _, typ := range types {
		if err := s.bs.Delete(typedKeyFor(typ, id)); err != nil && !errors.Is(err, ErrNotExist) {
			return err
		}
	}
	if err := s.bs.Delete(keyFor(id)); err != nil && !errors.Is(err, ErrNotExist) {
		return err
	}
	return nil
}

// GetStored 返回对象的**存储字节**（0x01||type||zstd(raw)），不解压。
func (s *Store) GetStored(id hash.ID) ([]byte, error) {
	types, _ := s.typesOf(id)
	if len(types) == 1 {
		return s.GetStoredTyped(types[0], id)
	}
	if old, ok := s.legacyType(id); ok {
		return s.GetStoredTyped(old, id)
	}
	for typ := uint8(codec.TypeBlob); typ <= codec.TypeVersionMerge; typ++ {
		if b, err := s.GetStoredTyped(typ, id); err == nil {
			return b, nil
		}
	}
	return nil, ErrNotExist
}

// GetStoredTyped 返回指定类型对象的存储字节。
func (s *Store) GetStoredTyped(typ uint8, id hash.ID) ([]byte, error) {
	for _, key := range []string{typedKeyFor(typ, id), keyFor(id)} {
		rc, _, err := s.bs.Get(key)
		if err != nil {
			if errors.Is(err, ErrNotExist) {
				continue
			}
			return nil, err
		}
		b, rerr := io.ReadAll(io.LimitReader(rc, MaxObjectRawSize*2))
		_ = rc.Close()
		if rerr != nil {
			return nil, rerr
		}
		if len(b) < 2 || b[0] != storedFormat || b[1] != typ {
			continue
		}
		return b, nil
	}
	return nil, ErrNotExist
}

// PutStored 写入一份**已编码**的对象字节（同步拉取用）。
func (s *Store) PutStored(id hash.ID, stored []byte) error {
	typ, raw, err := decodeStored(stored)
	if err != nil {
		return errors.Join(ErrCorrupt, err)
	}
	if got := hash.Sum(raw); got != id {
		return errors.Join(ErrCorrupt, fmt.Errorf("cas: 对象 %s 内容哈希为 %s", id, got))
	}
	if ok, err := s.idx.HasType(id, typ); err != nil {
		return err
	} else if ok {
		return nil
	}
	if old, ok := s.legacyType(id); ok && old == typ {
		return s.addPending(id, typ, int64(len(stored)), int64(len(raw)))
	}
	if err := s.bs.PutIfAbsent(typedKeyFor(typ, id), bytes.NewReader(stored), int64(len(stored))); err != nil && !errors.Is(err, ErrExists) {
		return err
	}
	return s.addPending(id, typ, int64(len(stored)), int64(len(raw)))
}

// PackLoose 把松散的小对象合并进 pack（§9 P2）。仅在带打包装饰层时可用。
func (s *Store) PackLoose(opt PackOptions) (PackResult, error) {
	ps, ok := s.bs.(*PackStore)
	if !ok {
		return PackResult{}, nil // 未启用打包：无操作
	}
	return ps.PackLoose(opt)
}

// SweepResult 是一次 pack 回收的结果。
type SweepResult struct {
	PacksDropped   int   `json:"packs_dropped"`
	ObjectsDropped int   `json:"objects_dropped"`
	BytesReclaimed int64 `json:"bytes_reclaimed"`
}

// SweepPacks 删除「全部对象都不可达」的 pack，并把这些对象从索引摘除。
//
// 只做整包回收：pack 内还有可达对象时保留（空间浪费有界，留待将来的 repack）。
func (s *Store) SweepPacks(reachable map[hash.ID]struct{}) (SweepResult, error) {
	var res SweepResult
	ps, ok := s.bs.(*PackStore)
	if !ok {
		return res, nil
	}
	packs, err := ps.Packs()
	if err != nil {
		return res, err
	}
	for _, pid := range packs {
		entries, err := ps.PackObjects(pid)
		if err != nil {
			return res, err
		}
		allDead := len(entries) > 0
		for _, en := range entries {
			if _, live := reachable[en.id]; live {
				allDead = false
				break
			}
		}
		if !allDead {
			continue
		}
		st, err := ps.base.Stat(packKey(pid))
		var size int64
		if err == nil {
			size = st.Size
		}
		// 先摘索引、再删 pack：反过来若在两步之间崩溃，索引仍声称对象
		// 存在而字节已经没了，后续同内容写入会被去重快路径永久吞掉。
		for _, en := range entries {
			if err := s.idx.Remove(en.id); err != nil {
				return res, err
			}
		}
		if err := ps.DeletePack(pid); err != nil {
			return res, err
		}
		res.PacksDropped++
		res.ObjectsDropped += len(entries)
		res.BytesReclaimed += size
	}
	return res, nil
}
