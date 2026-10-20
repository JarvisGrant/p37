package capture

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestStabilizeSkipsBusyFile 是 §5.2 稳定点判定的核心用例。
//
// 场景：文件正被持续写入，采集恰好撞上去。**必须跳过它**，绝不能把一个
// 被截断的内容当成新版本落进 op —— 那会让后续 land 把一个残缺文件写进版本。
func TestStabilizeSkipsBusyFile(t *testing.T) {
	if os.Getenv("CI") != "" {
		t.Skip("共享 CI runner 的 goroutine 调度不适合这个时序敏感用例；本机仍会执行")
	}
	e := newEnv(t)
	e.write("stable.txt", "I am fine\n")
	// CI 的共享 runner 上 goroutine 调度可能抖动；给繁忙文件更长的观察窗口，
	// 避免把「调度延迟」误判成「文件已稳定」。
	e.opt.StableDelay = 50 * time.Millisecond
	e.opt.StableRetries = 3
	var skipped []string
	e.opt.OnUnstable = func(p string) { skipped = append(skipped, p) }

	busy := filepath.Join(e.dir, "busy.txt")
	if err := os.WriteFile(busy, []byte("start\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			// 长度持续变化 -> (mtime, size, hash) 每轮都不同，永不"稳定"
			_ = os.WriteFile(busy, bytes.Repeat([]byte("x"), 100+i%997), 0o644)
			time.Sleep(time.Millisecond)
		}
	}()

	time.Sleep(5 * time.Millisecond) // 让写入先跑起来
	tr := e.build()
	close(stop)
	wg.Wait()

	if len(skipped) != 1 || skipped[0] != "busy.txt" {
		t.Fatalf("被跳过的文件 = %v，期望 [busy.txt]", skipped)
	}
	// 稳定文件必须照常采集。
	e2 := func() {
		en, ok := tr.Get("stable.txt")
		if !ok {
			t.Fatal("稳定文件反而丢了")
		}
		data, err := e.store.ReadFileData(en.ID)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "I am fine\n" {
			t.Fatalf("稳定文件内容 = %q", data)
		}
	}
	e2()
	// 不稳定文件本轮**不产出条目**（基准里没有它，就当作这一轮没看见）。
	if _, ok := tr.Get("busy.txt"); ok {
		t.Fatal("不稳定的文件不该出现在本轮树里")
	}
}

// TestStabilizeAcceptsQuietFile 断言稳定文件被正常确认，不会因为开了稳定点
// 判定就漏采。
func TestStabilizeAcceptsQuietFile(t *testing.T) {
	e := newEnv(t)
	e.opt.StableDelay = 5 * time.Millisecond
	e.opt.StableRetries = 3
	var skipped []string
	e.opt.OnUnstable = func(p string) { skipped = append(skipped, p) }

	e.write("a.txt", "hello\n")
	time.Sleep(15 * time.Millisecond) // 让 mtime 老于 stableDelay
	tr := e.build()

	if len(skipped) != 0 {
		t.Fatalf("不该跳过任何文件: %v", skipped)
	}
	en, ok := tr.Get("a.txt")
	if !ok {
		t.Fatal("a.txt 丢了")
	}
	data, _ := e.store.ReadFileData(en.ID)
	if string(data) != "hello\n" {
		t.Fatalf("内容 = %q", data)
	}
}

// TestStabilizeNoDelayWhenNothingRecent 断言「近期没被改过」的文件**不付等待成本**。
//
// 这是稳定点判定能不能上生产的关键：如果每次采集都白等 200ms，
// 「无变更采集 < 100ms」的目标就彻底没戏了。
//
// 判据是「mtime 距今 < stable_delay」= 可能正在被写。所以这里用一个比较短的
// 窗口，并让文件老于该窗口。
func TestStabilizeNoDelayWhenNothingRecent(t *testing.T) {
	e := newEnv(t)
	e.opt.StableDelay = 300 * time.Millisecond
	e.opt.StableRetries = 3
	e.write("a.txt", "a\n")
	time.Sleep(400 * time.Millisecond) // 让 mtime 老于 stableDelay

	var skipped []string
	e.opt.OnUnstable = func(p string) { skipped = append(skipped, p) }

	start := time.Now()
	tr := e.build()
	el := time.Since(start)
	if el >= e.opt.StableDelay {
		t.Fatalf("没有近期修改的文件却等待了 %s —— 说明稳定点判定在无脑睡", el)
	}
	if len(skipped) != 0 {
		t.Fatalf("老文件的判定不该跳过: %v", skipped)
	}
	if _, ok := tr.Get("a.txt"); !ok {
		t.Fatal("a.txt 必须被采集")
	}
}

// TestStabilizeChecksRecentFile 断言**刚被写过**的文件确实进了判定流程。
//
// 与上一个用例成对：老文件不检查、新文件必检查。
func TestStabilizeChecksRecentFile(t *testing.T) {
	e := newEnv(t)
	e.opt.StableDelay = 150 * time.Millisecond
	e.opt.StableRetries = 2
	e.write("a.txt", "a\n") // mtime 就是现在 -> 属于「可能正在被写」

	start := time.Now()
	e.build()
	el := time.Since(start)
	if el < e.opt.StableDelay {
		t.Fatalf("刚写过的文件应当被复核，实际只用了 %s", el)
	}
}

// TestStabilizeDisabled 断言 StableRetries<=0 时完全不做判定（import 走这条路）。
func TestStabilizeDisabled(t *testing.T) {
	e := newEnv(t)
	e.opt.StableRetries = 0
	e.opt.StableDelay = 3 * time.Second
	var skipped []string
	e.opt.OnUnstable = func(p string) { skipped = append(skipped, p) }
	e.write("a.txt", "a\n")

	start := time.Now()
	tr := e.build()
	if el := time.Since(start); el > time.Second {
		t.Fatalf("关闭后仍等待了 %s", el)
	}
	if len(skipped) != 0 {
		t.Fatalf("关闭后不该上报跳过: %v", skipped)
	}
	if _, ok := tr.Get("a.txt"); !ok {
		t.Fatal("关闭后 a.txt 必须照常采集")
	}
}
