package remotesync

import (
	"os"
	"path/filepath"
	"sort"

	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/repo"
)

// lineHeads 返回全部未删除 line 的 head。没有 line 时退回默认 head。
func lineHeads(r *repo.Repo) (map[string]hash.ID, error) {
	ls, err := r.Lines()
	if err != nil {
		return nil, err
	}
	out := map[string]hash.ID{}
	for i := range ls {
		out[ls[i].Name] = ls[i].Head
	}
	return out, nil
}

// reachableVersions 返回从 roots 可达的版本，按 ts 升序（旧→新）。
func reachableVersions(r *repo.Repo, roots []hash.ID) ([]hash.ID, error) {
	set, err := r.DB.Reachable(roots)
	if err != nil {
		return nil, err
	}
	type item struct {
		id hash.ID
		ts int64
	}
	items := make([]item, 0, len(set))
	for id := range set {
		v, err := r.DB.GetVersion(id)
		if err != nil {
			continue
		}
		items = append(items, item{id, v.TS})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].ts != items[j].ts {
			return items[i].ts < items[j].ts
		}
		return items[i].id.String() < items[j].id.String()
	})
	out := make([]hash.ID, len(items))
	for i := range items {
		out[i] = items[i].id
	}
	return out, nil
}

// prepareMultiCheckpoint 为多 line 仓库生成全量 checkpoint。
//
// 多 line 下增量包的 base 语义会变得复杂（多个 head、merge 版本），本轮先用
// 全量 checkpoint 保证恢复正确；后续可在同一格式上做按 line 的增量。
func prepareMultiCheckpoint(r *repo.Repo, opt Options, force bool) (*Manifest, error) {
	heads, err := lineHeads(r)
	if err != nil {
		return nil, err
	}
	if len(heads) <= 1 {
		return nil, nil
	}
	head, ok, err := r.Head()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, repo.ErrNotInitialized
	}
	roots := make([]hash.ID, 0, len(heads))
	for _, h := range heads {
		roots = append(roots, h)
	}
	all, err := reachableVersions(r, roots)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, ErrUpToDate
	}
	base, err := baseForPrepare(r)
	if err != nil {
		return nil, err
	}
	set, err := collectVersionObjects(r, all)
	if err != nil {
		return nil, err
	}
	ids := make([]hash.ID, 0, len(set))
	for id := range set {
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
	st, err := r.Status()
	if err != nil {
		return nil, err
	}
	pm, err := collectMeta(r, all)
	if err != nil {
		return nil, err
	}
	metaBytes, err := pm.Encode()
	if err != nil {
		return nil, err
	}
	m := &Manifest{
		Format: FormatVersion, Kind: "checkpoint",
		BaseVersion: base.String(), ResultVersion: head.String(),
		Epoch: uint64(st.Epoch), CreatedTS: meta.NowMS(),
		ObjectCount: len(ids), RawBytes: built.raw, PayloadBytes: payloadBytes,
		PayloadHash: PayloadHash(built.parts), MetaBytes: int64(len(metaBytes)),
		MetaHash: hash.Sum(metaBytes).String(), Versions: versionStrings(all),
		Parts: built.parts,
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
