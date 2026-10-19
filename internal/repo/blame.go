package repo

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chenyiwen/p37/internal/codec"
	"github.com/chenyiwen/p37/internal/hash"
	"github.com/chenyiwen/p37/internal/tree"
	"github.com/chenyiwen/p37/internal/version"
)

// ErrPathNotFound 表示版本树中不存在该路径。
var ErrPathNotFound = errors.New("repo: 路径不存在")

// BlameRecord 是一条行归属记录（§12.2、附录 K）。
//
// 相对 git 的关键差异：每条记录都带 `agent` 与 `task` —— 直接就是「当时那次
// 整合的任务描述」，不需要再去猜 commit message。
type BlameRecord struct {
	FromLine   int    `json:"from_line"`
	ToLine     int    `json:"to_line"`
	Range      string `json:"range"`
	VersionID  string `json:"version_id"`
	OpID       string `json:"op_id,omitempty"`
	Agent      string `json:"agent,omitempty"`
	Task       string `json:"task,omitempty"`
	TS         int64  `json:"ts,omitempty"`
	Originated bool   `json:"originated,omitempty"`
	Available  bool   `json:"available"`
}

// BlameOptions 控制一次 blame。
type BlameOptions struct {
	// VersionID 为空表示主干 head。
	VersionID hash.ID
	// From / To 是 1-based 闭区间；From ≤ 1 表示从头，To ≤ 0 表示到末尾。
	From int
	To   int
	// Granularity 是 "line" 或 "hunk"（默认 line）。
	Granularity string
	// NoCache 为真时跳过缓存读写（诊断用）。
	NoCache bool
}

// blameCache 是持久化到 blame_cache 的负载。
type blameCache struct {
	// Owners[i] 是第 i+1 行的归属版本 id（十六进制）。
	Owners []string `json:"owners"`
	// BirthVersion 是文件诞生的版本。
	BirthVersion string `json:"birth_version"`
}

// Blame 把某版本下 path 的每一行归属到引入它的版本（§12.2）。
//
// 实现要点：**从文件诞生的那一版正向推进**，而不是从目标版本反向回溯。
//
// 反向回溯看着更自然（「未变更的行归 parent」），但会丢掉信息：某行在第 3 版
// 被改过、之后两版都没碰它，回溯到第 1 版时会被错误地归成第 1 版。正向推进
// 的语义则无歧义 —— 每一跳把新文件里**被改动的行**判给当前版本，其余行沿用
// 上一版的归属。
//
// 主干精简（D3）让这条链很短，因此 O(链长 × 文件行数) 可以接受。
func (r *Repo) Blame(path string, opt BlameOptions) ([]BlameRecord, error) {
	switch opt.Granularity {
	case "", "line":
		opt.Granularity = "line"
	case "hunk":
	default:
		return nil, fmt.Errorf("blame: 未知 granularity %q", opt.Granularity)
	}

	if opt.VersionID.IsZero() {
		head, ok, err := r.DB.Head()
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrNotInitialized
		}
		opt.VersionID = head
	}
	if _, err := r.DB.GetVersion(opt.VersionID); err != nil {
		return nil, err
	}

	owners, birth, err := r.blameOwners(opt.VersionID, path, opt.NoCache)
	if err != nil {
		return nil, err
	}
	if len(owners) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrPathNotFound, path)
	}

	from := 1
	if opt.From > 1 {
		from = opt.From
	}
	if from > len(owners) {
		return nil, fmt.Errorf("blame: 起始行 %d 超出文件长度 %d", from, len(owners))
	}
	to := len(owners)
	if opt.To > 0 && opt.To < to {
		to = opt.To
	}
	if to < from {
		return nil, fmt.Errorf("blame: 区间 %d-%d 非法", from, to)
	}

	return r.blameRecords(path, owners, birth, from, to), nil
}

// blameOwners 返回 path 在 versionID 下的逐行归属（下标 0 = 第 1 行）与诞生版本。
func (r *Repo) blameOwners(versionID hash.ID, path string, noCache bool) ([]hash.ID, hash.ID, error) {
	if !noCache {
		if owners, birth, ok := r.loadBlameCache(versionID.String(), path); ok {
			return owners, birth, nil
		}
	}

	// 1. 收集 [出生版本 … 目标版本]。
	var chain []*version.Version
	v, err := r.DB.GetVersion(versionID)
	if err != nil {
		return nil, hash.ID{}, err
	}
	targetLines, ok, err := r.linesAt(v.Tree, path)
	if err != nil {
		return nil, hash.ID{}, err
	}
	if !ok {
		return nil, hash.ID{}, nil
	}
	chain = append(chain, v)
	for {
		cur := chain[len(chain)-1]
		if cur.Parent.IsZero() {
			break
		}
		p, err := r.DB.GetVersion(cur.Parent)
		if err != nil {
			break // 剪枝边界（§10.3）：祖先已被 GC 回收，视作链的起点
		}
		if _, ok, err := r.linesAt(p.Tree, path); err != nil {
			return nil, hash.ID{}, err
		} else if !ok {
			break // 边界：文件在这一版诞生
		}
		chain = append(chain, p)
	}
	// 反转为 [出生 … 目标]。
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	birth := chain[0].ID()

	// 2. 正向推进。
	firstLines, _, err := r.linesAt(chain[0].Tree, path)
	if err != nil {
		return nil, hash.ID{}, err
	}
	owners := make([]hash.ID, len(firstLines))
	for i := range owners {
		owners[i] = birth
	}
	prevLines := firstLines
	for i := 1; i < len(chain); i++ {
		curLines, _, err := r.linesAt(chain[i].Tree, path)
		if err != nil {
			return nil, hash.ID{}, err
		}
		owners = alignForward(prevLines, curLines, owners, chain[i].ID())
		prevLines = curLines
	}
	if len(owners) != len(targetLines) {
		return nil, hash.ID{}, fmt.Errorf(
			"blame: 内部不一致（对齐后 %d 行，目标版本 %d 行）", len(owners), len(targetLines))
	}

	if !noCache {
		c := blameCache{Owners: make([]string, len(owners)), BirthVersion: birth.String()}
		for i := range owners {
			c.Owners[i] = owners[i].String()
		}
		if raw, err := json.Marshal(c); err == nil {
			_ = r.DB.PutBlameCache(versionID.String(), path, string(raw), time.Now().UnixMilli())
		}
	}
	return owners, birth, nil
}

func (r *Repo) loadBlameCache(versionID, path string) ([]hash.ID, hash.ID, bool) {
	raw, err := r.DB.GetBlameCache(versionID, path)
	if err != nil {
		return nil, hash.ID{}, false
	}
	var c blameCache
	if err := json.Unmarshal([]byte(raw), &c); err != nil || len(c.Owners) == 0 {
		return nil, hash.ID{}, false
	}
	birth, err := hash.Parse(c.BirthVersion)
	if err != nil {
		return nil, hash.ID{}, false
	}
	out := make([]hash.ID, len(c.Owners))
	for i, s := range c.Owners {
		id, perr := hash.Parse(s)
		if perr != nil {
			return nil, hash.ID{}, false
		}
		out[i] = id
	}
	return out, birth, true
}

// alignForward 把「按 prevLines 索引的归属」推进到「按 curLines 索引的归属」。
//
// 规则：cur 侧**被改动的行**归 cur；其余行沿用上一版的归属。
//
// 定位依赖 tree.Hunks 的 StartHint 语义：StartHint 是**改动块**在新文件中的
// 起始行号（不含 before 上下文），因此 hunk 的模式起点是 StartHint-len(Before)。
func alignForward(prevLines, curLines [][]byte, prevOwners []hash.ID, curVer hash.ID) []hash.ID {
	out := make([]hash.ID, len(curLines))
	for i := range out {
		out[i] = curVer // 默认归当前版本；未变更的部分随后被覆写
	}

	prevIdx, curIdx := 0, 0
	// carry 把 n 行「两侧一致」的归属从 prev 搬到 cur。
	carry := func(n int) {
		for k := 0; k < n; k++ {
			if prevIdx+k >= len(prevOwners) || curIdx+k >= len(out) {
				break
			}
			out[curIdx+k] = prevOwners[prevIdx+k]
		}
		prevIdx += n
		curIdx += n
	}

	for _, h := range tree.Hunks(prevLines, curLines, 3) {
		patStart := h.StartHint - len(h.Before)
		if patStart < curIdx {
			patStart = curIdx // 防御：保持单调
		}
		carry(patStart - curIdx) // hunk 之前的未变更区间
		carry(len(h.Before))     // 上文（两侧一致）
		prevIdx += len(h.Remove) // 被本版改掉的行：不进 cur 索引
		curIdx += len(h.Insert)  // 本版新增的行：保持 curVer
		carry(len(h.After))      // 下文（两侧一致）
	}
	if rest := len(curLines) - curIdx; rest > 0 {
		carry(rest)
	}
	return out
}

// linesAt 读取某个树中 path 的行。
func (r *Repo) linesAt(root hash.ID, path string) ([][]byte, bool, error) {
	cur, err := r.Store.GetTree(root)
	if err != nil {
		return nil, false, err
	}
	parts := strings.Split(path, "/")
	for i, seg := range parts {
		e, ok := cur.Get(seg)
		if !ok {
			return nil, false, nil
		}
		if i == len(parts)-1 {
			if e.Kind != codec.EntryFile && e.Kind != codec.EntrySymlink {
				return nil, false, nil // 目录/其它：不是可 blame 的路径
			}
			data, err := r.Store.ReadFileData(e.ID)
			if err != nil {
				return nil, false, err
			}
			lines := tree.SplitLines(data)
			// 行切分把「以换行结尾」表示成一个尾随空元素。那是编码产物，
			// 不是用户眼里的一行，blame 的**行号**必须对应真实的行。
			if n := len(lines); n > 0 && len(lines[n-1]) == 0 {
				lines = lines[:n-1]
			}
			return lines, true, nil
		}
		cur, err = r.Store.GetTree(e.ID)
		if err != nil {
			return nil, false, err
		}
	}
	return nil, false, nil
}

// blameRecords 把逐行归属整理成输出记录（相同归属的相邻行合并成区间）。
func (r *Repo) blameRecords(path string, owners []hash.ID, birth hash.ID, from, to int) []BlameRecord {
	type jinfo struct {
		agent, task, opID string
		ts                int64
		available         bool
	}
	cache := map[hash.ID]jinfo{}
	lookup := func(id hash.ID) jinfo {
		if m, ok := cache[id]; ok {
			return m
		}
		m := jinfo{}
		if j, err := r.DB.GetJournal(id.String()); err == nil {
			m.agent, m.task, m.ts, m.available = j.Agent, j.TaskDesc, j.TS, true
			var items []struct {
				OpID string `json:"op_id"`
				Path string `json:"path"`
			}
			if json.Unmarshal([]byte(j.OpsJSON), &items) == nil {
				for _, it := range items {
					if it.Path == path {
						m.opID = it.OpID
						break
					}
				}
			}
		}
		cache[id] = m
		return m
	}

	out := make([]BlameRecord, 0, 8)
	for i := from - 1; i < to; {
		id := owners[i]
		j := i
		for j+1 < to && owners[j+1] == id {
			j++
		}
		m := lookup(id)
		out = append(out, BlameRecord{
			FromLine: i + 1, ToLine: j + 1,
			Range:      fmt.Sprintf("%d-%d", i+1, j+1),
			VersionID:  id.String(),
			OpID:       m.opID,
			Agent:      m.agent,
			Task:       m.task,
			TS:         m.ts,
			Originated: id == birth,
			Available:  m.available,
		})
		i = j + 1
	}
	return out
}
