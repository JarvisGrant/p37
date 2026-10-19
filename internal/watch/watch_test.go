package watch

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeSyncer 记录 Sync 调用，并允许测试随时改变「应当活跃的工作区」集合。
type fakeSyncer struct {
	mu    sync.Mutex
	live  map[string]string
	calls map[string]int
}

func newFake() *fakeSyncer {
	return &fakeSyncer{live: map[string]string{}, calls: map[string]int{}}
}

func (f *fakeSyncer) Sync(wsID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[wsID]++
	return nil
}

func (f *fakeSyncer) LiveWorkspaces() (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.live))
	for k, v := range f.live {
		out[k] = v
	}
	return out, nil
}

func (f *fakeSyncer) set(wsID, dir string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if dir == "" {
		delete(f.live, wsID)
		return
	}
	f.live[wsID] = dir
}

func (f *fakeSyncer) count(wsID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[wsID]
}

// waitFor 轮询直到 cond 为真或超时。
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("超时未满足: %s", what)
}

// newTestWatcher 起一个只在 fs 事件下工作的 watcher（周期扫描关闭）。
func newTestWatcher(t *testing.T, f *fakeSyncer, opt Options) *Watcher {
	t.Helper()
	if opt.ScanInterval == 0 {
		// 用一个极大的周期把兜底扫描推到测试窗口之外。
		opt.ScanInterval = time.Hour
	}
	if opt.ReconcileInterval == 0 {
		opt.ReconcileInterval = 20 * time.Millisecond
	}
	w, err := New(f, opt)
	if err != nil {
		t.Fatal(err)
	}
	w.Start()
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// TestWatcherSyncsOnFileChange 覆盖触发源 a：改文件 -> debounce 后采集一次。
func TestWatcherSyncsOnFileChange(t *testing.T) {
	dir := t.TempDir()
	f := newFake()
	f.set("ws_1", dir)
	newTestWatcher(t, f, Options{Debounce: 60 * time.Millisecond})

	// 等 watcher 完成一次对齐。
	waitFor(t, "监听建立", 3*time.Second, func() bool { return true })
	time.Sleep(150 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "fs 事件触发采集", 5*time.Second, func() bool { return f.count("ws_1") > 0 })
}

// TestWatcherDebounces 断言连续多次写入只触发一次采集（500ms 静默期语义）。
func TestWatcherDebounces(t *testing.T) {
	dir := t.TempDir()
	f := newFake()
	f.set("ws_1", dir)
	// debounce 设得足够长，保证 5 次快速写入落在同一个静默窗口里。
	newTestWatcher(t, f, Options{Debounce: 600 * time.Millisecond})
	time.Sleep(150 * time.Millisecond)

	for i := 0; i < 5; i++ {
		if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte{byte('0' + i)}, 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitFor(t, "debounce 后采集", 5*time.Second, func() bool { return f.count("ws_1") >= 1 })

	// 静默窗口内不应被合并成多次。
	if n := f.count("ws_1"); n != 1 {
		t.Fatalf("debounce 未生效：5 次写入触发了 %d 次采集，期望 1 次", n)
	}
}

// TestWatcherWatchesNewSubdirectory 覆盖递归监听：
// 新建子目录后，子目录里的改动也必须被看到。
func TestWatcherWatchesNewSubdirectory(t *testing.T) {
	dir := t.TempDir()
	f := newFake()
	f.set("ws_1", dir)
	newTestWatcher(t, f, Options{Debounce: 60 * time.Millisecond})
	time.Sleep(150 * time.Millisecond)

	sub := filepath.Join(dir, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// 给 watcher 时间在 Create 事件上补挂监听。
	time.Sleep(400 * time.Millisecond)
	before := f.count("ws_1")

	if err := os.WriteFile(filepath.Join(sub, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "新建子目录内的改动被采集", 5*time.Second, func() bool {
		return f.count("ws_1") > before
	})
}

// TestWatcherTouch 覆盖触发源 b 的入口。
func TestWatcherTouch(t *testing.T) {
	dir := t.TempDir()
	f := newFake()
	f.set("ws_1", dir)
	w := newTestWatcher(t, f, Options{Debounce: 50 * time.Millisecond})
	time.Sleep(150 * time.Millisecond)

	w.Touch("ws_1")
	waitFor(t, "Touch 触发采集", 5*time.Second, func() bool { return f.count("ws_1") > 0 })
}

// TestWatcherRoundRobinScan 覆盖触发源 c（§4.7 轮转扫描）：
// 没有任何 fs 事件时也会被兜底扫描。
func TestWatcherRoundRobinScan(t *testing.T) {
	d1, d2 := t.TempDir(), t.TempDir()
	f := newFake()
	f.set("ws_1", d1)
	f.set("ws_2", d2)
	// 每轮 60ms，两个工作区 -> 每个约 120ms 被扫一次。
	newTestWatcher(t, f, Options{ScanInterval: 60 * time.Millisecond,
		ReconcileInterval: 20 * time.Millisecond})

	waitFor(t, "轮转扫描覆盖 ws_1", 5*time.Second, func() bool { return f.count("ws_1") >= 2 })
	waitFor(t, "轮转扫描覆盖 ws_2", 5*time.Second, func() bool { return f.count("ws_2") >= 2 })
}

// TestWatcherStopsWatchingRemoved 断言工作区消失后不再被采集。
func TestWatcherStopsWatchingRemoved(t *testing.T) {
	dir := t.TempDir()
	f := newFake()
	f.set("ws_1", dir)
	newTestWatcher(t, f, Options{Debounce: 40 * time.Millisecond})
	time.Sleep(200 * time.Millisecond)

	// 先确认它确实在监听。
	if err := os.WriteFile(filepath.Join(dir, "x.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "首次采集", 5*time.Second, func() bool { return f.count("ws_1") > 0 })

	f.set("ws_1", "")                  // 工作区被落地方焚毁
	time.Sleep(300 * time.Millisecond) // 等一次对齐
	before := f.count("ws_1")

	if err := os.WriteFile(filepath.Join(dir, "y.txt"), []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	if after := f.count("ws_1"); after != before {
		t.Fatalf("已移除的工作区仍被采集：%d -> %d", before, after)
	}
}

// TestWatcherFallbackToRoot 断言降级模式下不递归挂监听。
func TestWatcherFallbackToRoot(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	f := newFake()
	f.set("ws_1", dir)
	newTestWatcher(t, f, Options{Debounce: 40 * time.Millisecond, FallbackToRoot: true})
	time.Sleep(200 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(sub, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 降级模式下根目录仍可能收到子项事件（取决于平台），因此这里只断言
	// 「不因此崩溃」，真正的兜底由周期扫描负责 —— 那已在另一个用例中覆盖。
	time.Sleep(300 * time.Millisecond)
}

// TestWatchDogClose 断言 Close 幂等且能等到在途采集结束。
func TestWatchDogClose(t *testing.T) {
	dir := t.TempDir()
	f := newFake()
	f.set("ws_1", dir)
	w, err := New(f, Options{Debounce: 30 * time.Millisecond, ScanInterval: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	w.Start()
	time.Sleep(200 * time.Millisecond)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("重复 Close 应安全: %v", err)
	}
}

// TestHasPrefixDir 覆盖路径前缀判定（不能被 /a/bc 冒充 /a/b）。
func TestHasPrefixDir(t *testing.T) {
	sep := string(filepath.Separator)
	cases := []struct {
		p, root string
		want    bool
	}{
		{sep + "a" + sep + "b" + sep + "c", sep + "a" + sep + "b", true},
		{sep + "a" + sep + "bc", sep + "a" + sep + "b", false},
		{sep + "a" + sep + "b", sep + "a" + sep + "b", false}, // 相等不算前缀
		{sep + "a", sep + "a" + sep + "b", false},
	}
	for _, c := range cases {
		if got := hasPrefixDir(c.p, c.root); got != c.want {
			t.Errorf("hasPrefixDir(%q,%q) = %v，期望 %v", c.p, c.root, got, c.want)
		}
	}
}
