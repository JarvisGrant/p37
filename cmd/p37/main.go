// Command p37 是 p37 的运维 CLI（初始化、导入、检查、调试）。
//
// 日常的版本操作走 MCP（§11.3），本 CLI 只覆盖初始化与运维。
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chenyiwen/p37/internal/cas"
	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
	"github.com/chenyiwen/p37/internal/mountmgr"
	"github.com/chenyiwen/p37/internal/repo"
	"github.com/chenyiwen/p37/internal/tree"
	"github.com/chenyiwen/p37/internal/workspace"
)

const usage = `p37 —— AI 原生版本控制系统

用法：
  p37 <命令> [参数]

初始化与运维：
  init <root>                     创建空仓库
  import <root> <srcdir>          把目录树作为 v0 导入
  status <root>                   打印仓库摘要
  doctor <root>                   运行部署自检（附录 G.4）
  fsck <root>                     校验对象完整性与封闭性
  gc <root> [--prune] [--scrub] [--pack] [--report-roots] [--workspaces]
                                  保留策略 + 对象回收 + 抽样校验
  migrate <root>                  向前迁移 format_ver
  ignore-check <root> <path>      检查某路径是否被忽略

调试用版本操作（产品路径是 MCP）：
  ws-open <root> <task> [--materialize auto|overlay|copy] [--parent <id>]
  ws-list <root>
  ws-info <root> <ws_id>
  ws-sync <root> <ws_id>
  ws-diff <root> <ws_id>          预览将落地的改动
  ws-land <root> <ws_id>
  ws-drop <root> <ws_id> [--cascade=false]
  log <root> [limit]
  show <root> <version_id>
  revert <root> <version_id>
  blame <root> <path> [--range a-b] [--version <id>] [--hunk]

远端同步（P3，S3 兼容对象存储）：
  sync <root> prepare [--full] [--force]  生成增量包到本地 outbox（不上传）
  sync <root> list                        列出本地暂存包
  sync <root> show <id|last> [--verify]   查看/校验包
  sync <root> status [--remote]           同步状态
  sync <root> push [--all|<id>]           上传暂存包并更新远端 ref
  sync <root> fetch                       下载远端包并导入对象（不移动 head）
  sync <root> pull                        fetch + 仅快进本地 head
  sync <root> prune [--local] [--remote]  清理已上传的本地包 / 远端旧包

其它：
  version
  help
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, out, errw io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(errw, usage)
		return 2
	}
	cmd := args[0]
	rest := args[1:]

	switch cmd {
	case "help", "-h", "--help":
		fmt.Fprint(out, usage)
		return 0
	case "version", "-v", "--version":
		fmt.Fprintf(out, "p37 %s (format_ver=%d)\n", version, formatVer)
		return 0
	}

	if len(rest) == 0 {
		fmt.Fprintf(errw, "命令 %s 缺少 <root>\n\n%s", cmd, usage)
		return 2
	}
	root := rest[0]

	switch cmd {
	case "init":
		return cmdInit(root, out, errw)
	case "import":
		if len(rest) < 2 {
			fmt.Fprintln(errw, "import 需要 <srcdir>")
			return 2
		}
		return cmdImport(root, rest[1], out, errw)
	case "status":
		return cmdStatus(root, out, errw)
	case "doctor":
		return cmdDoctor(root, out, errw)
	case "fsck":
		return cmdFsck(root, out, errw)
	case "gc":
		return cmdGC(root, rest[1:], out, errw)
	case "migrate":
		return cmdMigrate(root, out, errw)
	case "ignore-check":
		if len(rest) < 2 {
			fmt.Fprintln(errw, "ignore-check 需要 <path>")
			return 2
		}
		return cmdIgnoreCheck(root, rest[1], out, errw)
	case "ws-open":
		return cmdWSOpen(root, rest[1:], out, errw)
	case "ws-list":
		return cmdWSList(root, out, errw)
	case "ws-info":
		return cmdWSInfo(root, rest[1:], out, errw)
	case "ws-sync":
		return cmdWSSync(root, rest[1:], out, errw)
	case "ws-diff":
		return cmdWSDiff(root, rest[1:], out, errw)
	case "ws-land":
		return cmdWSLand(root, rest[1:], out, errw)
	case "ws-drop":
		return cmdWSDrop(root, rest[1:], out, errw)
	case "log":
		return cmdLog(root, rest[1:], out, errw)
	case "show":
		return cmdShow(root, rest[1:], out, errw)
	case "revert":
		return cmdRevert(root, rest[1:], out, errw)
	case "blame":
		return cmdBlame(root, rest[1:], out, errw)
	case "sync":
		return cmdSync(root, rest[1:], out, errw)
	default:
		fmt.Fprintf(errw, "未知命令 %q\n\n%s", cmd, usage)
		return 2
	}
}

const (
	version   = "0.1.0"
	formatVer = 1
)

// openRepo 按 root 的 config.toml 打开仓库。
func openRepo(root string) (*repo.Repo, error) {
	cfg, err := config.Load(root)
	if err != nil {
		return nil, err
	}
	return repo.Open(root, cfg.RepoOptions())
}

func fail(errw io.Writer, err error) int {
	fmt.Fprintf(errw, "错误: %v\n", err)
	return 1
}

func cmdInit(root string, out, errw io.Writer) int {
	cfg := config.Default()
	cfg.Daemon.Socket = filepath.Join(root, ".p37", "p37d.sock")

	r, err := repo.Init(root, repo.Options{Materialize: mountmgr.ModeAuto})
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()

	if err := config.Save(root, cfg); err != nil {
		return fail(errw, err)
	}
	st, err := r.Status()
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "已初始化仓库 %s\n  初始版本 %s\n  配置     %s\n",
		root, short(st.MainlineHead.String()), config.Path(root))
	return 0
}

func cmdImport(root, src string, out, errw io.Writer) int {
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()

	start := time.Now()
	vid, err := r.Import(src)
	if err != nil {
		return fail(errw, err)
	}
	v, err := r.DB.GetVersion(vid)
	if err != nil {
		return fail(errw, err)
	}
	files, err := countTreeFiles(r, v.Tree)
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "已导入 %s\n  版本   %s\n  文件数 %d\n  耗时   %s\n",
		src, short(vid.String()), files, time.Since(start).Round(time.Millisecond))
	return 0
}

func cmdStatus(root string, out, errw io.Writer) int {
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()

	st, err := r.Status()
	if err != nil {
		return fail(errw, err)
	}
	nObj, err := r.DB.ObjectIndex().Count()
	if err != nil {
		return fail(errw, err)
	}
	nCache, err := r.DB.StatCache().CountAll()
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "仓库 %s\n", st.Root)
	fmt.Fprintf(out, "  mainline_head  %s\n", st.MainlineHead.String())
	fmt.Fprintf(out, "  version_count  %d（epoch %d）\n", st.VersionCount, st.Epoch)
	fmt.Fprintf(out, "  工作区         %d\n", st.Workspaces)
	fmt.Fprintf(out, "  CAS 对象       %d\n", nObj)
	fmt.Fprintf(out, "  stat 缓存       %d 行\n", nCache)
	fmt.Fprintf(out, "  format_ver     %d\n", st.FormatVer)
	if st.LastLandTS > 0 {
		fmt.Fprintf(out, "  最近落地       %s\n",
			time.UnixMilli(st.LastLandTS).Format(time.RFC3339))
	}
	fmt.Fprintf(out, "  物化管理器     基底缓存 %s\n", humanBytes(r.MM.BaseCacheSize()))
	return 0
}

func cmdDoctor(root string, out, errw io.Writer) int {
	// 自检读配置：磁盘阈值与其它建议值都来自配置，不能在这里另写一套常量
	// （那正是「配置改了不生效」的来源）。
	cfg, err := config.Load(root)
	if err != nil {
		return fail(errw, err)
	}
	ok := true
	report := func(name string, good bool, detail string) {
		mark := "OK  "
		if !good {
			mark = "FAIL"
			ok = false
		}
		fmt.Fprintf(out, "[%s] %-28s %s\n", mark, name, detail)
	}

	// 1. 目录与文件系统。
	p37 := filepath.Join(root, ".p37")
	wsDir := filepath.Join(root, "workspaces")
	if _, err := os.Stat(p37); err != nil {
		report("仓库目录", false, err.Error())
	} else {
		report("仓库目录", true, p37)
	}
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		report("workspaces/ 可写", false, err.Error())
	} else {
		report("workspaces/ 可写", true, wsDir)
	}

	// 2. 同一文件系统（OverlayFS 的 lower/upper/work 必须同 FS）。
	same, detail := sameFilesystem(p37, wsDir)
	report("同一文件系统", same, detail)

	// 3. OverlayFS 可用性（非 Linux 平台不适用，开发机走 copy）。
	if !isLinux {
		report("OverlayFS", true, "不适用（非 Linux，将使用 copy 模式）")
	} else {
		mm, err := mountmgr.New(root, mountmgr.Options{})
		if err != nil {
			report("OverlayFS", false, err.Error())
		} else {
			probe := filepath.Join(wsDir, ".p37-doctor-probe")
			_ = os.RemoveAll(probe)
			_ = os.MkdirAll(probe, 0o755)
			err := mm.ProbeOverlay(probe)
			_ = os.RemoveAll(probe)
			if err != nil {
				report("OverlayFS", false, err.Error()+"（将回退 copy 模式）")
			} else {
				report("OverlayFS", true, "可用")
			}
		}
	}

	// 4. 大小写敏感性：两种结果都合法，只影响是否多做一次冲突检查。
	sensitive := mountmgr.CaseSensitiveProbe(wsDir)
	report("大小写敏感性", true, boolDetail(sensitive))

	// 5. inotify 限额（仅 Linux 有意义）。
	watches, err := maxUserWatches()
	if err != nil {
		report("inotify 限额", true, "不适用（非 Linux）")
	} else {
		recommended := 1 << 20
		good := watches >= recommended
		report("inotify 限额", good,
			fmt.Sprintf("max_user_watches=%d（推荐 ≥ %d）", watches, recommended))
	}

	// 6. 磁盘可用空间。
	avail, err2 := diskAvailable(root)
	if err2 != nil {
		report("磁盘可用空间", true, "无法探测")
	} else {
		// 磁盘空间是**建议值**，不是硬性错误：空间偏小只是意味着并发工作区
		// 或仓库规模要相应收敛，不影响正确性。阈值取配置的
		// workspace.warn_disk_bytes（默认 50 GiB）。
		warnBytes := cfg.WarnDiskBytes()
		if avail >= warnBytes {
			report("磁盘可用空间", true, humanBytes(avail))
		} else {
			fmt.Fprintf(out, "[WARN] %-28s %s（建议 ≥ %s；空间偏小只影响并发度，不影响正确性）\n",
				"磁盘可用空间", humanBytes(avail), humanBytes(warnBytes))
		}
	}

	// 6b. 抽样校验（§10.4）：默认按 gc.scrub_sample 抽，能立刻暴露静默损坏。
	if cfg.GC.ScrubSample > 0 {
		r0, err := openRepo(root)
		if err == nil {
			if res, serr := r0.Scrub(cfg.GC.ScrubSample); serr == nil {
				if len(res.Corrupt) > 0 {
					report("对象抽样校验", false,
						fmt.Sprintf("%d 个损坏（共抽 %d 个）", len(res.Corrupt), res.Checked))
					for _, c := range res.Corrupt {
						fmt.Fprintf(out, "        损坏: %s\n", c)
					}
				} else {
					report("对象抽样校验", true,
						fmt.Sprintf("抽 %d 个，全部完好（比例 %g）", res.Checked, cfg.GC.ScrubSample))
				}
			}
			_ = r0.Close()
		}
	}

	// 7. 仓库可打开且格式匹配。
	r, err := openRepo(root)
	if err != nil {
		report("打开仓库", false, err.Error())
	} else {
		report("打开仓库", true, "format_ver 匹配")
		_ = r.Close()
	}

	if ok {
		fmt.Fprintln(out, "\n全部检查通过，可以进入验证。")
		return 0
	}
	fmt.Fprintln(out, "\n存在未通过项，见上。")
	return 1
}

func cmdFsck(root string, out, errw io.Writer) int {
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()

	// 索引一致性：扫描 objects/ 与 object_index 对比。
	stats, err := r.Store.BlobStore().ListPrefix("objects/")
	if err != nil {
		return fail(errw, err)
	}
	onDisk := int64(len(stats))
	inIndex, err := r.DB.ObjectIndex().Count()
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "对象总数（松散+打包）%d\nobject_index 行数 %d\n", onDisk, inIndex)

	// 重建索引并比对（索引是纯缓存，可由对象自描述形态重建）。
	idx := r.DB.ObjectIndex()
	if err := idx.Clear(); err != nil {
		return fail(errw, err)
	}
	n, err := r.Store.RebuildIndex()
	if err != nil {
		return fail(errw, fmt.Errorf("重建索引失败: %w", err))
	}
	fmt.Fprintf(out, "重建后对象数     %d\n", n)
	if int64(n) != onDisk {
		fmt.Fprintf(out, "警告: 有 %d 个对象文件无法解析为合法对象\n", onDisk-int64(n))
		return 1
	}

	// 头部可达性。
	head, ok, err := r.DB.Head()
	if err != nil {
		return fail(errw, err)
	}
	if !ok {
		fmt.Fprintln(out, "主干尚未初始化")
		return 1
	}
	if err := r.WalkHead(head, func(id string) error { return nil }); err != nil {
		return fail(errw, fmt.Errorf("head 链不可达: %w", err))
	}
	fmt.Fprintln(out, "head 链完整")
	fmt.Fprintln(out, "fsck 通过")
	return 0
}

func cmdGC(root string, args []string, out, errw io.Writer) int {
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()

	prune, scrub, workspacesOnly, rootsOnly, pack := false, false, false, false, false
	for _, a := range args {
		switch a {
		case "--prune":
			prune = true
		case "--scrub":
			scrub = true
		case "--workspaces":
			workspacesOnly = true
		case "--report-roots":
			rootsOnly = true
		case "--pack":
			pack = true
		default:
			fmt.Fprintf(errw, "未知参数 %q\n", a)
			return 2
		}
	}

	if workspacesOnly {
		return gcWorkspaces(r, root, prune, out, errw)
	}

	// 保留 + mark-sweep（§10）。默认只报告（dry-run），--prune 才真的删。
	rep, err := r.GC(!prune || rootsOnly)
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "版本: 共 %d，保留 %d，%s %d\n",
		rep.VersionTotal, rep.VersionKeep,
		map[bool]string{true: "将剪枝", false: "已剪枝"}[rep.DryRun], rep.VersionDrop)
	fmt.Fprintf(out, "对象: 共 %d，可达 %d，%s %d（%s）\n",
		rep.ObjectTotal, rep.ObjectKeep,
		map[bool]string{true: "可回收", false: "已删除"}[rep.DryRun],
		rep.ObjectDrop, humanBytes(rep.DropBytes))
	fmt.Fprintf(out, "根（谁钉住了什么）:\n")
	for _, rt := range rep.Roots {
		fmt.Fprintf(out, "  %-16s %-20s %4d 个对象  %s\n",
			rt.Kind, rt.ID, rt.Count, rt.Note)
	}
	if !rep.DryRun {
		fmt.Fprintf(out, "已剪枝版本 %d，删除对象 %d，回收 %s\n",
			rep.VersionsPruned, rep.ObjectsDeleted, humanBytes(rep.BytesReclaimed))
		fmt.Fprintf(out, "已清空 stat 缓存 %d 行（避免悬空对象引用，下一轮采集会重建）\n",
			rep.StatCacheCleared)
	}

	// 基底缓存 LRU（§4.8）。
	if freed, err := r.MM.Evict(); err == nil && freed > 0 {
		fmt.Fprintf(out, "已淘汰 overlay 基底缓存 %s\n", humanBytes(freed))
	}

	// 打包（§9 P2）：把松散小对象合并进 pack 文件。与其它回收一致，
	// 只有 --prune 才真的执行；否则只报告。
	if pack {
		if !prune {
			fmt.Fprintln(out, "（--pack 需要配合 --prune 才会真的打包；当前为预演）")
		} else {
			pr, err := r.Pack(cas.DefaultPackOptions())
			if err != nil {
				return fail(errw, err)
			}
			fmt.Fprintf(out, "打包: 新建 %d 个 pack，纳入对象 %d（%s），删除松散文件 %d，跳过超大对象 %d\n",
				pr.PacksCreated, pr.ObjectsPacked, humanBytes(pr.BytesPacked),
				pr.LooseRemoved, pr.SkippedLarge)
		}
	}

	if scrub {
		res, err := r.Scrub(r.GCOptions().ScrubSample)
		if err != nil {
			return fail(errw, err)
		}
		fmt.Fprintf(out, "scrub: 抽样校验 %d 个对象（%s），损坏 %d 个\n",
			res.Checked, humanBytes(res.Bytes), len(res.Corrupt))
		for _, c := range res.Corrupt {
			fmt.Fprintf(errw, "  损坏: %s\n", c)
		}
		if len(res.Corrupt) > 0 {
			return 1
		}
	}

	if rep.DryRun {
		fmt.Fprintln(out, "（这是预演；加 --prune 才会真的删除）")
	}
	return 0
}

// gcWorkspaces 处理孤儿工作区目录（§4.8）。
func gcWorkspaces(r *repo.Repo, root string, prune bool, out, errw io.Writer) int {
	all, err := r.WS.List()
	if err != nil {
		return fail(errw, err)
	}
	known := map[string]bool{}
	for _, w := range all {
		known[w.ID] = true
	}
	entries, err := os.ReadDir(filepath.Join(root, "workspaces"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(errw, err)
	}
	var orphans []string
	for _, e := range entries {
		if !e.IsDir() || known[e.Name()] {
			continue
		}
		orphans = append(orphans, e.Name())
	}
	sort.Strings(orphans)
	fmt.Fprintf(out, "活跃工作区 %d 个，孤儿目录 %d 个\n", len(all), len(orphans))
	for _, o := range orphans {
		fmt.Fprintf(out, "  孤儿 %s\n", o)
	}
	if prune {
		for _, o := range orphans {
			if err := os.RemoveAll(filepath.Join(root, "workspaces", o)); err != nil {
				return fail(errw, err)
			}
		}
		fmt.Fprintf(out, "已删除 %d 个孤儿目录\n", len(orphans))
	} else if len(orphans) > 0 {
		fmt.Fprintln(out, "（加 --prune 才会删除）")
	}
	return 0
}

func cmdMigrate(root string, out, errw io.Writer) int {
	r, err := repo.Open(root, repo.Options{})
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	st, err := r.Status()
	if err != nil {
		return fail(errw, err)
	}
	if st.FormatVer == formatVer {
		fmt.Fprintf(out, "已是当前格式版本 %d，无需迁移。\n", formatVer)
		return 0
	}
	fmt.Fprintf(errw, "不支持的迁移路径: %d -> %d\n", st.FormatVer, formatVer)
	return 1
}

func cmdIgnoreCheck(root, p string, out, errw io.Writer) int {
	cfg, err := config.Load(root)
	if err != nil {
		return fail(errw, err)
	}
	ig, err := tree.NewIgnore(true, cfg.Repo.IgnoreExtra)
	if err != nil {
		return fail(errw, err)
	}
	if lines, err := tree.LoadIgnoreFile(root); err != nil {
		return fail(errw, err)
	} else if len(lines) > 0 {
		if ig, err = tree.NewIgnore(true, append(append([]string{}, cfg.Repo.IgnoreExtra...), lines...)); err != nil {
			return fail(errw, err)
		}
	}
	norm, err := tree.NormalizePath(p)
	if err != nil {
		return fail(errw, err)
	}
	ignored := ig.Match(norm, false)
	fmt.Fprintf(out, "%s -> %v\n", norm, ignored)
	return 0
}

// --- 调试用版本操作 -----------------------------------------------------

func cmdWSOpen(root string, args []string, out, errw io.Writer) int {
	task := ""
	parent := ""
	mode := "auto"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--parent":
			if i+1 < len(args) {
				parent = args[i+1]
				i++
			}
		case "--materialize":
			if i+1 < len(args) {
				mode = args[i+1]
				i++
			}
		default:
			if task == "" {
				task = args[i]
			}
		}
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()

	w, err := r.WS.Open(workspace.OpenRequest{
		Parent: parent, TaskDesc: task, Mode: mountmgr.Mode(mode),
	})
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "ws_id       %s\n目录        %s\n物化模式    %s\n父          %s:%s\n",
		w.ID, r.WS.Dir(w.ID), w.Materialize, w.ParentKind, short(w.ParentID))
	return 0
}

func cmdWSList(root string, out, errw io.Writer) int {
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	all, err := r.WS.List()
	if err != nil {
		return fail(errw, err)
	}
	if len(all) == 0 {
		fmt.Fprintln(out, "（无工作区）")
		return 0
	}
	fmt.Fprintf(out, "%-24s %-12s %-10s %-6s %s\n", "WS_ID", "STATE", "MATERIAL", "OPS", "TASK")
	for _, w := range all {
		n, _ := r.DB.CountOps(w.ID)
		fmt.Fprintf(out, "%-24s %-12s %-10s %-6d %s\n", w.ID, w.State, w.Materialize, n, w.TaskDesc)
	}
	return 0
}

func cmdWSInfo(root string, args []string, out, errw io.Writer) int {
	if len(args) < 1 {
		return fail(errw, errors.New("ws-info 需要 <ws_id>"))
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	info, err := r.WS.Info(args[0])
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "ws_id        %s\nstate        %s\ndir          %s\nmaterialize  %s\nparent       %s:%s\nbase_version %s\ndirty        %v\ncaptured_ops %d\nconflicts    %d\n",
		info.ID, info.State, info.Dir, info.Materialize, info.ParentKind, short(info.ParentID),
		short(info.BaseVersion.String()), info.Dirty, info.CapturedOps, info.ConflictCount)
	return 0
}

func cmdWSSync(root string, args []string, out, errw io.Writer) int {
	if len(args) < 1 {
		return fail(errw, errors.New("ws-sync 需要 <ws_id>"))
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	start := time.Now()
	if err := r.WS.Sync(args[0]); err != nil {
		return fail(errw, err)
	}
	n, _ := r.DB.CountOps(args[0])
	fmt.Fprintf(out, "采集完成：%d 个 op，耗时 %s\n", n, time.Since(start).Round(time.Millisecond))
	return 0
}

func cmdWSDiff(root string, args []string, out, errw io.Writer) int {
	if len(args) < 1 {
		return fail(errw, errors.New("ws-diff 需要 <ws_id>"))
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	res, err := r.Preview(args[0])
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "op 数 %d，可落地 %v\n", res.OpsCount, res.CanLand)
	for _, f := range res.Files {
		fmt.Fprintf(out, "  %-40s %-10s +%d -%d\n", f.Path, f.Kind, f.Added, f.Deleted)
	}
	for _, c := range res.Conflicts {
		fmt.Fprintf(out, "  冲突 %s %s (%s)\n", c.Key.String()[:16], c.Path, c.Kind)
	}
	return 0
}

func cmdWSLand(root string, args []string, out, errw io.Writer) int {
	if len(args) < 1 {
		return fail(errw, errors.New("ws-land 需要 <ws_id>"))
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	res, err := r.Land(args[0])
	if err != nil {
		if errors.Is(err, repo.ErrLandConflict) {
			fmt.Fprintf(out, "落地被冲突阻断，共 %d 个：\n", len(res.Conflicts))
			for _, c := range res.Conflicts {
				fmt.Fprintf(out, "  %s  %s  %s\n", c.Key.String(), c.Path, c.Kind)
			}
			fmt.Fprintln(out, "用 MCP 的 conflict_show / conflict_resolve 处理后重试。")
			return 1
		}
		return fail(errw, err)
	}
	fmt.Fprintf(out, "已落地为 %s（op 数 %d）\n", res.VersionID.String(), res.OpsCount)
	return 0
}

func cmdWSDrop(root string, args []string, out, errw io.Writer) int {
	if len(args) < 1 {
		return fail(errw, errors.New("ws-drop 需要 <ws_id>"))
	}
	cascade := true
	for _, a := range args[1:] {
		if a == "--cascade=false" {
			cascade = false
		}
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	dropped, reparented, err := r.WS.Drop(args[0], cascade, false)
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "已放弃 %v；重挂子工作区 %v\n", dropped, reparented)
	return 0
}

func cmdLog(root string, args []string, out, errw io.Writer) int {
	limit := 20
	if len(args) > 0 {
		_, _ = fmt.Sscanf(args[0], "%d", &limit)
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	vs, err := r.Log(limit, 0)
	if err != nil {
		return fail(errw, err)
	}
	for _, v := range vs {
		fmt.Fprintf(out, "%s  %s  %-12s  %s\n",
			short(v.ID.String()),
			time.UnixMilli(v.TS).Format("2006-01-02 15:04:05"),
			v.Agent, v.Task)
	}
	return 0
}

func cmdShow(root string, args []string, out, errw io.Writer) int {
	id := ""
	if len(args) > 0 {
		id = args[0]
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()
	vid, err := resolveVersionArg(r, id)
	if err != nil {
		return fail(errw, err)
	}
	d, err := r.VersionShow(vid)
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "版本   %s\n父     %s\n时间   %s\nagent  %s\n任务   %s\n",
		d.ID.String(), d.Parent.String(),
		time.UnixMilli(d.TS).Format(time.RFC3339), d.Agent, d.Task)
	if len(d.Files) > 0 {
		fmt.Fprintln(out, "文件：")
		for _, f := range d.Files {
			fmt.Fprintf(out, "  %-40s %-10s +%d -%d\n", f.Path, f.Kind, f.Added, f.Deleted)
		}
	}
	return 0
}

func cmdRevert(root string, args []string, out, errw io.Writer) int {
	if len(args) < 1 {
		return fail(errw, errors.New("revert 需要 <version_id>"))
	}
	r, err := openRepo(root)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = r.Close() }()

	vid, err := resolveVersionArg(r, args[0])
	if err != nil {
		return fail(errw, err)
	}
	rv, err := r.Revert(vid)
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "已回退，新版本 %s\n", rv.String())
	return 0
}

// --- 小工具 -------------------------------------------------------------

// mustVersionArg 解析版本参数，支持 `head` / `HEAD` / 空（= head）。
func mustVersionArg(s string) hash.ID {
	t := strings.TrimSpace(s)
	if t == "" || strings.EqualFold(t, "head") {
		return hash.ID{}
	}
	return mustHash(t)
}

// resolveVersionArg 解析版本参数，支持 head / 空 / 完整 id / **唯一前缀**。
//
// `p37 log` 打印的是 16 位短 id，用户理应用它回喂给 show / revert / blame。
func resolveVersionArg(r *repo.Repo, s string) (hash.ID, error) {
	t := strings.TrimSpace(s)
	if t == "" || strings.EqualFold(t, "head") {
		head, ok, err := r.Head()
		if err != nil {
			return hash.ID{}, err
		}
		if !ok {
			return hash.ID{}, errors.New("无 head")
		}
		return head, nil
	}
	if id, err := hash.Parse(t); err == nil {
		return id, nil
	}
	if !isHexPrefix(t) {
		return hash.ID{}, fmt.Errorf("非法版本 id %q", s)
	}
	id, err := r.DB.FindVersionByPrefix(t)
	switch {
	case errors.Is(err, meta.ErrNotExist):
		return hash.ID{}, fmt.Errorf("找不到版本（前缀 %q）", s)
	case errors.Is(err, meta.ErrAmbiguous):
		return hash.ID{}, fmt.Errorf("前缀 %q 匹配到多个版本，请给更长前缀", s)
	}
	return id, err
}

// isHexPrefix 报告 s 是否为一段十六进制字符（不要求偶数长度）。
func isHexPrefix(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func mustHash(s string) hash.ID {
	id, err := hash.Parse(s)
	if err != nil {
		fmt.Fprintf(os.Stderr, "非法 id %q: %v\n", s, err)
		os.Exit(2)
	}
	return id
}

func short(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:16]
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func boolDetail(b bool) string {
	if b {
		return "是"
	}
	return "否（物化前会检查大小写冲突）"
}

// countTreeFiles 递归统计树中的文件条目数。
func countTreeFiles(r *repo.Repo, root hash.ID) (int, error) {
	t, err := r.Store.GetTree(root)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range t.Entries {
		if e.Kind == codec.EntryTree {
			sub, err := countTreeFiles(r, e.ID)
			if err != nil {
				return 0, err
			}
			n += sub
			continue
		}
		n++
	}
	return n, nil
}
