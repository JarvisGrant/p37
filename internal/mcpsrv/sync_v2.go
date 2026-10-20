package mcpsrv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/hub"
	"github.com/chenyiwen/p37/internal/remotesync"
	"github.com/chenyiwen/p37/internal/repo"
)

func (s *Server) registerSyncV2() {
	s.register("sync_push", "显式、持久地推送本地仓库到远端；成功返回 durable=true。",
		`{"type":"object","properties":{"repo":{"type":"string"},"all":{"type":"boolean"},"full":{"type":"boolean"},"lines":{"type":"array","items":{"type":"string"}},"expected_remote_generation":{"type":"integer"},"idempotency_key":{"type":"string"},"async":{"type":"boolean"}},"required":["repo"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				All                      bool     `json:"all"`
				Full                     bool     `json:"full"`
				Lines                    []string `json:"lines"`
				ExpectedRemoteGeneration int64    `json:"expected_remote_generation"`
				IdempotencyKey           string   `json:"idempotency_key"`
				Async                    bool     `json:"async"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			if a.Async {
				go func() { _, _ = pushRepo(context.Background(), rr, s.Hub, a.Full, 0) }()
				return map[string]any{"durable": false, "async": true}, nil
			}
			if a.IdempotencyKey != "" {
				if ir, _ := rr.DB.GetIdempotencyResult("sync_push", a.IdempotencyKey, time.Now().UnixMilli()); ir != nil {
					var out map[string]any
					if json.Unmarshal([]byte(ir.ResultJSON), &out) == nil {
						return out, nil
					}
				}
			}
			out, err := pushRepo(ctx, rr, s.Hub, a.Full, a.ExpectedRemoteGeneration)
			if err == nil && a.IdempotencyKey != "" {
				if b, merr := json.Marshal(out); merr == nil {
					now := time.Now().UnixMilli()
					_ = rr.DB.PutIdempotencyResult("sync_push", a.IdempotencyKey, string(b), now,
						now+int64(7*24*time.Hour/time.Millisecond))
				}
			}
			return out, err
		})

	s.register("sync_fetch", "下载远端对象与 refs；不移动本地 head。",
		`{"type":"object","properties":{"repo":{"type":"string"}},"required":["repo"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			remote, cfg, err := remoteFor(rr)
			if err != nil {
				return nil, err
			}
			if remote == nil {
				return nil, coded("sync_not_configured", "仓库未配置远端")
			}
			release, ok := rr.BeginSync()
			if !ok {
				return nil, coded("sync_in_progress", "该仓库已有同步在运行")
			}
			defer release()
			res, err := remotesync.Fetch(ctx, rr, remote, cfg.RemoteOptions())
			if err != nil {
				return nil, classifySyncErr(err)
			}
			return res, nil
		})

	s.register("sync_pull", "下载并快进本地 line；分叉时保留本地。",
		`{"type":"object","properties":{"repo":{"type":"string"}},"required":["repo"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			remote, cfg, err := remoteFor(rr)
			if err != nil {
				return nil, err
			}
			if remote == nil {
				return nil, coded("sync_not_configured", "仓库未配置远端")
			}
			release, ok := rr.BeginSync()
			if !ok {
				return nil, coded("sync_in_progress", "该仓库已有同步在运行")
			}
			defer release()
			res, err := remotesync.Pull(ctx, rr, remote, cfg.RemoteOptions())
			if err != nil {
				return nil, classifySyncErr(err)
			}
			return res, nil
		})

	s.register("sync_restore", "从远端前缀恢复仓库（可自建 repo）。",
		`{"type":"object","properties":{"repo":{"type":"string"},"name":{"type":"string"},"remote_prefix":{"type":"string"},"remote":{"type":"object"},"force":{"type":"boolean"},"idempotency_key":{"type":"string"}},"required":["repo","remote_prefix"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			if s.Hub == nil {
				return nil, coded("sync_not_configured", "单仓库模式请用 sync_pull")
			}
			var a struct {
				Repo         string          `json:"repo"`
				Name         string          `json:"name"`
				RemotePrefix string          `json:"remote_prefix"`
				Remote       json.RawMessage `json:"remote"`
				Force        bool            `json:"force"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			name := a.Name
			if name == "" {
				name = a.Repo
			}
			if name == "" {
				return nil, coded("repo_required", "缺少 repo/name")
			}
			// 解析 remote_key：前缀最后一段。
			prefix := strings.TrimSuffix(a.RemotePrefix, "/")
			key := filepath.Base(prefix)
			base := filepath.Dir(prefix)
			if base == "." || base == "/" {
				base = ""
			}
			// 打开或创建 repo。
			e, err := s.Hub.Get(name)
			var rr *repo.Repo
			if err != nil {
				e, err = s.Hub.Create(name, key, prefix)
				if err != nil {
					return nil, err
				}
			}
			rr, err = s.Hub.Open(e.ID)
			if err != nil {
				return nil, err
			}
			if !isEmptyRepo(rr) {
				if !a.Force {
					return nil, coded("repo_busy", "仓库非空，需要 force=true")
				}
				if _, err := s.Hub.Delete(e.ID, true); err != nil {
					return nil, err
				}
				e, err = s.Hub.CreateEx(name, "main", key, prefix)
				if err != nil {
					return nil, err
				}
				rr, err = s.Hub.Open(e.ID)
				if err != nil {
					return nil, err
				}
			}
			cfg, err := config.Load(rr.Root)
			if err != nil {
				return nil, err
			}
			cfg.Remote.Enabled = true
			if cfg.Remote.Backend == "" {
				cfg.Remote.Backend = "s3"
			}
			if len(a.Remote) > 0 && string(a.Remote) != "null" {
				if err := json.Unmarshal(a.Remote, &cfg.Remote); err != nil {
					return nil, coded("invalid_argument", "remote 配置解析失败: %v", err)
				}
			}
			cfg.Remote.RemoteKey = key
			if base != "" {
				cfg.Remote.Prefix = base
			}
			if err := config.Save(rr.Root, cfg); err != nil {
				return nil, err
			}
			_ = s.Hub.UpdateRemote(e.ID, key, prefix)
			remote, cfg2, err := remoteFor(rr)
			if err != nil {
				return nil, err
			}
			if remote == nil {
				return nil, coded("sync_not_configured", "远端未启用")
			}
			release, ok := rr.BeginSync()
			if !ok {
				return nil, coded("sync_in_progress", "该仓库已有同步在运行")
			}
			defer release()
			res, err := remotesync.Pull(ctx, rr, remote, cfg2.RemoteOptions())
			if err != nil {
				return nil, classifySyncErr(err)
			}
			return res, nil
		})

	s.register("sync_prune", "回收远端旧包（保留最新 checkpoint 及后续 delta）。",
		`{"type":"object","properties":{"repo":{"type":"string"},"dry_run":{"type":"boolean"},"remote":{"type":"boolean"},"local":{"type":"boolean"}},"required":["repo"],"additionalProperties":false}`,
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			rr, err := s.repoFor(repoArg(raw))
			if err != nil {
				return nil, err
			}
			var a struct {
				DryRun *bool `json:"dry_run"`
				Remote bool  `json:"remote"`
				Local  bool  `json:"local"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, coded("invalid_argument", "参数解析失败: %v", err)
			}
			dry := true
			if a.DryRun != nil {
				dry = *a.DryRun
			}
			if dry {
				return map[string]any{"dry_run": true, "remote": 0, "local": 0}, nil
			}
			remote, _, err := remoteFor(rr)
			if err != nil {
				return nil, err
			}
			n, bytes := 0, int64(0)
			if remote != nil && !a.Local {
				n, bytes, err = remotesync.PruneRemote(ctx, remote)
				if err != nil {
					return nil, err
				}
			}
			ln, lb, err := remotesync.PruneLocal(rr)
			if err != nil {
				return nil, err
			}
			return map[string]any{"dry_run": false, "remote": n, "remote_bytes": bytes,
				"local": ln, "local_bytes": lb}, nil
		})
}

func pushRepo(ctx context.Context, rr *repo.Repo, h *hub.Hub, full bool, expectedGen int64) (map[string]any, error) {
	release, ok := rr.BeginSync()
	if !ok {
		return nil, coded("sync_in_progress", "该仓库已有同步在运行")
	}
	defer release()
	remote, cfg, err := remoteFor(rr)
	if err != nil {
		return nil, err
	}
	if remote == nil {
		return nil, coded("sync_not_configured", "仓库未配置远端")
	}
	if expectedGen > 0 {
		if ref, err := remote.GetRef(ctx); err == nil && ref.Generation != expectedGen {
			return nil, coded("sync_remote_moved", "远端 generation=%d，期望 %d", ref.Generation, expectedGen)
		}
	}
	// 生成/复用 remote_key。
	if cfg.Remote.RemoteKey == "" {
		key, err := newRemoteKey()
		if err != nil {
			return nil, err
		}
		cfg.Remote.RemoteKey = key
		if err := config.Save(rr.Root, cfg); err != nil {
			return nil, err
		}
		if h != nil {
			es, _ := h.List()
			for i := range es {
				if es[i].Path == rr.Root {
					_ = h.UpdateRemote(es[i].ID, key, filepath.Join(cfg.Remote.Prefix, key))
					break
				}
			}
		}
		remote, cfg, err = remoteFor(rr)
		if err != nil {
			return nil, err
		}
	}
	m, err := remotesync.Prepare(rr, cfg.RemoteOptions(), true, full)
	if err != nil && !errors.Is(err, remotesync.ErrUpToDate) && !errors.Is(err, remotesync.ErrNothingToPackage) {
		return nil, err
	}
	refMeta := remotesync.Ref{RemoteKey: cfg.Remote.RemoteKey,
		RemotePrefix: filepath.Join(cfg.Remote.Prefix, cfg.Remote.RemoteKey),
		RepoName:     filepath.Base(rr.Root)}
	pr, err := remotesync.PushRef(ctx, rr, remote, nil, refMeta)
	if err != nil {
		if errors.Is(err, remotesync.ErrUpToDate) {
			ref, _ := remote.GetRef(ctx)
			return map[string]any{"durable": true, "already_pushed": true,
				"remote_key":        cfg.Remote.RemoteKey,
				"remote_prefix":     refMeta.RemotePrefix,
				"remote_generation": ref.Generation, "packages": []string{}}, nil
		}
		return nil, classifySyncErr(err)
	}
	ref, _ := remote.GetRef(ctx)
	out := map[string]any{
		"durable": true, "remote_key": cfg.Remote.RemoteKey,
		"remote_prefix": refMeta.RemotePrefix, "remote_generation": ref.Generation,
		"packages": pr.Uploaded, "bytes": pr.Bytes, "remote_head": pr.RemoteHead,
	}
	if m != nil {
		out["checkpoint"] = m.Kind == "checkpoint"
		out["package_id"] = m.ID
	}
	return out, nil
}

func newRemoteKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "rk_" + hex.EncodeToString(b[:]), nil
}

func remoteFor(rr *repo.Repo) (remotesync.Remote, config.Config, error) {
	cfg, err := config.Load(rr.Root)
	if err != nil {
		return nil, cfg, err
	}
	if !cfg.Remote.Enabled {
		return nil, cfg, nil
	}
	prefix := cfg.Remote.Prefix
	if cfg.Remote.RemoteKey != "" {
		if prefix == "" {
			prefix = cfg.Remote.RemoteKey
		} else {
			prefix = strings.Trim(prefix, "/") + "/" + cfg.Remote.RemoteKey
		}
	}
	switch cfg.Remote.Backend {
	case "dir":
		dir := cfg.Remote.Dir
		if dir == "" {
			return nil, cfg, errors.New("remote.dir 未配置")
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(rr.Root, dir)
		}
		if prefix != "" {
			dir = filepath.Join(dir, prefix)
		}
		r, err := remotesync.NewDirRemote(dir)
		return r, cfg, err
	case "s3", "":
		c := remotesync.S3ConfigFromEnv(remotesync.S3Config{
			Endpoint: cfg.Remote.Endpoint, Region: cfg.Remote.Region,
			Bucket: cfg.Remote.Bucket, Prefix: prefix, PathStyle: cfg.Remote.PathStyle,
		})
		if c.AccessKey == "" || c.SecretKey == "" {
			return nil, cfg, errors.New("缺少 S3 凭证：请设置 P37_S3_ACCESS_KEY / P37_S3_SECRET_KEY")
		}
		r, err := remotesync.NewS3Remote(context.Background(), c)
		return r, cfg, err
	}
	return nil, cfg, fmt.Errorf("未知 remote.backend %q", cfg.Remote.Backend)
}

func classifySyncErr(err error) error {
	switch {
	case errors.Is(err, remotesync.ErrRemoteMoved):
		return coded("sync_remote_moved", "%v", err)
	case errors.Is(err, remotesync.ErrUpToDate):
		return nil
	}
	return err
}

func isEmptyRepo(rr *repo.Repo) bool {
	n, err := rr.DB.CountVersions()
	if err != nil || n > 1 {
		return false
	}
	ws, _ := rr.DB.ListWorkspaces()
	return len(ws) == 0
}

var _ = os.Getenv
var _ = time.Now
