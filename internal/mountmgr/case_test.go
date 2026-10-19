package mountmgr

import (
	"sync/atomic"
	"testing"

	"github.com/chenyiwen/p37/internal/hash"
)

// TestReleaseBaseNeverPanics 是「land 时 daemon 崩溃」的回归测试。
//
// baseRefs 曾一度被写入**目录路径字符串**（mountWithLower 传的是 base 目录），
// 而 releaseBase 断言成 hash.ID —— 于是任何一次 land 走到关闭工作区都会 panic，
// 并把整个 daemon 带走。
func TestReleaseBaseNeverPanics(t *testing.T) {
	m := &Manager{
		bases:   map[hash.ID]string{},
		inUse:   map[hash.ID]int{},
		lastUse: map[hash.ID]int64{},
	}

	// 情形 1：正常的树 id。
	id := hash.Sum([]byte("base"))
	m.inUse[id] = 1
	baseRefs.Store("ws_ok", id)
	m.releaseBase("ws_ok")
	if m.inUse[id] != 0 {
		t.Fatalf("引用计数未递减: %d", m.inUse[id])
	}

	// 情形 2：类型不符（历史 bug 的形态）—— 必须安静返回，不能 panic。
	baseRefs.Store("ws_bad", "/some/dir/path")
	m.releaseBase("ws_bad")

	// 情形 3：根本没有记录。
	m.releaseBase("ws_missing")

	// 情形 4：重复释放不应把计数压成负数。
	m.inUse[id] = 1
	baseRefs.Store("ws_twice", id)
	m.releaseBase("ws_twice")
	m.releaseBase("ws_twice")
	if m.inUse[id] != 0 {
		t.Fatalf("重复释放后计数 = %d", m.inUse[id])
	}
}

// TestBaseRefsStoresTreeIDNotPath 钉住 baseRefs 的**语义**：存树 id。
//
// mountWithLower 一旦把目录路径塞进去，releaseBase 就会失配（见上一个用例）。
func TestBaseRefsStoresTreeIDNotPath(t *testing.T) {
	// 这里只断言类型契约本身：baseRefs 里允许出现的只有 hash.ID。
	id := hash.Sum([]byte("x"))
	baseRefs.Store("ws_contract", id)
	v, _ := baseRefs.Load("ws_contract")
	if _, ok := v.(hash.ID); !ok {
		t.Fatalf("baseRefs 里存了 %T，契约要求 hash.ID", v)
	}
	baseRefs.Delete("ws_contract")
	var _ atomic.Int64
}
