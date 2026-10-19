package remotesync

import (
	"context"
	"errors"
)

// PruneRemote 回收远端旧包：保留「最新一个 checkpoint 及其之后的 delta」，
// 删除更早的包。
//
// 安全性：checkpoint 含全量对象闭包，因此删掉它之前的 delta 不会丢数据。
// 远端没有 checkpoint 时不做任何删除（返回 0）。
func PruneRemote(ctx context.Context, remote Remote) (int, int64, error) {
	ref, err := remote.GetRef(ctx)
	if err != nil {
		if errors.Is(err, ErrNoRef) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	ids, err := remote.ListPackages(ctx)
	if err != nil {
		return 0, 0, err
	}
	byResult := make(map[string]*Manifest, len(ids))
	for _, id := range ids {
		m, err := remote.GetManifest(ctx, id)
		if err != nil {
			return 0, 0, err
		}
		byResult[m.ResultVersion] = m
	}

	// 从 head 往回走，得到远端包链（新 -> 旧）。
	var chain []*Manifest
	cur := ref.Head
	seen := map[string]bool{}
	for cur != "" && !isZeroVersion(cur) {
		if seen[cur] {
			break
		}
		seen[cur] = true
		m, ok := byResult[cur]
		if !ok {
			break
		}
		chain = append(chain, m)
		cur = m.BaseVersion
	}

	keep := len(chain)
	foundCheckpoint := false
	for i, m := range chain {
		if m.Kind == "checkpoint" {
			keep = i + 1
			foundCheckpoint = true
			break
		}
	}
	if !foundCheckpoint {
		return 0, 0, nil // 没有 checkpoint：删任何一个包都可能丢历史
	}

	keepSet := make(map[string]bool, keep)
	var bytes int64
	for _, m := range chain[:keep] {
		keepSet[m.ID] = true
	}
	deleted := 0
	for _, m := range chain[keep:] {
		if keepSet[m.ID] {
			continue
		}
		if err := remote.DeletePackage(ctx, m.ID); err != nil {
			return deleted, bytes, err
		}
		deleted++
		bytes += m.PayloadBytes
	}
	return deleted, bytes, nil
}
