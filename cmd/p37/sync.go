package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/remotesync"
	"github.com/chenyiwen/p37/internal/repo"
)

// cmdSync 实现 `p37 sync <子命令>`。
func cmdSync(root string, args []string, out, errw io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(errw, `用法:
  p37 sync <root> prepare [--full] [--force]   生成增量包到本地 outbox（不上传）
  p37 sync <root> list                         列出本地暂存包
  p37 sync <root> show <id|last> [--verify]    查看清单；--verify 全量校验
  p37 sync <root> status [--remote]            同步状态
  p37 sync <root> push [--all|<id>]            上传暂存包并更新远端 ref
  p37 sync <root> fetch                        下载远端包并导入对象（不移动 head）
  p37 sync <root> pull                         fetch + 仅快进本地 head
  p37 sync <root> prune [--local] [--remote]   清理已上传的本地包 / 远端旧包
`)
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "prepare":
		return syncPrepare(root, rest, out, errw)
	case "list":
		return syncList(root, out, errw)
	case "show":
		return syncShow(root, rest, out, errw)
	case "status":
		return syncStatus(root, rest, out, errw)
	case "push":
		return syncPush(root, rest, out, errw)
	case "fetch":
		return syncPull(root, rest, out, errw, false)
	case "pull":
		return syncPull(root, rest, out, errw, true)
	case "prune":
		return syncPrune(root, rest, out, errw)
	default:
		fmt.Fprintf(errw, "未知 sync 子命令 %q\n", sub)
		return 2
	}
}

func openRemote(root string) (remotesync.Remote, config.RemoteConfig, bool, error) {
	cfg, err := config.Load(root)
	if err != nil {
		return nil, cfg.Remote, false, err
	}
	if !cfg.Remote.Enabled {
		return nil, cfg.Remote, false, nil
	}
	switch cfg.Remote.Backend {
	case "dir":
		dir := cfg.Remote.Dir
		if dir == "" {
			return nil, cfg.Remote, true, errors.New("remote.dir 未配置")
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(root, dir)
		}
		r, err := remotesync.NewDirRemote(dir)
		return r, cfg.Remote, true, err
	case "s3", "":
		c := remotesync.S3ConfigFromEnv(remotesync.S3Config{
			Endpoint: cfg.Remote.Endpoint, Region: cfg.Remote.Region,
			Bucket: cfg.Remote.Bucket, Prefix: cfg.Remote.Prefix,
			PathStyle: cfg.Remote.PathStyle,
		})
		if c.AccessKey == "" || c.SecretKey == "" {
			return nil, cfg.Remote, true, fmt.Errorf(
				"缺少 S3 凭证：请设置 %s / %s", remotesync.EnvAccessKey, remotesync.EnvSecretKey)
		}
		r, err := remotesync.NewS3Remote(context.Background(), c)
		return r, cfg.Remote, true, err
	default:
		return nil, cfg.Remote, true, fmt.Errorf("未知 remote.backend %q", cfg.Remote.Backend)
	}
}

func syncPrepare(root string, args []string, out, errw io.Writer) int {
	force, full := false, false
	for _, a := range args {
		switch a {
		case "--force":
			force = true
		case "--full":
			full = true
		default:
			fmt.Fprintf(errw, "未知参数 %q\n", a)
			return 2
		}
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	cfg, err := config.Load(root)
	if err != nil {
		return fail(errw, err)
	}
	m, err := remotesync.Prepare(r, cfg.RemoteOptions(), force, full)
	if err != nil {
		if errors.Is(err, remotesync.ErrUpToDate) || errors.Is(err, remotesync.ErrNothingToPackage) {
			fmt.Fprintf(out, "%v（当前无新内容可打包）\n", err)
			return 0
		}
		return fail(errw, err)
	}
	fmt.Fprintf(out, "已生成 %s 包 %s\n", m.Kind, m.ID)
	fmt.Fprintf(out, "  base    %s\n  result  %s\n", short(m.BaseVersion), short(m.ResultVersion))
	fmt.Fprintf(out, "  版本    %d  对象 %d  原始 %s  payload %s（压缩比 %.2fx）\n",
		len(m.Versions), m.ObjectCount, humanBytes(m.RawBytes),
		humanBytes(m.PayloadBytes), ratio(m.RawBytes, m.PayloadBytes))
	fmt.Fprintf(out, "  目录    %s\n", filepath.Join(cfg.RemoteOptions().Outbox, m.ID))
	fmt.Fprintln(out, "（仅暂存本地，尚未上传；用 p37 sync show 审查，p37 sync push 上传）")
	return 0
}

func syncList(root string, out, errw io.Writer) int {
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	rows, err := remotesync.StagedRows(r)
	if err != nil {
		return fail(errw, err)
	}
	if len(rows) == 0 {
		fmt.Fprintln(out, "（本地没有暂存包）")
		return 0
	}
	fmt.Fprintf(out, "%-18s %-10s %-18s %-8s %-8s %s\n", "ID", "KIND", "BASE->RESULT", "对象", "体积", "状态")
	for _, row := range rows {
		st := "待上传"
		if row.Uploaded {
			st = "已上传"
		}
		fmt.Fprintf(out, "%-18s %-10s %-18s %-8d %-8s %s\n",
			row.ID[:16], row.Kind, short(row.BaseVersion)+"->"+short(row.ResultVersion),
			row.ObjectCount, humanBytes(row.ByteSize), st)
	}
	return 0
}

func syncShow(root string, args []string, out, errw io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errw, "show 需要 <id|last>")
		return 2
	}
	id := args[0]
	verify := false
	for _, a := range args[1:] {
		if a == "--verify" {
			verify = true
		}
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	row, err := findPackage(r, id)
	if err != nil {
		return fail(errw, err)
	}
	m, err := remotesync.ReadStaged(*row)
	if err != nil {
		return fail(errw, err)
	}
	b, _ := m.Encode()
	fmt.Fprintln(out, string(b))
	if verify {
		if _, err := remotesync.VerifyDir(row.LocalPath); err != nil {
			fmt.Fprintf(errw, "校验失败: %v\n", err)
			return 1
		}
		fmt.Fprintln(out, "校验通过：分片哈希与逐对象内容一致")
	}
	return 0
}

func syncStatus(root string, args []string, out, errw io.Writer) int {
	live := false
	for _, a := range args {
		if a == "--remote" {
			live = true
		}
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()

	remote, _, configured, err := openRemote(root)
	if err != nil {
		fmt.Fprintf(errw, "警告: %v\n", err)
	}
	remoteHead := ""
	if live && remote != nil {
		ref, err := remote.GetRef(context.Background())
		if err != nil && !errors.Is(err, remotesync.ErrNoRef) {
			return fail(errw, err)
		}
		remoteHead = ref.Head
	}
	st, err := remotesync.GetStatus(r, remoteHead, configured)
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "local_head        %s\n", st.LocalHead)
	fmt.Fprintf(out, "remote_head       %s（%s）\n", st.RemoteHead, map[bool]string{true: "已配置", false: "未配置"}[st.RemoteConfigured])
	fmt.Fprintf(out, "待上传包          %d\n", st.PendingPackages)
	fmt.Fprintf(out, "上次同步          %s\n", tsString(st.LastSyncTS))
	fmt.Fprintf(out, "ahead/behind      %d / %d%s\n", st.Ahead, st.Behind,
		map[bool]string{true: "（已分叉）", false: ""}[st.Diverged])
	return 0
}

func syncPush(root string, args []string, out, errw io.Writer) int {
	var ids []string
	for _, a := range args {
		if a == "--all" {
			continue
		}
		if strings.HasPrefix(a, "--") {
			fmt.Fprintf(errw, "未知参数 %q\n", a)
			return 2
		}
		ids = append(ids, a)
	}
	remote, _, configured, err := openRemote(root)
	if err != nil {
		return fail(errw, err)
	}
	if !configured {
		return fail(errw, errors.New("remote 未配置（config.toml 的 [remote].enabled = false）"))
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	res, err := remotesync.Push(context.Background(), r, remote, ids)
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "已上传 %d 个包（%s），远端 head = %s\n",
		len(res.Uploaded), humanBytes(res.Bytes), short(res.RemoteHead))
	return 0
}

func syncPull(root string, args []string, out, errw io.Writer, pull bool) int {
	remote, _, configured, err := openRemote(root)
	if err != nil {
		return fail(errw, err)
	}
	if !configured {
		return fail(errw, errors.New("remote 未配置"))
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	cfg, err := config.Load(root)
	if err != nil {
		return fail(errw, err)
	}
	fn := remotesync.Fetch
	if pull {
		fn = remotesync.Pull
	}
	res, err := fn(context.Background(), r, remote, cfg.RemoteOptions())
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "远端 head %s，下载 %d 个包、导入 %d 个对象\n",
		short(res.RemoteHead), res.Downloaded, res.ImportedObjects)
	fmt.Fprintf(out, "本地 head %s（ahead %d / behind %d）\n", short(res.LocalHead), res.Ahead, res.Behind)
	switch {
	case res.UpToDate:
		fmt.Fprintln(out, "已是最新")
	case pull && res.FastForward:
		fmt.Fprintln(out, "已快进")
	case res.Diverged:
		fmt.Fprintln(out, "本地与远端已分叉：对象已导入，但未移动本地 head（需要合并）")
	case res.Behind > 0:
		fmt.Fprintln(out, "fetch 完成；执行 p37 sync pull 可快进本地 head")
	}
	return 0
}

func syncPrune(root string, args []string, out, errw io.Writer) int {
	local, remotePrune := false, false
	for _, a := range args {
		switch a {
		case "--local":
			local = true
		case "--remote":
			remotePrune = true
		default:
			fmt.Fprintf(errw, "未知参数 %q\n", a)
			return 2
		}
	}
	if !local && !remotePrune {
		local = true
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	if local {
		n, bytes, err := remotesync.PruneLocal(r)
		if err != nil {
			return fail(errw, err)
		}
		fmt.Fprintf(out, "已清理本地已上传包 %d 个（%s）\n", n, humanBytes(bytes))
	}
	if remotePrune {
		remote, _, configured, err := openRemote(root)
		if err != nil {
			return fail(errw, err)
		}
		if !configured {
			return fail(errw, errors.New("remote 未配置"))
		}
		n, bytes, err := remotesync.PruneRemote(context.Background(), remote)
		if err != nil {
			return fail(errw, err)
		}
		fmt.Fprintf(out, "已清理远端旧包 %d 个（%s）\n", n, humanBytes(bytes))
	}
	return 0
}

func findPackage(r *repo.Repo, id string) (*meta.PackageRow, error) {
	if id == "" || id == "last" {
		row, err := r.DB.LastPackage()
		if err != nil {
			return nil, err
		}
		return row, nil
	}
	rows, err := r.DB.ListPackages()
	if err != nil {
		return nil, err
	}
	var hit *meta.PackageRow
	for _, row := range rows {
		if row.ID == id || strings.HasPrefix(row.ID, id) {
			if hit != nil {
				return nil, errors.New("包 id 前缀匹配到多个，请给更长前缀")
			}
			rr := row
			hit = &rr
		}
	}
	if hit == nil {
		return nil, fmt.Errorf("找不到包 %q", id)
	}
	return hit, nil
}

func ratio(raw, stored int64) float64 {
	if stored <= 0 {
		return 0
	}
	return float64(raw) / float64(stored)
}

func tsString(ms int64) string {
	if ms == 0 {
		return "从未"
	}
	return time.UnixMilli(ms).Format(time.RFC3339)
}
