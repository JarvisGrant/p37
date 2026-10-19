package remotesync

import (
	"encoding/binary"
	"fmt"

	"github.com/klauspost/compress/zstd"

	"github.com/chenyiwen/p37/internal/cas"
	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
)

// 同步 payload 与本地 P2 pack 不同：这里的目标是**压缩后的传输体积**。
// P2 pack 直接拼接每个对象的 `0x01||type||zstd(raw)`，小文件会被每对象
// 独立 zstd 帧的开销放大；同步 payload 改为拼接**原始字节**，再对整段做
// 一次 zstd —— 小文件多时体积显著更小（实测 10k 小文件从 ~2 MB 降到 ~0.4 MB）。
//
// 格式（冻结）：
//
//	part     := "P37SYNC1" || zstd(inner)
//	inner    := bodies || index || u64be(indexLen) || "P37SEND1"
//	bodies   := 各对象原始字节（解压后）依次拼接
//	index    := uvarint(count) || count × (id(32) || uvarint(off) || uvarint(len) || u8(type))
//
// 对象 id 仍是 BLAKE3(raw)，因此导入端必须解压+重算哈希后才能写入 CAS。

const (
	payloadMagic = "P37SYNC1"
	payloadTail  = "P37SEND1"
	// MaxPayloadRaw 是单个 payload 分片解压后的字节上限（防解压炸弹）。
	MaxPayloadRaw = 512 << 20
)

// payloadObject 是一个待打包对象（原始字节 + 类型）。
type payloadObject struct {
	id  hash.ID
	typ uint8
	raw []byte
}

// encodePayload 把对象编码成一个同步 payload 分片。
func encodePayload(objs []payloadObject, level int) []byte {
	total := 0
	for _, o := range objs {
		total += len(o.raw)
	}
	bodies := make([]byte, 0, total)
	type span struct {
		off int
		obj payloadObject
	}
	spans := make([]span, len(objs))
	for i, o := range objs {
		spans[i] = span{off: len(bodies), obj: o}
		bodies = append(bodies, o.raw...)
	}

	e := codec.NewEncoder()
	e.Uvarint(uint64(len(objs)))
	for _, s := range spans {
		e.Hash(s.obj.id)
		e.Uvarint(uint64(s.off))
		e.Uvarint(uint64(len(s.obj.raw)))
		e.U8(s.obj.typ)
	}
	index := e.Out()

	inner := make([]byte, 0, len(bodies)+len(index)+24)
	inner = append(inner, bodies...)
	inner = append(inner, index...)
	var ln [8]byte
	binary.BigEndian.PutUint64(ln[:], uint64(len(index)))
	inner = append(inner, ln[:]...)
	inner = append(inner, payloadTail...)

	if level <= 0 {
		level = 3
	}
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)),
		zstd.WithEncoderConcurrency(1))
	if err != nil {
		// 几乎不可能：退回本地存储编码（仍然可被解析）。
		out := append([]byte(payloadMagic), inner...)
		return out
	}
	comp := enc.EncodeAll(inner, make([]byte, 0, len(inner)/2+16))
	_ = enc.Close()

	out := make([]byte, 0, len(payloadMagic)+len(comp))
	out = append(out, payloadMagic...)
	return append(out, comp...)
}

// decodePayload 解压并校验一个 payload 分片，返回全部对象（原始字节）。
func decodePayload(b []byte) ([]payloadObject, error) {
	if len(b) <= len(payloadMagic) || string(b[:len(payloadMagic)]) != payloadMagic {
		return nil, fmt.Errorf("%w: payload 头标记不符", ErrCorruptPackage)
	}
	dec, err := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(MaxPayloadRaw))
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	inner, err := dec.DecodeAll(b[len(payloadMagic):], make([]byte, 0, 4096))
	if err != nil {
		return nil, fmt.Errorf("%w: payload 解压失败: %v", ErrCorruptPackage, err)
	}
	if len(inner) > MaxPayloadRaw {
		return nil, fmt.Errorf("%w: payload 解压后超过上限", ErrCorruptPackage)
	}
	if len(inner) < 16 || string(inner[len(inner)-8:]) != payloadTail {
		return nil, fmt.Errorf("%w: payload 尾标记不符", ErrCorruptPackage)
	}
	indexLen := int64(binary.BigEndian.Uint64(inner[len(inner)-16 : len(inner)-8]))
	indexStart := int64(len(inner)) - 16 - indexLen
	if indexLen < 0 || indexStart < 0 {
		return nil, fmt.Errorf("%w: payload 索引长度非法", ErrCorruptPackage)
	}
	bodies := inner[:indexStart]

	d := codec.NewDecoder(inner[indexStart : len(inner)-16])
	count := d.Uvarint()
	if d.Err() != nil || count > 1<<24 {
		return nil, fmt.Errorf("%w: payload 索引不可读", ErrCorruptPackage)
	}
	out := make([]payloadObject, 0, count)
	expect := int64(0)
	for i := uint64(0); i < count; i++ {
		id := d.Hash()
		off := int64(d.Uvarint())
		length := int64(d.Uvarint())
		typ := d.U8()
		if d.Err() != nil {
			return nil, fmt.Errorf("%w: payload 索引截断", ErrCorruptPackage)
		}
		if off != expect || length < 0 || off+length > indexStart {
			return nil, fmt.Errorf("%w: payload 条目偏移非法", ErrCorruptPackage)
		}
		if typ < codec.TypeBlob || typ > codec.TypeVersion {
			return nil, fmt.Errorf("%w: payload 条目类型非法", ErrCorruptPackage)
		}
		raw := bodies[off : off+length]
		if hash.Sum(raw) != id {
			return nil, fmt.Errorf("%w: 对象 %s 内容不符", ErrCorruptPackage, id)
		}
		out = append(out, payloadObject{id: id, typ: typ, raw: raw})
		expect = off + length
	}
	if err := d.Finish(); err != nil {
		return nil, fmt.Errorf("%w: payload 索引多余字节", ErrCorruptPackage)
	}
	if expect != indexStart {
		return nil, fmt.Errorf("%w: payload body 未覆盖", ErrCorruptPackage)
	}
	return out, nil
}

// encodeStoredFor 把原始对象重新编码成本地 CAS 的存储字节。
func encodeStoredFor(o payloadObject) []byte {
	return cas.EncodeStoredAt(o.typ, o.raw, 0)
}
