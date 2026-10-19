package cas

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"

	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
)

// 本文件实现 §9（P2）的对象**打包**：把大量小对象从「一对象一文件」合并进
// 少数 pack 文件，消除每个对象一份的最小块开销（4 KiB 块粒度下，24 字节的
// 对象也要占一整块，1 万个小文件实测放大数百倍）。
//
// 设计取舍（在缺少 §9 原文时的保守实现）：
//
//   - **离线打包**：写路径不变（仍写松散对象），打包是一次维护操作
//     （`p37 gc --pack`）。这样「对象落盘 → 登记索引」的既有纪律不变，
//     不会引入「索引里有、字节还没落盘」的窗口。
//   - pack 文件是**不可变**的：内容是若干对象字节 + 尾部索引，key 由内容
//     寻址（`packs/<hash>`）。只创建、不原地改写，符合 BlobStore 的 S3 子集。
//   - 自描述、可重建：尾部索引给出 `id → (offset, length, type)`，因此
//     `object_index` 仍是纯缓存，扫描 `packs/` 即可重建。
//   - 崩溃安全：先写 pack（内容寻址，写坏=孤儿）→ Flush → 再删松散文件。
//     任何时刻崩溃，对象要么在松散文件里、要么在 pack 里，读者总取得到。
//   - pack 粒度回收：只有当 pack 内**所有**对象都不可达时才整体删除
//     （不做 repack 压实，避免复杂度；空间浪费有界）。
//
// 格式（冻结）：
//
//	pack   := header || body || footer || trailer
//	header := "P37PACK1"            (8 字节)
//	body   := 对象存储字节依次拼接（每个 = 0x01 || type || zstd(raw)，见 store.go）
//	footer := uvarint(count) || count × entry
//	entry  := id(32 字节) || uvarint(offset) || uvarint(length) || u8(type)
//	trailer:= "P37PKEND" || u64be(footerLen)
//
// entry 按 id 升序排列，因此同一组对象产生同一份 pack 字节。

const (
	packHeaderMagic  = "P37PACK1"
	packTrailerMagic = "P37PKEND"
	packHeaderLen    = 8
	packTrailerLen   = 16 // magic(8) + u64be(footerLen)
)

// PackOptions 控制一次打包。
type PackOptions struct {
	// TargetPackBytes 是单个 pack 的目标体积上限（对象字节之和）。
	TargetPackBytes int64
	// MaxObjectBytes 是允许入包的单对象存储字节上限：大对象打包收益低，
	// 留在松散文件里反而更利于顺序读。
	MaxObjectBytes int64
	// MinObjects 是触发打包的最少对象数：对象太少时不值得新建 pack。
	MinObjects int
}

// DefaultPackOptions 返回默认打包参数。
func DefaultPackOptions() PackOptions {
	return PackOptions{TargetPackBytes: 4 << 20, MaxObjectBytes: 1 << 20, MinObjects: 8}
}

func (o PackOptions) withDefaults() PackOptions {
	d := DefaultPackOptions()
	if o.TargetPackBytes <= 0 {
		o.TargetPackBytes = d.TargetPackBytes
	}
	if o.MaxObjectBytes <= 0 {
		o.MaxObjectBytes = d.MaxObjectBytes
	}
	if o.MinObjects < 0 {
		o.MinObjects = d.MinObjects
	}
	return o
}

// packEntry 是尾部索引里的一项。
type packEntry struct {
	id     hash.ID
	off    int64
	length int64
	typ    uint8
}

// packKey 是 pack 的 BlobStore key。
func packKey(id hash.ID) string { return "packs/" + id.String() }

// objectIDFromKey 解析 `objects/<hex>` 形式的 key。
func objectIDFromKey(key string) (hash.ID, bool) {
	const p = "objects/"
	if !strings.HasPrefix(key, p) {
		return hash.ID{}, false
	}
	id, err := hash.Parse(key[len(p):])
	if err != nil {
		return hash.ID{}, false
	}
	return id, true
}

// encodePack 由「已按 id 升序排列的对象」构造 pack 字节。
// 每个 obj 是其**存储字节**（含 0x01||type 头）。
func encodePack(ids []hash.ID, stored [][]byte, types []uint8) []byte {
	total := packHeaderLen + packTrailerLen
	for _, b := range stored {
		total += len(b)
	}
	buf := make([]byte, 0, total+len(ids)*48)
	buf = append(buf, packHeaderMagic...)

	entries := make([]packEntry, len(ids))
	for i := range ids {
		entries[i] = packEntry{id: ids[i], off: int64(len(buf)), length: int64(len(stored[i])), typ: types[i]}
		buf = append(buf, stored[i]...)
	}

	footer := encodePackIndex(entries)
	buf = append(buf, footer...)
	buf = append(buf, packTrailerMagic...)
	var ln [8]byte
	binary.BigEndian.PutUint64(ln[:], uint64(len(footer)))
	buf = append(buf, ln[:]...)
	return buf
}

// parsePack 校验并解析整份 pack 字节，返回按 id 升序的索引。
func parsePack(b []byte) ([]packEntry, error) {
	if len(b) < packHeaderLen+packTrailerLen {
		return nil, fmt.Errorf("%w: pack 过短 (%d 字节)", ErrCorrupt, len(b))
	}
	if string(b[:packHeaderLen]) != packHeaderMagic {
		return nil, fmt.Errorf("%w: pack 头标记不符", ErrCorrupt)
	}
	trailer := b[len(b)-packTrailerLen:]
	if string(trailer[:8]) != packTrailerMagic {
		return nil, fmt.Errorf("%w: pack 尾标记不符（可能被截断）", ErrCorrupt)
	}
	footerLen := binary.BigEndian.Uint64(trailer[8:])
	footerStart := len(b) - packTrailerLen - int(footerLen)
	if footerStart < packHeaderLen || footerStart > len(b)-packTrailerLen {
		return nil, fmt.Errorf("%w: pack 索引长度非法 (%d)", ErrCorrupt, footerLen)
	}

	entries, err := decodePackIndex(b[footerStart : len(b)-packTrailerLen])
	if err != nil {
		return nil, err
	}

	// body 必须恰好覆盖 [header, footerStart)，且条目连续、有序。
	expect := int64(packHeaderLen)
	for i := range entries {
		en := entries[i]
		if en.off != expect || en.length < 0 || en.off+en.length > int64(footerStart) {
			return nil, fmt.Errorf("%w: pack 条目 %d 偏移/长度非法", ErrCorrupt, i)
		}
		if en.typ < codec.TypeBlob || en.typ > codec.TypeVersion {
			return nil, fmt.Errorf("%w: pack 条目类型非法 0x%02x", ErrCorrupt, en.typ)
		}
		if i > 0 && bytes.Compare(entries[i-1].id[:], en.id[:]) >= 0 {
			return nil, fmt.Errorf("%w: pack 条目未按 id 升序/去重", ErrCorrupt)
		}
		expect = en.off + en.length
	}
	if expect != int64(footerStart) {
		return nil, fmt.Errorf("%w: pack body 长度与索引不符", ErrCorrupt)
	}
	return entries, nil
}

// MaxPackEntries 是单个 pack 的条目数上限（§13 的同类保护）。
const MaxPackEntries = 1 << 20

// DecodeStored 解开存储字节，返回类型与原文。
func DecodeStored(b []byte) (uint8, []byte, error) { return decodeStored(b) }

// EncodeStoredAt 用指定 zstd 等级把原文编成存储字节（level<=0 用默认等级）。
//
// 对象 id 由原文决定，因此重压不改变内容寻址，只影响传输体积。
func EncodeStoredAt(typ uint8, raw []byte, level int) []byte {
	if level <= 0 {
		return encodeStored(typ, raw)
	}
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)),
		zstd.WithEncoderConcurrency(1))
	if err != nil {
		return encodeStored(typ, raw)
	}
	defer func() { _ = enc.Close() }()
	comp := enc.EncodeAll(raw, make([]byte, 0, len(raw)/2+16))
	out := make([]byte, 0, len(comp)+2)
	out = append(out, storedFormat, typ)
	return append(out, comp...)
}

type packLoc struct {
	pack       hash.ID
	off        int64
	length     int64 // 存储字节长度（含 0x01||type）
	storedSize int64 // 与 length 同义，供 Stat 用
	typ        uint8
}

// PackStore 是 BlobStore 的**打包装饰层**：读时把 `objects/<id>` 透明地解析到
// 所属 pack 的区间；写路径原样透传（打包是离线操作）。
type PackStore struct {
	base BlobStore

	mu     sync.RWMutex
	loaded bool
	locs   map[hash.ID]packLoc
	packs  []hash.ID
	// loose 记录已知仍在松散文件里的对象（用于打包时挑选）。
	// 不加载全量，仅按需列举。
}

// NewPackStore 包装一个底层 BlobStore。
func NewPackStore(base BlobStore) *PackStore {
	return &PackStore{base: base, locs: map[hash.ID]packLoc{}}
}

// Base 返回底层后端。
func (ps *PackStore) Base() BlobStore { return ps.base }

// ensureLoaded 首次访问时扫描 packs/ 建立位置表。
func (ps *PackStore) ensureLoaded() error {
	ps.mu.RLock()
	if ps.loaded {
		ps.mu.RUnlock()
		return nil
	}
	ps.mu.RUnlock()

	stats, err := ps.base.ListPrefix("packs/")
	if err != nil {
		return err
	}
	locs := make(map[hash.ID]packLoc)
	packs := make([]hash.ID, 0, len(stats))
	for _, st := range stats {
		pid, err := hash.Parse(strings.TrimPrefix(st.Key, "packs/"))
		if err != nil {
			continue // 非 pack 文件
		}
		entries, err := ps.readFooter(pid, st.Size)
		if err != nil {
			return fmt.Errorf("cas: 读取 pack %s 索引失败: %w", pid, err)
		}
		packs = append(packs, pid)
		for _, en := range entries {
			if _, dup := locs[en.id]; dup {
				// 同一对象出现在多个 pack（打包未完成就崩了）：保留其一即可。
				continue
			}
			locs[en.id] = packLoc{pack: pid, off: en.off, length: en.length, storedSize: en.length, typ: en.typ}
		}
	}
	sort.Slice(packs, func(i, j int) bool { return bytes.Compare(packs[i][:], packs[j][:]) < 0 })

	ps.mu.Lock()
	if !ps.loaded {
		ps.locs = locs
		ps.packs = packs
		ps.loaded = true
	}
	ps.mu.Unlock()
	return nil
}

// readFooter 从底层读出 pack 的尾部索引（只读头部 8 字节 + 尾部 + 索引）。
func (ps *PackStore) readFooter(pid hash.ID, size int64) ([]packEntry, error) {
	key := packKey(pid)
	if size < packHeaderLen+packTrailerLen {
		return nil, fmt.Errorf("%w: pack 过短", ErrCorrupt)
	}
	hdr, err := ps.base.GetRange(key, 0, packHeaderLen)
	if err != nil {
		return nil, err
	}
	if string(hdr) != packHeaderMagic {
		return nil, fmt.Errorf("%w: pack 头标记不符", ErrCorrupt)
	}
	trailer, err := ps.base.GetRange(key, size-packTrailerLen, packTrailerLen)
	if err != nil {
		return nil, err
	}
	if string(trailer[:8]) != packTrailerMagic {
		return nil, fmt.Errorf("%w: pack 尾标记不符", ErrCorrupt)
	}
	footerLen := int64(binary.BigEndian.Uint64(trailer[8:]))
	if footerLen <= 0 || footerLen > size-packHeaderLen-packTrailerLen {
		return nil, fmt.Errorf("%w: pack 索引长度非法 (%d)", ErrCorrupt, footerLen)
	}
	footer, err := ps.base.GetRange(key, size-packTrailerLen-footerLen, footerLen)
	if err != nil {
		return nil, err
	}
	// 这里只需索引本身合法；body 连续性由打包端保证，fsck 会整份校验。
	return decodePackIndex(footer)
}

// encodePackIndex 编码尾部索引。
func encodePackIndex(entries []packEntry) []byte {
	e := codec.NewEncoder()
	e.Uvarint(uint64(len(entries)))
	for _, en := range entries {
		e.Hash(en.id)
		e.Uvarint(uint64(en.off))
		e.Uvarint(uint64(en.length))
		e.U8(en.typ)
	}
	return e.Out()
}

// decodePackIndex 解析尾部索引并做基本校验。
func decodePackIndex(footer []byte) ([]packEntry, error) {
	d := codec.NewDecoder(footer)
	count := d.Uvarint()
	if d.Err() != nil {
		return nil, fmt.Errorf("%w: pack 索引不可读", ErrCorrupt)
	}
	if count > MaxPackEntries {
		return nil, fmt.Errorf("%w: pack 条目数 %d 超上限", ErrCorrupt, count)
	}
	entries := make([]packEntry, 0, count)
	for i := uint64(0); i < count; i++ {
		var en packEntry
		en.id = d.Hash()
		en.off = int64(d.Uvarint())
		en.length = int64(d.Uvarint())
		en.typ = d.U8()
		if d.Err() != nil {
			return nil, fmt.Errorf("%w: pack 索引截断", ErrCorrupt)
		}
		entries = append(entries, en)
	}
	if err := d.Finish(); err != nil {
		return nil, fmt.Errorf("%w: pack 索引有多余字节", ErrCorrupt)
	}
	return entries, nil
}

// refreshPacks 增量扫描 packs/：只读取尚未登记过的 pack 的索引。
//
// 打包是**另一个进程**（p37 gc --pack）做的维护操作，常驻 daemon 的位置表
// 必须能发现新 pack，否则「松散副本已删、位置表还没更新」时会误报 key 不存在。
func (ps *PackStore) refreshPacks() error {
	stats, err := ps.base.ListPrefix("packs/")
	if err != nil {
		return err
	}
	ps.mu.RLock()
	known := make(map[hash.ID]struct{}, len(ps.packs))
	for _, p := range ps.packs {
		known[p] = struct{}{}
	}
	loaded := ps.loaded
	ps.mu.RUnlock()
	if !loaded {
		return ps.ensureLoaded()
	}

	type pending struct {
		id   hash.ID
		size int64
	}
	var fresh []pending
	for _, st := range stats {
		pid, err := hash.Parse(strings.TrimPrefix(st.Key, "packs/"))
		if err != nil {
			continue
		}
		if _, ok := known[pid]; ok {
			continue
		}
		fresh = append(fresh, pending{id: pid, size: st.Size})
	}
	if len(fresh) == 0 {
		return nil
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for _, f := range fresh {
		entries, err := ps.readFooter(f.id, f.size)
		if err != nil {
			return fmt.Errorf("cas: 读取 pack %s 索引失败: %w", f.id, err)
		}
		for _, en := range entries {
			if _, dup := ps.locs[en.id]; dup {
				continue
			}
			ps.locs[en.id] = packLoc{pack: f.id, off: en.off, length: en.length, storedSize: en.length, typ: en.typ}
		}
		if !ps.hasPackLocked(f.id) {
			ps.packs = append(ps.packs, f.id)
		}
	}
	sort.Slice(ps.packs, func(i, j int) bool { return bytes.Compare(ps.packs[i][:], ps.packs[j][:]) < 0 })
	return nil
}

// lookup 返回对象所在 pack 的位置。
func (ps *PackStore) lookup(id hash.ID) (packLoc, bool, error) {
	if err := ps.ensureLoaded(); err != nil {
		return packLoc{}, false, err
	}
	ps.mu.RLock()
	loc, ok := ps.locs[id]
	ps.mu.RUnlock()
	return loc, ok, nil
}

// lookupFresh 先查缓存；未命中时增量刷新 pack 列表再查一次。
func (ps *PackStore) lookupFresh(id hash.ID) (packLoc, bool, error) {
	loc, ok, err := ps.lookup(id)
	if err != nil || ok {
		return loc, ok, err
	}
	if err := ps.refreshPacks(); err != nil {
		return packLoc{}, false, err
	}
	return ps.lookup(id)
}

// Get 实现 BlobStore。
func (ps *PackStore) Get(key string) (io.ReadCloser, Stat, error) {
	if id, ok := objectIDFromKey(key); ok {
		if loc, found, err := ps.lookupFresh(id); err != nil {
			return nil, Stat{}, err
		} else if found {
			b, err := ps.readObject(loc)
			if err != nil {
				return nil, Stat{}, err
			}
			return io.NopCloser(bytes.NewReader(b)),
				Stat{Key: key, Size: int64(len(b)), ETag: id.String()}, nil
		}
	}
	return ps.base.Get(key)
}

// GetRange 实现 BlobStore。
func (ps *PackStore) GetRange(key string, off, length int64) ([]byte, error) {
	if id, ok := objectIDFromKey(key); ok {
		if loc, found, err := ps.lookupFresh(id); err != nil {
			return nil, err
		} else if found {
			if off < 0 || length < 0 || off > loc.length {
				return nil, fmt.Errorf("cas: 区间 [%d,%d) 越界（对象 %d 字节）", off, off+length, loc.length)
			}
			if off+length > loc.length {
				length = loc.length - off
			}
			return ps.base.GetRange(packKey(loc.pack), loc.off+off, length)
		}
	}
	return ps.base.GetRange(key, off, length)
}

// readObject 读出一个已打包对象的完整存储字节。
func (ps *PackStore) readObject(loc packLoc) ([]byte, error) {
	return ps.base.GetRange(packKey(loc.pack), loc.off, loc.length)
}

// Stat 实现 BlobStore。
func (ps *PackStore) Stat(key string) (Stat, error) {
	if id, ok := objectIDFromKey(key); ok {
		if loc, found, err := ps.lookupFresh(id); err != nil {
			return Stat{}, err
		} else if found {
			return Stat{Key: key, Size: loc.length, ETag: id.String()}, nil
		}
	}
	return ps.base.Stat(key)
}

// PutIfAbsent 实现 BlobStore（写路径透传：打包是离线操作）。
func (ps *PackStore) PutIfAbsent(key string, r io.Reader, size int64) error {
	return ps.base.PutIfAbsent(key, r, size)
}

// PutIfMatch 实现 BlobStore。
func (ps *PackStore) PutIfMatch(key, etag string, data []byte) (string, error) {
	return ps.base.PutIfMatch(key, etag, data)
}

// Delete 实现 BlobStore。已打包的对象**删除松散副本是 no-op**（对象在 pack
// 里，单个删除需要 repack；GC 在「整个 pack 全不可达」时走 DeletePack）。
func (ps *PackStore) Delete(key string) error {
	return ps.base.Delete(key)
}

// Flush 实现 BlobStore。
func (ps *PackStore) Flush() error { return ps.base.Flush() }

// ListPrefix 实现 BlobStore：`objects/` 前缀同时返回松散对象与打包对象。
func (ps *PackStore) ListPrefix(prefix string) ([]Stat, error) {
	out, err := ps.base.ListPrefix(prefix)
	if err != nil {
		return nil, err
	}
	if !(prefix == "" || strings.HasPrefix(prefix, "objects/") || strings.HasPrefix("objects/", prefix)) {
		return out, nil
	}
	if err := ps.refreshPacks(); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(out))
	for _, st := range out {
		seen[st.Key] = struct{}{}
	}
	ps.mu.RLock()
	for id, loc := range ps.locs {
		key := keyFor(id)
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		out = append(out, Stat{Key: key, Size: loc.length, ETag: id.String()})
	}
	ps.mu.RUnlock()
	return out, nil
}

// Packs 返回已知 pack 的 id（升序）。
func (ps *PackStore) Packs() ([]hash.ID, error) {
	if err := ps.ensureLoaded(); err != nil {
		return nil, err
	}
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return append([]hash.ID(nil), ps.packs...), nil
}

// PackObjects 返回某个 pack 里的全部对象索引。
func (ps *PackStore) PackObjects(pid hash.ID) ([]packEntry, error) {
	if err := ps.ensureLoaded(); err != nil {
		return nil, err
	}
	st, err := ps.base.Stat(packKey(pid))
	if err != nil {
		return nil, err
	}
	return ps.readFooter(pid, st.Size)
}

// DeletePack 删除一个 pack 并清理位置表。
func (ps *PackStore) DeletePack(pid hash.ID) error {
	if err := ps.base.Delete(packKey(pid)); err != nil && !errors.Is(err, ErrNotExist) {
		return err
	}
	ps.mu.Lock()
	deleteSet := map[hash.ID]struct{}{}
	for id, loc := range ps.locs {
		if loc.pack == pid {
			delete(ps.locs, id)
			deleteSet[id] = struct{}{}
		}
	}
	kept := ps.packs[:0]
	for _, p := range ps.packs {
		if p != pid {
			kept = append(kept, p)
		}
	}
	ps.packs = kept
	ps.mu.Unlock()
	return nil
}

// PackResult 是一次离线打包的结果。
type PackResult struct {
	PacksCreated  int   `json:"packs_created"`
	ObjectsPacked int   `json:"objects_packed"`
	BytesPacked   int64 `json:"bytes_packed"`
	LooseRemoved  int   `json:"loose_removed"`
	// SkippedLarge 是因超过 MaxObjectBytes 而未打包的对象数。
	SkippedLarge int `json:"skipped_large"`
}

// PackLoose 把松散的小对象合并进 pack 文件，然后删除这些松散文件。
//
// 安全性：每写完一个 pack 并 Flush 之后才删对应的松散文件；任何一步失败都
// 保留松散副本，读者总能取到对象。
func (ps *PackStore) PackLoose(opt PackOptions) (PackResult, error) {
	opt = opt.withDefaults()
	var res PackResult

	if err := ps.ensureLoaded(); err != nil {
		return res, err
	}

	// 1. 收集可打包的松散对象（跳过已在 pack 里的重复副本与大对象）。
	stats, err := ps.base.ListPrefix("objects/")
	if err != nil {
		return res, err
	}
	type cand struct {
		id     hash.ID
		key    string
		typ    uint8
		size   int64
		stored []byte
	}
	var cands []cand
	for _, st := range stats {
		id, ok := objectIDFromKey(st.Key)
		if !ok {
			continue
		}
		if _, packed, err := ps.lookup(id); err != nil {
			return res, err
		} else if packed {
			continue // 已在 pack 里：松散副本留给后面的清理
		}
		if st.Size > opt.MaxObjectBytes {
			res.SkippedLarge++
			continue
		}
		typ, err := ps.baseHeadType(st.Key)
		if err != nil {
			return res, err
		}
		cands = append(cands, cand{id: id, key: st.Key, typ: typ, size: st.Size})
	}
	if len(cands) < opt.MinObjects {
		return res, nil
	}
	sort.Slice(cands, func(i, j int) bool {
		return bytes.Compare(cands[i].id[:], cands[j].id[:]) < 0
	})

	// 2. 分批构造 pack。
	for len(cands) > 0 {
		var (
			group []cand
			total int64
		)
		for len(cands) > 0 && (len(group) == 0 || total < opt.TargetPackBytes) {
			c := cands[0]
			cands = cands[1:]
			group = append(group, c)
			total += c.size
		}
		// 读取存储字节。
		ids := make([]hash.ID, len(group))
		stored := make([][]byte, len(group))
		types := make([]uint8, len(group))
		for i, c := range group {
			b, err := ps.base.GetRange(c.key, 0, c.size)
			if err != nil {
				return res, err
			}
			if int64(len(b)) != c.size {
				return res, fmt.Errorf("%w: 松散对象 %s 长度不符", ErrCorrupt, c.id)
			}
			ids[i], stored[i], types[i] = c.id, b, c.typ
		}
		packBytes := encodePack(ids, stored, types)
		pid := hash.Sum(packBytes)
		pkey := packKey(pid)
		if err := ps.base.PutIfAbsent(pkey, bytes.NewReader(packBytes), int64(len(packBytes))); err != nil && !errors.Is(err, ErrExists) {
			return res, err
		}
		// pack 必须先落盘，再删松散副本。
		if err := ps.base.Flush(); err != nil {
			return res, err
		}

		// 登记位置。
		entries, err := ps.readFooter(pid, int64(len(packBytes)))
		if err != nil {
			return res, err
		}
		ps.mu.Lock()
		for _, en := range entries {
			if _, dup := ps.locs[en.id]; dup {
				continue
			}
			ps.locs[en.id] = packLoc{pack: pid, off: en.off, length: en.length, storedSize: en.length, typ: en.typ}
		}
		if !ps.hasPackLocked(pid) {
			ps.packs = append(ps.packs, pid)
		}
		ps.mu.Unlock()

		res.PacksCreated++
		res.ObjectsPacked += len(group)
		res.BytesPacked += int64(len(packBytes))

		// 删除松散副本（pack 已在盘上并已登记）。
		for _, c := range group {
			if err := ps.base.Delete(c.key); err != nil && !errors.Is(err, ErrNotExist) {
				return res, err
			}
			res.LooseRemoved++
		}
	}
	// 清理松散对象留下的空分片目录（本地后端会占块）。
	if pr, ok := ps.base.(interface{ PruneEmptyDirs(string) error }); ok {
		if err := pr.PruneEmptyDirs("objects/"); err != nil {
			return res, err
		}
	}
	return res, nil
}

// hasPackLocked 报告 pid 是否已登记。调用方持写锁。
func (ps *PackStore) hasPackLocked(pid hash.ID) bool {
	for _, p := range ps.packs {
		if p == pid {
			return true
		}
	}
	return false
}

// baseHeadType 读取松散对象头部的类型字节。
func (ps *PackStore) baseHeadType(key string) (uint8, error) {
	hdr, err := ps.base.GetRange(key, 0, 2)
	if err != nil {
		return 0, err
	}
	if len(hdr) != 2 || hdr[0] != storedFormat {
		return 0, fmt.Errorf("%w: %s 头部非法", ErrCorrupt, key)
	}
	if hdr[1] < codec.TypeBlob || hdr[1] > codec.TypeVersion {
		return 0, fmt.Errorf("%w: %s 类型非法 0x%02x", ErrCorrupt, key, hdr[1])
	}
	return hdr[1], nil
}
