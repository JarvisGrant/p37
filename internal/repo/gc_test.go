package repo_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyiwen/p37/internal/repo"
	"github.com/chenyiwen/p37/internal/workspace"
)

// gcEnv 起一个仓库，并以指定的 GC 参数重新打开。
type gcEnv struct {
	*env
	root string
}

func newGCEnv(t *testing.T, keepVersions int) *gcEnv {
	t.Helper()
	e := newEnv(t)
	// newEnv 已经 Init + 持有 r；这里换一个带 GC 选项的句柄。
	_ = e.r.Close()
	r, err := repo.Open(e.root, repo.Options{
		GC: repo.GCOptions{KeepVersions: keepVersions, KeepEpochs: 20, ScrubSample: 1, VerifyOnRead: "sample"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	e.r = r
	return &gcEnv{env: e, root: e.root}
}

// TestGCPrunesOldVersionsAndSweepsObjects 覆盖 §10 的主路径。
func TestGCPrunesOldVersionsAndSweepsObjects(t *testing.T) {
	e := newGCEnv(t, 1) // 只保留最近 1 个版本
	e.seed("a.txt", "v0\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	// 造 3 个版本，每次写入一段独特且较大的内容（保证对象不可共享）。
	var versions []string
	for i := 0; i < 3; i++ {
		w := e.open("", "改一版")
		body := ""
		for j := 0; j < 40; j++ {
			body += "line-" + string(rune('A'+i)) + "-" + string(rune('0'+j%10)) + "\n"
		}
		e.write(w.ID, "a.txt", body)
		res, err := e.r.Land(w.ID)
		if err != nil {
			t.Fatal(err)
		}
		versions = append(versions, res.VersionID.String())
	}

	before, err := e.r.Store.ListObjects()
	if err != nil {
		t.Fatal(err)
	}

	// 预演：必须什么都不改。
	plan, err := e.r.GC(true)
	if err != nil {
		t.Fatal(err)
	}
	if plan.VersionDrop == 0 {
		t.Fatalf("期望有版本可剪枝，得到 %+v", plan)
	}
	if plan.DryRun != true || plan.ObjectsDeleted != 0 || plan.VersionsPruned != 0 {
		t.Fatalf("预演不该动任何东西: %+v", plan)
	}
	mid, _ := e.r.Store.ListObjects()
	if len(mid) != len(before) {
		t.Fatalf("预演改变了对象数: %d -> %d", len(before), len(mid))
	}

	// 执行。
	rep, err := e.r.GC(false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.VersionsPruned != plan.VersionDrop {
		t.Fatalf("剪枝 %d 个，计划是 %d", rep.VersionsPruned, plan.VersionDrop)
	}
	if rep.ObjectsDeleted == 0 {
		t.Fatal("应当回收掉一些对象")
	}

	// ★ 关键：head 的整棵树必须仍然完整可读。
	head, _, _ := e.r.Head()
	if err := e.r.WalkHead(head, nil); err != nil {
		t.Fatalf("GC 之后 head 链不可走: %v", err)
	}
	if _, err := e.r.VersionShow(head); err != nil {
		t.Fatalf("GC 之后 head 版本不可读: %v", err)
	}
	// 全量读一遍 head 的树，确认没有悬空引用。
	v, _ := e.r.DB.GetVersion(head)
	if _, err := e.r.Store.GetTree(v.Tree); err != nil {
		t.Fatalf("head 树丢失: %v", err)
	}
}

// TestGCKeepsActiveWorkspaceObjects 覆盖 §10.1 第 3、4 条根：
// 活跃工作区的 base_tree 与 **op 引用的内容对象**都不能被扫掉。
//
// 第 4 条规格里没写但必须做：采集产出的 FileContent 只被 op 表引用，
// 不属于任何版本的树。漏了它，下一次 land 会读到已被删除的对象。
func TestGCKeepsActiveWorkspaceObjects(t *testing.T) {
	e := newGCEnv(t, 1)
	e.seed("a.txt", "base\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	// 落几版，把历史推远。
	for i := 0; i < 3; i++ {
		w := e.open("", "推历史")
		e.write(w.ID, "a.txt", "hist-"+string(rune('A'+i))+"\n")
		if _, err := e.r.Land(w.ID); err != nil {
			t.Fatal(err)
		}
	}

	// 开一个工作区并改一个**全新**文件 —— 它的内容只被 op 引用。
	w := e.open("", "只被 op 引用的改动")
	e.write(w.ID, "brand-new.txt", "这段内容不属于任何版本的树\n")
	if err := e.r.WS.Sync(w.ID); err != nil {
		t.Fatal(err)
	}
	ops, _ := e.r.DB.ListOps(w.ID)
	if len(ops) == 0 {
		t.Fatal("没有采集到 op")
	}
	newFile := ops[0].NewFile
	if newFile.IsZero() {
		t.Fatal("op 未携带 NewFile")
	}
	if _, _, err := e.r.Store.Get(newFile); err != nil {
		t.Fatalf("前置条件不成立，NewFile 不可读: %v", err)
	}

	if _, err := e.r.GC(false); err != nil {
		t.Fatal(err)
	}

	// ★ op 引用的对象必须还在。
	if _, _, err := e.r.Store.Get(newFile); err != nil {
		t.Fatalf("★ GC 把活跃工作区 op 引用的对象扫掉了: %v", err)
	}
	// ★ 而且这个工作区必须还能正常落地。
	res, err := e.r.Land(w.ID)
	if err != nil {
		t.Fatalf("★ GC 之后工作区无法落地: %v", err)
	}
	head, _, _ := e.r.Head()
	if res.VersionID != head {
		t.Fatal("落地版本不是 head")
	}
}

// TestGCRetentionKeepsWorkspaceVersions 断言被工作区引用的版本不会被剪枝。
func TestGCRetentionKeepsWorkspaceVersions(t *testing.T) {
	e := newGCEnv(t, 1)
	e.seed("a.txt", "a\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		w := e.open("", "推历史")
		e.write(w.ID, "a.txt", "h"+string(rune('A'+i))+"\n")
		if _, err := e.r.Land(w.ID); err != nil {
			t.Fatal(err)
		}
	}
	// 记录当前 head，然后开一个挂在它上面的工作区。
	oldHead, _, _ := e.r.Head()
	w := e.open("", "挂在旧 head 上")

	rep, err := e.r.GC(false)
	if err != nil {
		t.Fatal(err)
	}
	// 工作区的基准版本必须还在。
	if _, err := e.r.DB.GetVersion(oldHead); err != nil {
		t.Fatalf("工作区依赖的版本被剪掉了: %v（报告 %+v）", err, rep)
	}
	// 工作区仍可用。
	if err := e.r.WS.Sync(w.ID); err != nil {
		t.Fatalf("剪枝后工作区不可用: %v", err)
	}
}

// TestGCToleratesPrunedChain 断言剪枝之后版本链的读取不会崩。
func TestGCToleratesPrunedChain(t *testing.T) {
	e := newGCEnv(t, 1)
	e.seed("a.txt", "a\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		w := e.open("", "推历史")
		e.write(w.ID, "a.txt", "t"+string(rune('A'+i))+"\n")
		if _, err := e.r.Land(w.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.r.GC(false); err != nil {
		t.Fatal(err)
	}
	head, _, _ := e.r.Head()
	// WalkHead 必须正常结束（而不是因为父代被删而报错）。
	if err := e.r.WalkHead(head, nil); err != nil {
		t.Fatalf("WalkHead 不接受已剪枝的链: %v", err)
	}
	// blame 同理。
	if _, err := e.r.Blame("a.txt", repo.BlameOptions{}); err != nil {
		t.Fatalf("blame 不接受已剪枝的链: %v", err)
	}
}

// TestGCIdempotent 断言连续 GC 第二次是空操作。
func TestGCIdempotent(t *testing.T) {
	e := newGCEnv(t, 1)
	e.seed("a.txt", "a\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		w := e.open("", "推历史")
		e.write(w.ID, "a.txt", "z"+string(rune('A'+i))+"\n")
		if _, err := e.r.Land(w.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.r.GC(false); err != nil {
		t.Fatal(err)
	}
	second, err := e.r.GC(false)
	if err != nil {
		t.Fatal(err)
	}
	if second.ObjectDrop != 0 || second.VersionDrop != 0 {
		t.Fatalf("第二次 GC 应当无事可做，得到 %+v", second)
	}
}

// TestGCWorkspaceSurvivesViaStatCacheClear 断言 GC 清空 stat 缓存之后，
// 下一次采集仍然正确（不会因为悬空 id 而烂树）。
func TestGCWorkspaceSurvivesViaStatCacheClear(t *testing.T) {
	e := newGCEnv(t, 1)
	e.seed("a.txt", "a\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	w := e.open("", "改文件")
	e.write(w.ID, "a.txt", "changed\n")
	e.write(w.ID, "sub/new.txt", "nested\n")
	if err := e.r.WS.Sync(w.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.r.DB.StatCache().CountAll(); n == 0 {
		t.Fatal("前置条件：应当有 stat 缓存")
	}

	if _, err := e.r.GC(false); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.r.DB.StatCache().CountAll(); n != 0 {
		t.Fatalf("GC 后 stat 缓存应被清空，仍有 %d 行", n)
	}

	// 清空后重新采集：必须仍然得出正确结果（内容相同 -> 无变化）。
	if err := e.r.WS.Sync(w.ID); err != nil {
		t.Fatalf("GC 之后重新采集失败: %v", err)
	}
	res, err := e.r.Land(w.ID)
	if err != nil {
		t.Fatalf("GC 之后落地失败: %v", err)
	}
	v, _ := e.r.DB.GetVersion(res.VersionID)
	tr, _ := e.r.Store.GetTree(v.Tree)
	if got, err := readTreeFile(e.env, tr, "a.txt"); err != nil || got != "changed\n" {
		t.Fatalf("a.txt = %q err=%v", got, err)
	}
	if got, err := readTreeFile(e.env, tr, "sub/new.txt"); err != nil || got != "nested\n" {
		t.Fatalf("sub/new.txt = %q err=%v", got, err)
	}
}

// TestScrubDetectsCorruption 覆盖 §10.4：scrub 必须能发现被篡改的对象。
func TestScrubDetectsCorruption(t *testing.T) {
	e := newGCEnv(t, 100)
	e.seed("a.txt", "content to be corrupted\n")
	if _, err := e.r.Import(e.src); err != nil {
		t.Fatal(err)
	}
	// 干净时不应报损坏。
	clean, err := e.r.Scrub(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(clean.Corrupt) != 0 {
		t.Fatalf("干净仓库报了损坏: %v", clean.Corrupt)
	}
	if clean.Checked == 0 {
		t.Fatal("抽样数为 0")
	}

	// 篡改一个对象文件（对象按 objects/ab/cd/<hash> 分片）。
	objectsDir := filepath.Join(e.root, ".p37", "objects")
	var target string
	_ = filepath.Walk(objectsDir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && target == "" {
			target = p
		}
		return nil
	})
	if target == "" {
		t.Fatal("找不到对象文件")
	}
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 0xFF // 翻掉最后一个字节
	if err := os.WriteFile(target, b, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := e.r.Scrub(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Corrupt) == 0 {
		t.Fatal("★ scrub 没发现被篡改的对象 —— 校验形同虚设")
	}
}

var _ = workspace.OpenRequest{}
