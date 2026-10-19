package remotesync

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/repo"
)

// FetchResult 是一次拉取/快进的结果。
type FetchResult struct {
	Downloaded      int    `json:"downloaded_packages"`
	ImportedObjects int    `json:"imported_objects"`
	RemoteHead      string `json:"remote_head"`
	LocalHead       string `json:"local_head"`
	UpToDate        bool   `json:"up_to_date"`
	FastForward     bool   `json:"fast_forward"`
	Diverged        bool   `json:"diverged"`
	Ahead           int    `json:"ahead"`
	Behind          int    `json:"behind"`
}

// Fetch 下载远端缺失的包并导入对象，更新远端跟踪 ref；**不移动本地 head**。
func Fetch(ctx context.Context, r *repo.Repo, remote Remote, opt Options) (*FetchResult, error) {
	res := &FetchResult{}
	ref, err := remote.GetRef(ctx)
	if err != nil {
		if errors.Is(err, ErrNoRef) {
			return finishFetch(r, res, "")
		}
		return nil, err
	}
	res.RemoteHead = ref.Head

	ids, err := remote.ListPackages(ctx)
	if err != nil {
		return nil, err
	}
	byResult := make(map[string]*Manifest, len(ids))
	for _, id := range ids {
		m, err := remote.GetManifest(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("remotesync: 读取远端清单 %s: %w", id, err)
		}
		if err := m.Verify(); err != nil {
			return nil, err
		}
		byResult[m.ResultVersion] = m
	}

	// 从远端 head 往回走，收集本地还缺的包（老的在前）。
	var need []*Manifest
	cur := ref.Head
	visited := map[string]bool{}
	for cur != "" && !isZeroVersion(cur) {
		if visited[cur] {
			return nil, fmt.Errorf("%w: 包链存在环", ErrCorruptPackage)
		}
		visited[cur] = true
		if _, err := r.DB.GetVersion(mustParse(cur)); err == nil {
			break // 本地已有这个版本（及其全部历史）
		}
		m, ok := byResult[cur]
		if !ok {
			return nil, fmt.Errorf("remotesync: 远端缺少版本 %s 对应的包", cur)
		}
		need = append([]*Manifest{m}, need...)
		cur = m.BaseVersion
	}
	if len(need) > 0 {
		first := need[0]
		if !isZeroVersion(first.BaseVersion) {
			if _, err := r.DB.GetVersion(mustParse(first.BaseVersion)); err != nil {
				return nil, fmt.Errorf("remotesync: 缺少 base 版本 %s（需要 checkpoint 包）", first.BaseVersion)
			}
		}
	}

	for _, m := range need {
		tmp, err := os.MkdirTemp("", "p37sync-")
		if err != nil {
			return nil, err
		}
		_, imp, err := func() (*Manifest, int, error) {
			if _, err := remote.GetPackage(ctx, m.ID, tmp); err != nil {
				return nil, 0, err
			}
			return ImportDir(r, tmp)
		}()
		_ = os.RemoveAll(tmp)
		if err != nil {
			return nil, fmt.Errorf("remotesync: 应用包 %s: %w", m.ID, err)
		}
		res.Downloaded++
		res.ImportedObjects += imp
	}

	return finishFetch(r, res, ref.Head)
}

// Pull = Fetch + 仅快进。分叉时保持本地 head 不变并如实报告。
func Pull(ctx context.Context, r *repo.Repo, remote Remote, opt Options) (*FetchResult, error) {
	res, err := Fetch(ctx, r, remote, opt)
	if err != nil {
		return nil, err
	}
	if res.UpToDate {
		return res, nil
	}
	if res.Diverged {
		// 空仓库的初始 v0 与远端首版本必然不同 id（时间戳不同），但它没有任何
		// 用户内容 —— 视为空仓库，允许快进（与 git 在无提交仓库上 pull 一致）。
		empty, err := isInitOnlyHead(r)
		if err != nil {
			return nil, err
		}
		if !empty {
			return res, nil
		}
	} else if res.Ahead > 0 {
		// 本地有远端没有的提交（领先或分叉）：绝不移动本地 head。
		return res, nil
	}
	target := mustParse(res.RemoteHead)
	if target.IsZero() {
		return res, nil
	}
	if err := r.AdvanceHead(target, epochOf(r, target)); err != nil {
		return nil, err
	}
	res.LocalHead = target.String()
	res.FastForward = true
	res.Ahead = 0
	res.Behind = 0
	res.Diverged = false
	return res, nil
}

// finishFetch 更新远端跟踪状态并计算 ahead/behind。
func finishFetch(r *repo.Repo, res *FetchResult, remoteHead string) (*FetchResult, error) {
	head, ok, err := r.Head()
	if err != nil {
		return nil, err
	}
	if ok {
		res.LocalHead = head.String()
	}
	if remoteHead != "" {
		if err := r.DB.SetRemoteHead(remoteHead, meta.NowMS()); err != nil {
			return nil, err
		}
	}
	st, err := GetStatus(r, remoteHead, remoteHead != "")
	if err != nil {
		return nil, err
	}
	res.Ahead, res.Behind, res.Diverged = st.Ahead, st.Behind, st.Diverged
	res.UpToDate = res.LocalHead != "" && res.LocalHead == res.RemoteHead
	if res.LocalHead == "" && res.RemoteHead == "" {
		res.UpToDate = true
	}
	return res, nil
}

func epochOf(r *repo.Repo, id hash.ID) uint64 {
	if v, err := r.DB.GetVersion(id); err == nil {
		return v.Epoch
	}
	return 0
}

// isInitOnlyHead 报告本地是否只有仓库初始化产生的空版本（没有用户历史）。
func isInitOnlyHead(r *repo.Repo) (bool, error) {
	head, ok, err := r.Head()
	if err != nil {
		return false, err
	}
	if !ok {
		return true, nil
	}
	v, err := r.DB.GetVersion(head)
	if err != nil {
		return false, err
	}
	return v.Parent.IsZero() && v.Agent == "p37-init", nil
}
