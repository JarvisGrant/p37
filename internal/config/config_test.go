package config

import (
	"testing"
	"time"

	"github.com/chenyiwen/p37/internal/workspace"
)

// TestRepoOptionsWiresEveryKnob 是「配置写了不生效」的回归测试。
//
// 背景：此前 retention.*、gc.*、workspace.max_*/warn_disk_bytes、
// merge.max_chain_length、workspace.stable_* 等一大批配置项**声明了但从未被
// 读取**，改了没有任何反应。这个测试逐项钉住映射关系：任何一项漏接都会失败。
func TestRepoOptionsWiresEveryKnob(t *testing.T) {
	c := Default()
	// 全部改成非默认值，这样任何「漏接」都会暴露成默认值。
	c.Workspace.MaxActive = 7
	c.Workspace.MaxDepth = 9
	c.Workspace.MaxFanout = 3
	c.Workspace.StableRetries = 11
	c.Workspace.StableDelay = "123ms"
	c.Workspace.BaseCacheBytes = 456
	c.Workspace.WarnDiskBytes = 789
	c.Workspace.InotifyFallback = true
	c.Merge.ContextLines = 5
	c.Merge.MaxMatches = 42
	c.Merge.MaxChainLength = 17
	c.Merge.MoveSimilarity = 0.75
	c.Retention.EpochSize = 13
	c.Retention.KeepVersions = 21
	c.Retention.KeepEpochs = 4
	c.GC.ScrubSample = 0.5
	c.GC.VerifyOnRead = "always"
	c.Daemon.LandQueueMax = 99
	c.Daemon.LogLevel = "debug"
	c.Files.MaxFileSize = 111
	c.Files.MaxTextSize = 222
	c.Files.BinaryProbeBytes = 333

	o := c.RepoOptions()

	if o.MaxActive != 7 || o.MaxDepth != 9 || o.MaxFanout != 3 {
		t.Errorf("工作区上限未接线: %d/%d/%d", o.MaxActive, o.MaxDepth, o.MaxFanout)
	}
	if o.MaxMatches != 42 {
		t.Errorf("merge.max_matches 未接线: %d", o.MaxMatches)
	}
	if o.EpochSize != 13 {
		t.Errorf("retention.epoch_size 未接线: %d", o.EpochSize)
	}
	if o.LandQueueMax != 99 {
		t.Errorf("daemon.land_queue_max 未接线: %d", o.LandQueueMax)
	}
	if o.BaseCacheBytes != 456 {
		t.Errorf("workspace.base_cache_bytes 未接线: %d", o.BaseCacheBytes)
	}
	if o.GC.KeepVersions != 21 || o.GC.KeepEpochs != 4 {
		t.Errorf("retention.keep_* 未接线: %+v", o.GC)
	}
	if o.GC.ScrubSample != 0.5 || o.GC.VerifyOnRead != "always" {
		t.Errorf("gc.* 未接线: %+v", o.GC)
	}

	cp := o.Capture
	if cp.ContextLines != 5 || cp.MaxChain != 17 {
		t.Errorf("merge.* 未接线: %+v", cp)
	}
	if cp.MoveSimilarity != 0.75 {
		t.Errorf("merge.move_similarity 未接线: %v", cp.MoveSimilarity)
	}
	if cp.StableRetries != 11 || cp.StableDelay != 123*time.Millisecond {
		t.Errorf("workspace.stable_* 未接线: retries=%d delay=%s", cp.StableRetries, cp.StableDelay)
	}
	if cp.MaxFileSize != 111 || cp.MaxTextSize != 222 || cp.ProbeBytes != 333 {
		t.Errorf("files.* 未接线: %+v", cp)
	}

	if c.WarnDiskBytes() != 789 {
		t.Errorf("workspace.warn_disk_bytes 未接线: %d", c.WarnDiskBytes())
	}
	if !c.InotifyFallback() {
		t.Error("workspace.inotify_fallback 未接线")
	}
	if c.LogLevel() != "debug" {
		t.Errorf("daemon.log_level 未接线: %q", c.LogLevel())
	}
}

// TestDefaultConfigIsSane 钉住默认值（附录 B）。
func TestDefaultConfigIsSane(t *testing.T) {
	c := Default()
	if c.Workspace.MaxActive != workspace.HardMaxActive {
		t.Errorf("默认 max_active 应等于硬上限 %d，得到 %d",
			workspace.HardMaxActive, c.Workspace.MaxActive)
	}
	if got := c.WarnDiskBytes(); got != 50<<30 {
		t.Errorf("默认 warn_disk_bytes = %d", got)
	}
	if c.LogLevel() != "info" {
		t.Errorf("默认 log_level = %q", c.LogLevel())
	}
	// 空配置也要给出可用值（不能因为用户没写就变成 0）。
	var empty Config
	if empty.WarnDiskBytes() <= 0 {
		t.Error("空配置的 warn_disk_bytes 必须是正的默认值")
	}
	if empty.LogLevel() == "" {
		t.Error("空配置的 log_level 必须有默认值")
	}
}

// TestRepoOptionsExportsStableDelayForWorkspaces 断言工作区采集能拿到稳定点参数。
func TestRepoOptionsExportsStableDelayForWorkspaces(t *testing.T) {
	c := Default()
	o := c.RepoOptions()
	if o.Capture.StableRetries <= 0 || o.Capture.StableDelay <= 0 {
		t.Fatalf("默认配置必须开启稳定点判定: retries=%d delay=%s",
			o.Capture.StableRetries, o.Capture.StableDelay)
	}
}
