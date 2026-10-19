package remotesync

import (
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/repo"
)

// Status 是同步状态（MCP sync_status / CLI sync status）。
type Status struct {
	LocalHead        string `json:"local_head"`
	RemoteHead       string `json:"remote_head"`
	RemoteConfigured bool   `json:"remote_configured"`
	PendingPackages  int64  `json:"pending_packages"`
	LastSyncTS       int64  `json:"last_sync_ts"`
	Ahead            int    `json:"ahead"`
	Behind           int    `json:"behind"`
	Diverged         bool   `json:"diverged"`
}

// GetStatus 汇总本地状态。remoteHead 为空表示尚未同步/未配置。
func GetStatus(r *repo.Repo, remoteHead string, configured bool) (*Status, error) {
	head, ok, err := r.Head()
	if err != nil {
		return nil, err
	}
	pend, err := r.DB.CountPendingPackages()
	if err != nil {
		return nil, err
	}
	st, err := r.DB.GetSyncState()
	if err != nil {
		return nil, err
	}
	out := &Status{
		RemoteConfigured: configured,
		PendingPackages:  pend,
		LastSyncTS:       st.LastSyncTS,
	}
	if ok {
		out.LocalHead = head.String()
	}
	if remoteHead == "" {
		remoteHead = st.RemoteHead
	}
	out.RemoteHead = remoteHead
	if err := fillDivergence(r, out); err != nil {
		return nil, err
	}
	return out, nil
}

func fillDivergence(r *repo.Repo, st *Status) error {
	if st.LocalHead == "" && st.RemoteHead == "" {
		return nil
	}
	if st.RemoteHead == "" {
		if st.LocalHead != "" {
			chain, err := r.DB.VersionChain(mustParse(st.LocalHead))
			if err != nil {
				return err
			}
			st.Ahead = len(chain)
		}
		return nil
	}
	remoteChain, err := r.DB.VersionChain(mustParse(st.RemoteHead))
	if err != nil {
		// 远端版本不在本地（还没 fetch）：无法算 ahead/behind，视为分叉。
		st.Diverged = st.LocalHead != st.RemoteHead
		return nil
	}
	remotePos := make(map[hash.ID]int, len(remoteChain))
	for i, id := range remoteChain {
		remotePos[id] = i
	}
	if st.LocalHead == "" {
		st.Behind = len(remoteChain)
		return nil
	}
	localChain, err := r.DB.VersionChain(mustParse(st.LocalHead))
	if err != nil {
		return err
	}
	for i, id := range localChain {
		if j, ok := remotePos[id]; ok {
			st.Ahead, st.Behind = i, j
			// 双方各自都有对方没有的提交 -> 分叉（有共同祖先也算）。
			st.Diverged = i > 0 && j > 0
			return nil
		}
	}
	st.Diverged = true
	return nil
}

func mustParse(s string) hash.ID {
	id, err := hash.Parse(s)
	if err != nil {
		return hash.ID{}
	}
	return id
}
