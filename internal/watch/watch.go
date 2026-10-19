// Package watch 实现 §5.2 的采集触发源 a（fs 事件）与 c（周期全量扫描），
// 以及 §4.7 的轮转调度。
//
// 触发源 b（以工作区为目标的 MCP 调用）在 mcpsrv 里，不在这里。
//
// 本包只负责「什么时候该采集」，**不做采集本身** —— 通过 Syncer 回调交给
// workspace.Manager.Sync。这样它既不需要懂树，也不需要懂落盘。
package watch

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Syncer 是采集动作。实现必须是幂等的：同一工作区连续调用两次
// 只有第一次产生 op。
type Syncer interface {
	// Sync 采集一个工作区。
	Sync(wsID string) error
	// LiveWorkspaces 返回当前应当被监听的工作区（id → 目录）。
	//
	// 由调用方（repo）提供，因此本包不需要依赖 meta 包。
	LiveWorkspaces() (map[string]string, error)
}

// Options 控制 watcher 的行为。
type Options struct {
	// Debounce 是 fs 事件的静默期（§5.2，默认 500ms）。
	Debounce time.Duration
	// ScanInterval 是每个工作区的兜底扫描周期（§4.7，默认 30s）。
	// 0 表示关闭周期扫描。
	ScanInterval time.Duration
	// ReconcileInterval 是「重新对齐监听集合」的间隔（默认 1s）。
	ReconcileInterval time.Duration
	// FallbackToRoot 为真时只监听工作区根目录（不递归）。
	//
	// 附录 G.3：当 fs.inotify.max_user_watches 偏低时启用，牺牲延迟换取
	// 不耗尽 watch 额度。此时深层改动只能靠周期扫描发现。
	FallbackToRoot bool
	// Logger 用于记录非致命错误；nil 表示静默。
	Logger *log.Logger
	// OnSync 在每次成功采集后回调（可观测性用，可为 nil）。
	OnSync func(wsID string, took time.Duration)
}

func (o *Options) withDefaults() {
	if o.Debounce <= 0 {
		o.Debounce = 500 * time.Millisecond
	}
	if o.ScanInterval < 0 {
		o.ScanInterval = 0
	}
	if o.ScanInterval == 0 {
		o.ScanInterval = 30 * time.Second
	}
	if o.ReconcileInterval <= 0 {
		o.ReconcileInterval = time.Second
	}
	if o.Logger == nil {
		o.Logger = log.New(os.Stderr, "p37d ", log.LstdFlags|log.LUTC)
	}
}

// tick 是主循环的粒度。所有调度都在这一个 goroutine 里推进，因此
// 下面的状态都不需要额外加锁。
const tick = 50 * time.Millisecond

// Watcher 把 fs 事件与周期扫描合流成「该采集某某工作区」的调度器。
type Watcher struct {
	sync Syncer
	opt  Options

	fsw *fsnotify.Watcher

	// 以下字段只在 loop goroutine 里读写。
	ws       map[string]*wsState // wsID -> 状态
	byDir    map[string]string   // 目录 -> wsID（用于事件归属）
	order    []string            // 轮转顺序（已排序，保证确定性）
	rrCursor int

	// inflight / closed 跨 goroutine 共享（loop 写、采集 goroutine 清）。
	mu       sync.Mutex
	inflight map[string]bool
	closed   bool

	// touches 是外部提示（触发源 b）的入口。
	touches chan string

	done chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

type wsState struct {
	id      string
	dir     string
	dirty   bool
	due     time.Time // debounce 到期时刻
	scanned time.Time
}

// New 创建 watcher。Start 之前不会监听任何东西。
func New(s Syncer, opt Options) (*Watcher, error) {
	opt.withDefaults()
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("watch: 创建 fsnotify 失败: %w", err)
	}
	return &Watcher{
		sync:     s,
		opt:      opt,
		fsw:      fsw,
		ws:       map[string]*wsState{},
		byDir:    map[string]string{},
		inflight: map[string]bool{},
		touches:  make(chan string, 256),
		done:     make(chan struct{}),
	}, nil
}

// Start 启动主循环。
func (w *Watcher) Start() {
	w.wg.Add(1)
	go w.loop()
}

// Close 停止监听并等待在途采集结束。
//
// 幂等：重复调用安全。
func (w *Watcher) Close() error {
	var err error
	w.once.Do(func() {
		// 在 mu 保护下置 closed，保证 dispatch 不会在 Wait 开始之后才 Add。
		w.mu.Lock()
		w.closed = true
		w.mu.Unlock()
		close(w.done)
		err = w.fsw.Close()
	})
	w.wg.Wait()
	return err
}

// Touch 手动把一个工作区标记为「可能脏了」。
//
// 用于触发源 b（MCP 调用后）与外部提示：它只是提前把该工作区排进 debounce
// 队列，真正的「哪些文件变了」仍由 Sync 的 stat 缓存判定。
// 非阻塞：队列满时直接丢弃（下一次周期扫描兜底）。
func (w *Watcher) Touch(wsID string) {
	select {
	case w.touches <- wsID:
	default:
	}
}

// loop 是唯一的调度者。
func (w *Watcher) loop() {
	defer w.wg.Done()

	t := time.NewTicker(tick)
	defer t.Stop()

	lastReconcile := time.Time{}
	for {
		select {
		case <-w.done:
			return
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.onEvent(ev)
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			w.logf("fs 监听错误: %v", err)
		case id := <-w.touches:
			if st, ok := w.ws[id]; ok {
				st.dirty = true
				st.due = time.Now().Add(w.opt.Debounce)
			}
		case now := <-t.C:
			if now.Sub(lastReconcile) >= w.opt.ReconcileInterval {
				lastReconcile = now
				w.reconcile()
			}
			w.fireDue(now)
			w.scanRoundRobin(now)
		}
	}
}

// onEvent 把一个 fs 事件归到某个工作区并推迟 debounce。
func (w *Watcher) onEvent(ev fsnotify.Event) {
	p := ev.Name

	// 新建目录：补挂监听（递归），否则子目录里的改动会漏掉。
	if ev.Op&fsnotify.Create != 0 {
		if fi, err := os.Lstat(p); err == nil && fi.IsDir() {
			if wsID, ok := w.ownerOf(p); ok {
				_ = w.addRecursive(wsID, p)
			}
		}
	}

	wsID, ok := w.ownerOf(p)
	if !ok {
		return
	}
	st := w.ws[wsID]
	if st == nil {
		return
	}
	st.dirty = true
	st.due = time.Now().Add(w.opt.Debounce)
}

// ownerOf 找到路径所属的工作区（向上回溯到已登记的监听目录）。
func (w *Watcher) ownerOf(path string) (string, bool) {
	for {
		if id, ok := w.byDir[path]; ok {
			return id, true
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", false
		}
		path = parent
	}
}

// reconcile 让监听集合与「应当活跃的工作区」对齐。
func (w *Watcher) reconcile() {
	live, err := w.sync.LiveWorkspaces()
	if err != nil {
		w.logf("列举工作区失败: %v", err)
		return
	}

	for id, dir := range live {
		if _, ok := w.ws[id]; ok {
			continue
		}
		st := &wsState{id: id, dir: dir}
		w.ws[id] = st
		if err := w.addRecursive(id, dir); err != nil {
			w.logf("监听 %s 失败: %v", id, err)
		}
	}

	for id, st := range w.ws {
		if _, ok := live[id]; ok {
			continue
		}
		w.removeWatches(st.dir)
		delete(w.ws, id)
	}

	// 维持确定的轮转顺序（按 id 排序，避免 map 迭代顺序影响行为）。
	next := make([]string, 0, len(w.ws))
	for id := range w.ws {
		next = append(next, id)
	}
	sort.Strings(next)
	w.order = next
	if w.rrCursor >= len(w.order) {
		w.rrCursor = 0
	}
}

// addRecursive 递归登记一个目录树。
func (w *Watcher) addRecursive(wsID, root string) error {
	w.byDir[root] = wsID
	if err := w.fsw.Add(root); err != nil {
		// 目录可能刚被删掉；这不是致命错误。
		if errors.Is(err, fs.ErrNotExist) {
			delete(w.byDir, root)
			return nil
		}
		return err
	}
	if w.opt.FallbackToRoot {
		return nil
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 单个子目录读不了不影响其它
		}
		if !d.IsDir() || p == root {
			return nil
		}
		w.byDir[p] = wsID
		if aerr := w.fsw.Add(p); aerr != nil {
			delete(w.byDir, p)
		}
		return nil
	})
}

// removeWatches 摘掉一个目录树的全部监听。
func (w *Watcher) removeWatches(root string) {
	for dir, id := range w.byDir {
		if id != "" && (dir == root || hasPrefixDir(dir, root)) {
			_ = w.fsw.Remove(dir)
			delete(w.byDir, dir)
		}
	}
}

// hasPrefixDir 报告 p 是否位于 root 之下（以路径分段比较）。
func hasPrefixDir(p, root string) bool {
	if len(p) <= len(root)+1 {
		return false
	}
	return p[:len(root)] == root && p[len(root)] == filepath.Separator
}

// fireDue 触发所有已过静默期的工作区。
func (w *Watcher) fireDue(now time.Time) {
	for _, st := range w.ws {
		if !st.dirty || now.Before(st.due) {
			continue
		}
		st.dirty = false
		st.scanned = now
		w.dispatch(st)
	}
}

// scanRoundRobin 是 §4.7 的轮转扫描：一次只扫一个工作区，
// 间隔 = scan_interval / 工作区数，从而把负载摊平。
func (w *Watcher) scanRoundRobin(now time.Time) {
	if w.opt.ScanInterval <= 0 || len(w.order) == 0 {
		return
	}
	per := w.opt.ScanInterval / time.Duration(len(w.order))
	if per < tick {
		per = tick
	}
	// 一次 tick 里最多推进一个（严格轮转，避免峰值）。
	for i := 0; i < len(w.order); i++ {
		id := w.order[w.rrCursor%len(w.order)]
		w.rrCursor++
		st, ok := w.ws[id]
		if !ok {
			continue
		}
		if now.Sub(st.scanned) < per {
			continue
		}
		st.scanned = now
		w.dispatch(st)
		return
	}
}

// dispatch 在后台跑一次采集；同一工作区同时只允许一个在途采集。
func (w *Watcher) dispatch(st *wsState) {
	w.mu.Lock()
	if w.closed || w.inflight[st.id] {
		w.mu.Unlock()
		return
	}
	w.inflight[st.id] = true
	// Add 与 closed 检查同在 mu 保护下，因此 Add 必然发生在 Close 的 Wait 之前。
	w.wg.Add(1)
	w.mu.Unlock()

	go func() {
		defer w.wg.Done()
		defer func() {
			w.mu.Lock()
			delete(w.inflight, st.id)
			w.mu.Unlock()
		}()

		start := time.Now()
		if err := w.sync.Sync(st.id); err != nil {
			// 状态不对（rebasing / integrating / broken / 未挂载）是**正常**的：
			// watcher 是只读触发源，它的判断必然落后于状态变化，交给下次即可。
			w.logf("自动采集 %s 失败: %v", st.id, err)
			return
		}
		if w.opt.OnSync != nil {
			w.opt.OnSync(st.id, time.Since(start))
		}
	}()
}

func (w *Watcher) logf(format string, args ...any) {
	if w.opt.Logger != nil {
		w.opt.Logger.Printf(format, args...)
	}
}
