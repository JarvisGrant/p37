package workspace

import (
	"sort"
	"sync"
)

// wsLockSet 是按 ws_id 的读写锁集合。
//
// 为什么需要它：
//
//  1. Sync 会整体替换 op-log，而 EffectiveTree 会「读 op → 算树 → 写缓存」。
//     两者交错时，EffectiveTree 可能把基于旧 op 算出的树写回缓存，而旧 op 的
//     epoch 恰好与新 op 相同。读写锁把这段窗口关掉（会话 #90 的缓存污染）。
//  2. land_into 需要在「读父子有效树 → 重放 → 写父目录」期间独占父工作区，
//     否则 ws_rebase / ws_drop / 另一次采集可以中途改写磁盘或基准。
//
// 锁序约定：Repo.landMu → wsLock(子) → wsLock(父)。只在「子 → 父」方向
// 同时持锁，工作区层级本身无环，因此不会死锁。
type wsLockSet struct {
	mu sync.Mutex
	m  map[string]*wsLockEntry
}

type wsLockEntry struct {
	refs int
	mu   sync.RWMutex
}

func newWSLockSet() *wsLockSet {
	return &wsLockSet{m: map[string]*wsLockEntry{}}
}

func (s *wsLockSet) acquire(id string) *wsLockEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.m[id]
	if e == nil {
		e = &wsLockEntry{}
		s.m[id] = e
	}
	e.refs++
	return e
}

// release 必须在解锁 RW 锁之后调用。
//
// 先 unlock 再减引用：若减引用期间有新的 acquire，它会拿到同一个 entry 并把
// refs 加回去，因此不会出现「删掉 entry 后两人各持一把新锁」的并发窗口。
func (s *wsLockSet) release(id string, e *wsLockEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.refs--
	if e.refs <= 0 {
		delete(s.m, id)
	}
}

func (s *wsLockSet) lock(id string) func() {
	e := s.acquire(id)
	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		s.release(id, e)
	}
}

func (s *wsLockSet) rlock(id string) func() {
	e := s.acquire(id)
	e.mu.RLock()
	return func() {
		e.mu.RUnlock()
		s.release(id, e)
	}
}

// LockWorkspace 取工作区写锁，返回解锁函数。
//
// 调用方在持锁期间如果要调用 Sync / EffectiveTree / Apply*，必须使用对应的
// *Locked 变体（Go 的 RWMutex 不可重入）。
func (m *Manager) LockWorkspace(wsID string) func() { return m.wsLocks.lock(wsID) }

// RLockWorkspace 取工作区读锁，返回解锁函数。
func (m *Manager) RLockWorkspace(wsID string) func() { return m.wsLocks.rlock(wsID) }

// LockWorkspaces 按 ws_id 升序取多把写锁，返回按相反顺序释放的函数。
//
// 固定升序可以避免「两个调用方各持一把、互相等待」的死锁。栈 land 需要同时
// 锁住折叠集合里的全部工作区，避免 ws_rebase 在中途改写 op-log。
func (m *Manager) LockWorkspaces(ids []string) func() {
	seen := make(map[string]bool, len(ids))
	uniq := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		uniq = append(uniq, id)
	}
	sort.Strings(uniq)
	unlocks := make([]func(), 0, len(uniq))
	for _, id := range uniq {
		unlocks = append(unlocks, m.wsLocks.lock(id))
	}
	return func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
}
