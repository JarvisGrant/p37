package remotesync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/chenyiwen/p37/internal/cas"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/repo"
)

// Prepare 生成一个同步包并**只写到本地 outbox**（不上传）。
//
// full 为真时强制生成 checkpoint；force 为真时忽略最小版本/字节阈值。
func Prepare(r *repo.Repo, opt Options, force, full bool) (*Manifest, error) {
	opt = opt.withDefaults(r.Root)
	var out *Manifest
	err := r.WithExclusive(func() error {
		m, err := prepareLocked(r, opt, force, full)
		if err != nil {
			return err
		}
		out = m
		return nil
	})
	return out, err
}

func prepareLocked(r *repo.Repo, opt Options, force, full bool) (*Manifest, error) {
	head, ok, err := r.Head()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("remotesync: 主干尚未初始化")
	}
	base, err := baseForPrepare(r)
	if err != nil {
		return nil, err
	}
	if base == head {
		return nil, ErrUpToDate
	}

	chain, err := r.DB.VersionChain(head) // head -> 最老
	if err != nil {
		return nil, err
	}
	baseIdx := -1
	if !base.IsZero() {
		for i, id := range chain {
			if id == base {
				baseIdx = i
				break
			}
		}
	}

	kind := "delta"
	switch {
	case full, base.IsZero(), baseIdx < 0:
		if !full && !base.IsZero() && baseIdx < 0 {
			return nil, ErrNotAncestor
		}
		kind = "checkpoint"
	case opt.CheckpointEvery > 0 && baseIdx >= opt.CheckpointEvery:
		kind = "checkpoint"
	}

	var newChain []hash.ID // 从新到旧
	if kind == "checkpoint" {
		newChain = chain
	} else {
		newChain = chain[:baseIdx] // head..(base 不含)
	}
	if len(newChain) == 0 {
		return nil, ErrUpToDate
	}
	// 低频策略只约束增量：首次 checkpoint（空远端）总是生成，否则新仓库
	// 版本数不到阈值就永远无法开始同步。
	if !force && kind == "delta" && len(newChain) < opt.MinVersions {
		return nil, ErrNothingToPackage
	}

	newVersions := reverseIDs(newChain)
	newSet, err := collectVersionObjects(r, newVersions)
	if err != nil {
		return nil, err
	}
	if kind == "delta" {
		baseSet, err := collectVersionObjects(r, chain[baseIdx:]) // base 及更老
		if err != nil {
			return nil, err
		}
		for id := range baseSet {
			delete(newSet, id)
		}
	}
	if len(newSet) == 0 {
		return nil, ErrUpToDate
	}

	ids := make([]hash.ID, 0, len(newSet))
	for id := range newSet {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })

	built, err := buildResultFor(r, ids, opt)
	if err != nil {
		return nil, err
	}
	var payloadBytes int64
	for _, p := range built.parts {
		payloadBytes += p.Bytes
	}
	if !force && kind == "delta" && payloadBytes < opt.MinBytes {
		return nil, ErrNothingToPackage
	}

	st, err := r.Status()
	if err != nil {
		return nil, err
	}
	pm, err := collectMeta(r, newVersions)
	if err != nil {
		return nil, err
	}
	metaBytes, err := pm.Encode()
	if err != nil {
		return nil, err
	}
	m := &Manifest{
		Format:        FormatVersion,
		Kind:          kind,
		BaseVersion:   base.String(),
		ResultVersion: head.String(),
		Epoch:         uint64(st.Epoch),
		CreatedTS:     meta.NowMS(),
		ObjectCount:   len(ids),
		RawBytes:      built.raw,
		PayloadBytes:  payloadBytes,
		PayloadHash:   PayloadHash(built.parts),
		MetaBytes:     int64(len(metaBytes)),
		MetaHash:      hash.Sum(metaBytes).String(),
		Versions:      versionStrings(newVersions),
		Parts:         built.parts,
	}
	m.ID = ComputeID(m)

	dir := filepath.Join(opt.Outbox, m.ID)
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := m.WriteDir(dir, metaBytes, built.bodies); err != nil {
		return nil, err
	}
	if err := r.DB.InsertPackage(meta.PackageRow{
		ID: m.ID, Kind: m.Kind, BaseVersion: m.BaseVersion,
		ResultVersion: m.ResultVersion, Epoch: m.Epoch, CreatedTS: m.CreatedTS,
		ObjectCount: m.ObjectCount, ByteSize: m.PayloadBytes,
		LocalPath: dir, Uploaded: false,
	}); err != nil {
		return nil, err
	}
	return m, nil
}

// buildResult 保存分片字节，供写盘使用。
type buildResult struct {
	parts  []Part
	bodies map[string][]byte
	raw    int64
}

// buildResultFor 把对象集合切成若干 payload 分片（整体 zstd 压缩）。
func buildResultFor(r *repo.Repo, ids []hash.ID, opt Options) (*buildResult, error) {
	res := &buildResult{bodies: make(map[string][]byte)}
	var (
		cur      []payloadObject
		curBytes int64
	)
	flush := func() {
		if len(cur) == 0 {
			return
		}
		b := encodePayload(cur, opt.CompressLevel)
		name := fmt.Sprintf("payload.part%03d", len(res.parts))
		res.parts = append(res.parts, Part{
			Name: name, Hash: hash.Sum(b).String(),
			Bytes: int64(len(b)), Objects: len(cur),
		})
		res.bodies[name] = b
		cur, curBytes = nil, 0
	}

	for _, id := range ids {
		stored, err := r.Store.GetStored(id)
		if err != nil {
			return nil, fmt.Errorf("remotesync: 读取对象 %s: %w", id, err)
		}
		typ, raw, err := cas.DecodeStored(stored)
		if err != nil {
			return nil, err
		}
		if hash.Sum(raw) != id {
			return nil, fmt.Errorf("remotesync: 对象 %s 内容不符", id)
		}
		res.raw += int64(len(raw))
		if curBytes > 0 && curBytes+int64(len(raw)) > opt.MaxPackageBytes {
			flush()
		}
		cur = append(cur, payloadObject{id: id, typ: typ, raw: raw})
		curBytes += int64(len(raw))
	}
	flush()

	// 只有一个分片时用更直观的名字。
	if len(res.parts) == 1 {
		old := res.parts[0].Name
		res.parts[0].Name = "payload.pack"
		res.bodies["payload.pack"] = res.bodies[old]
		delete(res.bodies, old)
	}
	return res, nil
}

func baseForPrepare(r *repo.Repo) (hash.ID, error) {
	if p, err := r.DB.LastPackage(); err == nil {
		if p.ResultVersion != "" {
			return hash.Parse(p.ResultVersion)
		}
	} else if !errors.Is(err, meta.ErrNotExist) {
		return hash.ID{}, err
	}
	st, err := r.DB.GetSyncState()
	if err != nil {
		return hash.ID{}, err
	}
	if st.RemoteHead == "" {
		return hash.ID{}, nil
	}
	return hash.Parse(st.RemoteHead)
}

func reverseIDs(in []hash.ID) []hash.ID {
	out := make([]hash.ID, len(in))
	for i := range in {
		out[i] = in[len(in)-1-i]
	}
	return out
}

func versionStrings(ids []hash.ID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}
