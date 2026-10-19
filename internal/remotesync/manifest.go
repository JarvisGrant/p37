package remotesync

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/chenyiwen/p37/internal/hash"
)

// FormatVersion 是同步包格式版本（冻结）。
const FormatVersion = 1

// Part 是包的一个 payload 分片（每个分片本身是一个完整的 P37PACK1 pack）。
type Part struct {
	Name    string `json:"name"`
	Hash    string `json:"hash"`
	Bytes   int64  `json:"bytes"`
	Objects int    `json:"objects"`
}

// Manifest 是一个同步增量包的清单（落盘为 manifest.json，人可读）。
type Manifest struct {
	Format        int      `json:"format"`
	ID            string   `json:"id"`
	Kind          string   `json:"kind"` // checkpoint | delta
	BaseVersion   string   `json:"base_version"`
	ResultVersion string   `json:"result_version"`
	Epoch         uint64   `json:"epoch"`
	CreatedTS     int64    `json:"created_ts"`
	ObjectCount   int      `json:"object_count"`
	RawBytes      int64    `json:"raw_bytes"`
	PayloadBytes  int64    `json:"payload_bytes"`
	PayloadHash   string   `json:"payload_hash"`
	MetaBytes     int64    `json:"meta_bytes"`
	MetaHash      string   `json:"meta_hash"`
	Versions      []string `json:"versions"`
	Parts         []Part   `json:"parts"`
}

// ManifestName 是清单文件名。
const ManifestName = "manifest.json"

// ErrCorruptPackage 表示包内容与清单不符。
var ErrCorruptPackage = errors.New("remotesync: 包损坏")

// ComputeID 由包的语义字段计算内容标识。
//
// 不含 ID/CreatedTS 之外的可变字段以外的内容；payload 分片的哈希进 ID，
// 因此同一组对象 + 同一 base/result 得到同一个包 id。
func ComputeID(m *Manifest) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "p37pkg1|%s|%s|%s|%s|", m.Kind, m.BaseVersion, m.ResultVersion, m.MetaHash)
	for _, p := range m.Parts {
		fmt.Fprintf(&sb, "%s:%d;", p.Hash, p.Bytes)
	}
	return hash.Sum([]byte(sb.String())).String()
}

// Verify 校验清单自身的一致性（格式、id、分片统计）。
func (m *Manifest) Verify() error {
	if m.Format != FormatVersion {
		return fmt.Errorf("%w: 未知格式 %d", ErrCorruptPackage, m.Format)
	}
	if m.Kind != "checkpoint" && m.Kind != "delta" {
		return fmt.Errorf("%w: 未知 kind %q", ErrCorruptPackage, m.Kind)
	}
	if m.ResultVersion == "" {
		return fmt.Errorf("%w: 缺少 result_version", ErrCorruptPackage)
	}
	if len(m.Parts) == 0 {
		return fmt.Errorf("%w: 没有 payload 分片", ErrCorruptPackage)
	}
	if m.MetaHash == "" || m.MetaBytes <= 0 {
		return fmt.Errorf("%w: 缺少 meta.json 信息", ErrCorruptPackage)
	}
	var total int64
	var objs int
	for _, p := range m.Parts {
		if p.Name == "" || p.Hash == "" || p.Bytes <= 0 {
			return fmt.Errorf("%w: 分片信息不完整", ErrCorruptPackage)
		}
		total += p.Bytes
		objs += p.Objects
	}
	if total != m.PayloadBytes {
		return fmt.Errorf("%w: payload_bytes %d != 分片合计 %d", ErrCorruptPackage, m.PayloadBytes, total)
	}
	if objs != m.ObjectCount {
		return fmt.Errorf("%w: object_count %d != 分片合计 %d", ErrCorruptPackage, m.ObjectCount, objs)
	}
	if m.ID == "" {
		return fmt.Errorf("%w: 缺少 id", ErrCorruptPackage)
	}
	if got := ComputeID(m); got != m.ID {
		return fmt.Errorf("%w: id 不符（清单 %s，重算 %s）", ErrCorruptPackage, m.ID, got)
	}
	return nil
}

// Encode 返回确定性 JSON（用于落盘/上传）。字段顺序固定，便于审查。
func (m *Manifest) Encode() ([]byte, error) {
	if m.Versions == nil {
		m.Versions = []string{}
	}
	if m.Parts == nil {
		m.Parts = []Part{}
	}
	return json.MarshalIndent(m, "", "  ")
}

// DecodeManifest 解析 manifest.json。
func DecodeManifest(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptPackage, err)
	}
	if err := m.Verify(); err != nil {
		return nil, err
	}
	return &m, nil
}

// ReadManifest 读取包目录里的 manifest.json。
func ReadManifest(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		return nil, err
	}
	return DecodeManifest(b)
}

// WriteDir 把清单与全部分片写进 dir。
func (m *Manifest) WriteDir(dir string, metaBytes []byte, parts map[string][]byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, MetaName), metaBytes, 0o600); err != nil {
		return err
	}
	for _, p := range m.Parts {
		b, ok := parts[p.Name]
		if !ok {
			return fmt.Errorf("remotesync: 缺少分片 %s", p.Name)
		}
		if err := os.WriteFile(filepath.Join(dir, p.Name), b, 0o600); err != nil {
			return err
		}
	}
	enc, err := m.Encode()
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ManifestName), enc, 0o600)
}

// PayloadHash 返回「分片哈希按顺序拼接后」的哈希（用于 ID 与快速比对）。
func PayloadHash(parts []Part) string {
	parts2 := make([]string, 0, len(parts))
	for _, p := range parts {
		parts2 = append(parts2, p.Hash)
	}
	return hash.Sum([]byte(strings.Join(parts2, ""))).String()
}
