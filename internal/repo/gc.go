package repo

import (
	"fmt"
	"sort"

	"github.com/chenyiwen/p37/internal/cas"
	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/meta"
)

// 本文件实现 §10 的保留与 GC。
//
// 两件事分开：
//
//	mark-sweep  —— 扫掉**不可达**的 CAS 对象（磁盘空间真正的大头）
//	retention   —— 剪掉**最老的一批版本行**（§10.3）
//
// 根集合（§10.1，缺一个就丢数据）：
//
//	1. mainline_head
//	2. 保留策略命中的 version
//	3. 所有活跃 workspace 的 base_tree
//	4. 活跃 workspace 的 op 引用的内容对象
//
// 第 4 条规格里没写，但**必须**有：工作区采集出来的 FileContent 只被 op 表
// 引用，不属于任何版本的树。漏了它，下一次 land 会去读一个已被扫掉的对象。

// GCRootInfo 描述一个根，供 `gc --report-roots` 展示「谁钉住了什么」。
type GCRootInfo struct {
	Kind  string `json:"kind"` // head | version | workspace_tree | workspace_ops
	ID    string `json:"id"`
	Note  string `json:"note,omitempty"`
	Count int    `json:"objects"`
}

// GCReport 是一次 GC 的完整结果（dry-run 时执行相关字段为 0）。
type GCReport struct {
	DryRun bool `json:"dry_run"`

	VersionTotal int `json:"version_total"`
	VersionKeep  int `json:"version_keep"`
	VersionDrop  int `json:"version_drop"`

	ObjectTotal int   `json:"object_total"`
	ObjectKeep  int   `json:"object_keep"`
	ObjectDrop  int   `json:"object_drop"`
	DropBytes   int64 `json:"drop_bytes"`

	Roots []GCRootInfo `json:"roots"`

	// 执行后才有的字段。
	VersionsPruned   int   `json:"versions_pruned"`
	ObjectsDeleted   int   `json:"objects_deleted"`
	BytesReclaimed   int64 `json:"bytes_reclaimed"`
	StatCacheCleared int64 `json:"stat_cache_cleared"`
	Bootstrapped     bool  `json:"stat_cache_bootstrapped"`

	// pack 回收（§9 P2）：只有当 pack 内所有对象都不可达时整包删除。
	PacksDropped       int   `json:"packs_dropped"`
	PackObjectsDropped int   `json:"pack_objects_dropped"`
	PackBytesReclaimed int64 `json:"pack_bytes_reclaimed"`
}

// GC 执行一次保留 + mark-sweep（§10）。
//
// dryRun 为真时只计算计划，不改任何东西。
func (r *Repo) GC(dryRun bool) (*GCReport, error) {
	// GC 会删对象，必须与 land **互斥**（而不只是限流）：land 正在拼树时把
	// 它要读的对象扫掉就完了。整条 land 流水线共用一把 landMu（§7.4），
	// 这里也拿它。
	r.landMu.Lock()
	defer r.landMu.Unlock()

	var err error
	rep := &GCReport{DryRun: dryRun}

	lines, err := r.DB.ListLines()
	if err != nil {
		return nil, err
	}
	head, ok, err := r.DB.Head()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotInitialized
	}
	roots := make([]hash.ID, 0, len(lines))
	for i := range lines {
		roots = append(roots, lines[i].Head)
	}
	if len(roots) == 0 {
		roots = append(roots, head)
	}
	// 保留策略：每条 line 各自保留最近 KeepVersions 个祖先。这样一条繁忙的
	// session line 不会把另一条线的全部历史钉住。
	keepN := r.gc.KeepVersions
	if keepN <= 0 {
		keepN = DefaultGCOptions().KeepVersions
	}
	reachable := map[hash.ID]bool{}
	for _, l := range lines {
		depths, err := r.DB.AncestorDepths(l.Head)
		if err != nil {
			return nil, err
		}
		type di struct {
			id    hash.ID
			depth int
		}
		ds := make([]di, 0, len(depths))
		for id, d := range depths {
			ds = append(ds, di{id, d})
		}
		sort.Slice(ds, func(i, j int) bool {
			if ds[i].depth != ds[j].depth {
				return ds[i].depth < ds[j].depth
			}
			return ds[i].id.String() < ds[j].id.String()
		})
		for i := 0; i < len(ds) && i < keepN; i++ {
			reachable[ds[i].id] = true
		}
	}
	if len(lines) == 0 {
		chain, err := r.DB.VersionChain(head)
		if err != nil {
			return nil, err
		}
		for i := 0; i < len(chain) && i < keepN; i++ {
			reachable[chain[i]] = true
		}
	}
	// 额外保留：工作区基准、幂等结果引用的版本（7 天内）。
	allWs, err := r.DB.ListWorkspaces()
	if err != nil {
		return nil, err
	}
	for i := range allWs {
		w := &allWs[i]
		for _, ref := range []string{w.BaseVersion.String(), w.ParentID} {
			if id, err := hash.Parse(ref); err == nil && !id.IsZero() {
				reachable[id] = true
			}
		}
	}
	if ids, err := r.DB.IdempotentVersionIDs(meta.NowMS() - int64(7*24*60*60*1000)); err == nil {
		for _, id := range ids {
			reachable[id] = true
		}
	}
	if ids, err := r.DB.RecentResetHeads(meta.NowMS() - int64(7*24*60*60*1000)); err == nil {
		for _, id := range ids {
			reachable[id] = true
		}
	}
	rows, err := r.DB.ListVersions(1_000_000, 0)
	if err != nil {
		return nil, err
	}
	rep.VersionTotal = len(rows)
	keepSet := reachable
	var keepVersions, dropVersions []hash.ID
	for _, row := range rows {
		if keepSet[row.ID] {
			keepVersions = append(keepVersions, row.ID)
		} else {
			dropVersions = append(dropVersions, row.ID)
		}
	}
	rep.VersionKeep, rep.VersionDrop = len(keepVersions), len(dropVersions)

	// -- 2. 对象清单（只读头部，不解压） ------------------------------------
	objects, err := r.Store.ListObjects()
	if err != nil {
		return nil, err
	}
	rep.ObjectTotal = len(objects)
	types := make(map[hash.ID]uint8, len(objects))
	size := make(map[hash.ID]int64, len(objects))
	for _, o := range objects {
		types[o.ID] = o.Type
		size[o.ID] = o.StoredSize
	}

	// -- 3. 标记 ------------------------------------------------------------
	marked := make(map[hash.ID]struct{}, len(objects))
	counts := map[string]int{}
	mark := func(kind, id string, root hash.ID) error {
		n, err := r.markReachable(root, types, marked)
		if err != nil {
			return err
		}
		if n > 0 {
			rep.Roots = append(rep.Roots, GCRootInfo{Kind: kind, ID: id, Count: n})
			counts[kind] += n
		}
		return nil
	}

	for _, vid := range keepVersions {
		v, err := r.DB.GetVersion(vid)
		if err != nil {
			return nil, err
		}
		if err := mark("version", vid.String()[:16], v.Tree); err != nil {
			return nil, err
		}
	}

	all := allWs
	for i := range all {
		w := &all[i]
		if err := mark("workspace_tree", w.ID, w.BaseTree); err != nil {
			return nil, err
		}
		ops, err := r.DB.ListOps(w.ID)
		if err != nil {
			return nil, err
		}
		for j := range ops {
			o := &ops[j]
			for _, id := range []hash.ID{o.BaseFile, o.NewFile} {
				if id.IsZero() {
					continue
				}
				if _, ok := marked[id]; ok {
					continue
				}
				if _, err := r.markReachable(id, types, marked); err != nil {
					return nil, err
				}
				counts["workspace_ops"]++
			}
		}
	}
	rep.ObjectKeep = len(marked)
	rep.Roots = append(rep.Roots, GCRootInfo{
		Kind: "workspace_ops", ID: "(活跃工作区的 op 引用)",
		Count: counts["workspace_ops"],
		Note:  "采集产物只被 op 表引用，不属于任何版本的树",
	})

	// -- 4. 差集 ------------------------------------------------------------
	var drop []cas.ObjectInfo
	for _, o := range objects {
		if _, ok := marked[o.ID]; ok {
			continue
		}
		drop = append(drop, o)
		rep.DropBytes += o.StoredSize
	}
	rep.ObjectDrop = len(drop)

	if dryRun {
		return rep, nil
	}

	// -- 5. 执行 ------------------------------------------------------------
	// 顺序要紧：**先剪版本行、再删对象**。反过来会出现「版本还在但对象没了」
	// 的窗口，此时若进程被杀，仓库就处于损坏状态。
	for _, vid := range dropVersions {
		if err := r.DB.PruneVersion(vid); err != nil {
			return rep, err
		}
		rep.VersionsPruned++
	}
	for _, o := range drop {
		if err := r.Store.DeleteObject(o.ID); err != nil {
			return rep, err
		}
		rep.ObjectsDeleted++
		rep.BytesReclaimed += o.StoredSize
	}
	// 打包对象不能逐个删除：只有当 pack 内**所有**对象都不可达时整包删除。
	sw, err := r.Store.SweepPacks(marked)
	if err != nil {
		return rep, err
	}
	rep.PacksDropped = sw.PacksDropped
	rep.PackObjectsDropped = sw.ObjectsDropped
	rep.PackBytesReclaimed = sw.BytesReclaimed
	rep.BytesReclaimed += sw.BytesReclaimed

	// stat 缓存必须清空：它可能持有「只被缓存引用」的中间 Tree 对象 id，
	// 而那些对象刚刚被扫掉了。留着就是悬空引用 —— 下一次采集会直接返回一个
	// 不存在的对象 id，树立刻烂掉。它是纯缓存，清掉即可。
	n, err := r.DB.StatCache().ClearAll()
	if err != nil {
		return rep, err
	}
	rep.StatCacheCleared = n
	rep.Bootstrapped = true

	// 对象落盘后再返回（与 land 的提交点纪律一致）。
	if err := r.Store.Flush(); err != nil {
		return rep, err
	}
	return rep, nil
}

// retentionCount 返回版本链上要保留的前缀长度。
//
// 保留规则（§10.3）：最近 KeepVersions 个版本；此外**活跃工作区引用到的
// 版本必须保留**（它们是 base_version 或 stack 的父），否则工作区重放时
// 找不到基准。为保持「保留的是一个连续前缀」，把前缀延长到覆盖最老的
// 被引用版本。
func (r *Repo) retentionCount(chain []hash.ID) (int, error) {
	n := r.gc.KeepVersions
	if n <= 0 {
		n = DefaultGCOptions().KeepVersions
	}
	idx := make(map[hash.ID]int, len(chain))
	for i, id := range chain {
		idx[id] = i
	}

	all, err := r.DB.ListWorkspaces()
	if err != nil {
		return 0, err
	}
	for i := range all {
		w := &all[i]
		for _, ref := range []string{w.BaseVersion.String(), w.ParentID} {
			if ref == "" {
				continue
			}
			id, err := hash.Parse(ref)
			if err != nil {
				continue
			}
			if at, ok := idx[id]; ok && at+1 > n {
				n = at + 1
			}
		}
	}
	return n, nil
}

// markReachable 从 root 出发标记全部可达对象，返回新标记的个数。
//
// 只解压 Tree 与 FileContent（都小）；Blob 是叶子，只看类型不解压 ——
// 所以 10 GB 仓库的标记阶段不需要读那 10 GB。
func (r *Repo) markReachable(root hash.ID, types map[hash.ID]uint8,
	marked map[hash.ID]struct{}) (int, error) {
	if root.IsZero() {
		return 0, nil
	}
	added := 0
	stack := []hash.ID{root}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, ok := marked[id]; ok {
			continue
		}
		marked[id] = struct{}{}
		added++

		switch types[id] {
		case codec.TypeTree:
			tr, err := r.Store.GetTree(id)
			if err != nil {
				return added, fmt.Errorf("gc: 树 %s 不可读: %w", id, err)
			}
			for _, e := range tr.Entries {
				stack = append(stack, e.ID)
			}
		case codec.TypeFileContent:
			fc, err := r.Store.GetFileContent(id)
			if err != nil {
				return added, fmt.Errorf("gc: FileContent %s 不可读: %w", id, err)
			}
			stack = append(stack, fc.Chunks...)
		case codec.TypeBlob, codec.TypeVersion:
			// 叶子。
		default:
			// 类型未知（索引缺失）→ 保守起见不再展开，但已算作可达，
			// 不会被误删。
		}
	}
	return added, nil
}

// ScrubResult 是一次 scrub 的结果（§10.4）。
type ScrubResult struct {
	Checked int      `json:"checked"`
	Corrupt []string `json:"corrupt"`
	Bytes   int64    `json:"bytes"`
}

// Scrub 按比例抽样校验对象：解压 + 与 key 比对哈希（§10.4）。
//
// 「校验常驻」是硬要求 —— 静默损坏无法靠测试覆盖。抽样比例来自
// gc.scrub_sample；比例 >= 1 时等价于全量。
func (r *Repo) Scrub(sample float64) (*ScrubResult, error) {
	if sample <= 0 {
		sample = DefaultGCOptions().ScrubSample
	}
	objects, err := r.Store.ListObjects()
	if err != nil {
		return nil, err
	}
	res := &ScrubResult{}
	if len(objects) == 0 {
		return res, nil
	}

	// 确定性抽样：按对象 id 排序后按步长取，保证同一仓库每次抽到同一批
	// （可复现），而不是随机的。
	sort.Slice(objects, func(i, j int) bool { return objects[i].ID.String() < objects[j].ID.String() })
	step := 1
	if sample < 1 {
		step = int(1 / sample)
		if step < 1 {
			step = 1
		}
	}
	for i := 0; i < len(objects); i += step {
		o := objects[i]
		res.Checked++
		res.Bytes += o.StoredSize
		if _, _, err := r.Store.Get(o.ID); err != nil {
			res.Corrupt = append(res.Corrupt, fmt.Sprintf("%s: %v", o.ID, err))
			if len(res.Corrupt) >= 32 {
				break
			}
		}
	}
	return res, nil
}

// GCReportFull 在 dry-run 之外补齐「被谁钉住」的可读视图。
func (r *Repo) GCReportFull(rep *GCReport) string {
	out := ""
	add := func(f string, a ...any) { out += fmt.Sprintf(f+"\n", a...) }
	add("版本: 共 %d，保留 %d，剪枝 %d", rep.VersionTotal, rep.VersionKeep, rep.VersionDrop)
	add("对象: 共 %d，可达 %d，可回收 %d（%s）",
		rep.ObjectTotal, rep.ObjectKeep, rep.ObjectDrop, humanBytes(rep.DropBytes))
	if len(rep.Roots) > 0 {
		add("根（谁钉住了什么）:")
		for _, rt := range rep.Roots {
			add("  %-16s %-20s %d 个对象  %s", rt.Kind, rt.ID, rt.Count, rt.Note)
		}
	}
	return out
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

var _ = meta.ErrNotExist
