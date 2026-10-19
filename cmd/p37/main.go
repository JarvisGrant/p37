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

	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/hash"
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
  gc <root> [--workspaces] [--report-roots]
                                  回收与孤儿清理
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
	return repo.Open(root, repo.Options{
		ExtraIgnore:    cfg.Repo.IgnoreExtra,
		Capture:        cfg.CaptureOptions(),
		Materialize:    cfg.MountMode(),
		BaseCacheBytes: cfg.Workspace.BaseCacheBytes,
	})
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
	avail, err := diskAvailable(root)
	if err != nil {
		report("磁盘可用空间", true, "无法探测")
	} else {
		// 磁盘空间是**建议值**，不是硬性错误：空间偏小只是意味着并发工作区
		// 或仓库规模要相应收敛，不影响正确性。
		warnBytes := int64(40 << 30)
		if avail >= warnBytes {
			report("磁盘可用空间", true, humanBytes(avail))
		} else {
			fmt.Fprintf(out, "[WARN] %-28s %s（建议 ≥ %s；空间偏小只影响并发度，不影响正确性）\n",
				"磁盘可用空间", humanBytes(avail), humanBytes(warnBytes))
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
	fmt.Fprintf(out, "objects/ 文件数   %d\nobject_index 行数 %d\n", onDisk, inIndex)

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

	reportOnly := true
	workspacesOnly := false
	for _, a := range args {
		switch a {
		case "--prune":
			reportOnly = false
		case "--workspaces":
			workspacesOnly = true
		case "--report-roots":
			reportOnly = true
		}
	}

	if workspacesOnly || reportOnly {
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
		if !reportOnly {
			for _, o := range orphans {
				if err := os.RemoveAll(filepath.Join(root, "workspaces", o)); err != nil {
					return fail(errw, err)
				}
			}
			fmt.Fprintf(out, "已删除 %d 个孤儿目录\n", len(orphans))
		}
	}
	if workspacesOnly {
		return 0
	}

	freed, err := r.MM.Evict()
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "已淘汰基底缓存 %s\n", humanBytes(freed))
	fmt.Fprintln(out, "提示: 对象级 mark-sweep 与保留策略计划于 P2 交付。")
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
	var vid = strings.TrimSpace(id)
	if vid == "" {
		head, ok, err := r.Head()
		if err != nil || !ok {
			return fail(errw, errors.New("无 head"))
		}
		vid = head.String()
	}
	d, err := r.VersionShow(mustHash(vid))
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
	rv, err := r.Revert(mustHash(args[0]))
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintf(out, "已回退，新版本 %s\n", rv.String())
	return 0
}

// --- 小工具 -------------------------------------------------------------

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
