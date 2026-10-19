package remotesync

import (
	"context"
	"errors"
	"fmt"

	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/repo"
)

// PushResult 是一次推送的结果。
type PushResult struct {
	Uploaded   []string `json:"uploaded"`
	RemoteHead string   `json:"remote_head"`
	Bytes      int64    `json:"bytes"`
}

// Push 把本地暂存包按链顺序上传，最后条件更新远端 ref。
//
// 只有 ref 更新成功后才把包标记为已上传；中途失败可安全重试（Put 幂等）。
func Push(ctx context.Context, r *repo.Repo, remote Remote, ids []string) (*PushResult, error) {
	rows, err := r.DB.ListPackages()
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var todo []meta.PackageRow
	for _, row := range rows {
		if len(want) > 0 {
			if want[row.ID] {
				todo = append(todo, row)
			}
			continue
		}
		if !row.Uploaded {
			todo = append(todo, row)
		}
	}
	if len(todo) == 0 {
		return nil, ErrUpToDate
	}

	ref, err := remote.GetRef(ctx)
	if err != nil {
		if !errors.Is(err, ErrNoRef) {
			return nil, err
		}
		ref = Ref{}
	}

	res := &PushResult{}
	cur := ref.Head
	lastID := ref.Package
	for _, row := range todo {
		if !sameVersion(row.BaseVersion, cur) {
			return nil, fmt.Errorf("%w: 包 %s 的 base=%s，远端 head=%s",
				ErrRemoteMoved, row.ID, row.BaseVersion, cur)
		}
		m, err := ReadStaged(row)
		if err != nil {
			return nil, err
		}
		if m.ResultVersion != row.ResultVersion {
			return nil, fmt.Errorf("remotesync: 包 %s 的清单与记录不一致", row.ID)
		}
		if err := remote.PutPackage(ctx, m, row.LocalPath); err != nil {
			return nil, err
		}
		res.Uploaded = append(res.Uploaded, row.ID)
		res.Bytes += row.ByteSize
		cur = row.ResultVersion
		lastID = row.ID
	}

	next := Ref{Head: cur, Package: lastID, TS: meta.NowMS()}
	if err := remote.UpdateRef(ctx, ref, next); err != nil {
		return nil, err
	}
	for _, id := range res.Uploaded {
		if err := r.DB.MarkPackageUploaded(id); err != nil {
			return nil, err
		}
	}
	st, err := r.DB.GetSyncState()
	if err != nil {
		return nil, err
	}
	st.RemoteHead = cur
	st.LastPackageID = lastID
	st.LastSyncTS = next.TS
	if err := r.DB.SetSyncState(st); err != nil {
		return nil, err
	}
	res.RemoteHead = cur
	return res, nil
}

// sameVersion 比较两个版本字符串，把空串与全零视为同一个「无」。
func sameVersion(a, b string) bool {
	return normVersion(a) == normVersion(b)
}

func normVersion(s string) string {
	if s == "" {
		return hash.Zero.String()
	}
	if id, err := hash.Parse(s); err == nil {
		return id.String()
	}
	return s
}

func isZeroVersion(s string) bool {
	return normVersion(s) == hash.Zero.String()
}
