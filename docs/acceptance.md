# 验收清单（v0.2 / format_ver=2）

自动化测试：`go test ./...`；并发回归：`go test -race ./internal/hub ./internal/repo ./internal/mcpsrv`。
P36 真机验收（多仓库）：

```bash
P37_ROOT=/srv/repos P37_REPO=/srv/repo P37_REPO_NAME=accept \
  bash packaging/p36/ci/acceptance.sh
```

脚本会创建 `/srv/repos`、初始化 legacy `/srv/repo`、创建 `accept` repo，
并验证 legacy 自动注册为 `default`。

| # | 场景 | 覆盖 |
|---|---|---|
| 1 | 多仓库创建/隔离/删除/重启恢复 | `internal/hub`、`internal/mcpsrv/multi_test.go` |
| 2 | 命名线创建、land、main 不变、log/compare/graph、reset/protect | `internal/repo/line_v2_test.go` |
| 3 | `ws_open(parent=line)` + `ws_land(target=line)` + 销毁重开 | `TestLineSessionLoopAndMerge`、`TestMultiRepoMCPLineLoop` |
| 4 | 子任务 `ws_land_into`（含删除/编辑冲突） | `TestLandIntoWorkspace`、`TestLandIntoDeleteVsEditConflict` |
| 5 | 会话 line 两回合 + 应用线合并 | `TestLineSessionLoopAndMerge` |
| 6 | `line_merge` 双亲版本/冲突解决/幂等 | `TestMergeConflictPreviewAndResolve`、`TestLineMergeIdempotent` |
| 7 | `version_tree/file/archive`、路径穿越、二进制 | `TestVersionReadAPIs` |
| 8 | Web 编辑 = 专用 workspace + line land | 同 3 |
| 9 | 目录远端 push/prune/restore、DAG 恢复 | `TestSyncPushRestoreDir` |
| 10 | 多 repo 并行 land | `TestMultiRepoLandParallel` |
| 11 | land 崩溃恢复（version/line head/integrating） | `internal/meta` reconcile + repo 测试 |
| 12 | MCP stdio 真实客户端 / EOF | `internal/mcpstdio`（含 `TestProxyRespondsBeforeEOF`） |
| 13 | copy/overlay、磁盘统计 | 既有 mount/workspace 测试、`ws_disk_usage` |
| 14 | P36 新 VM / 多仓库会话 / legacy default | `packaging/p36/acceptance` + `ci/acceptance.sh`（`P37_ROOT`） |
| 15 | 幂等/错误码/受保护 line/非祖先 reset | `idem_v2_test.go`、`line_v2_test.go`、MCP 测试 |
| 16 | 配额回收：landed 不占额度、Drop 清理失败仍删行、fanout 忽略 hidden/landed | `internal/repo/quota_recovery_test.go` |
| 17 | 子工作区冲突可发现/可重试/父 land 不死锁 + `ws_quota`/quota detail | `quota_recovery_test.go`、`internal/mcpsrv/quota_test.go` |

## 已知未做/限制

- 在线 GC 仍未实现；维护窗口 `p37 gc`。
- 多 line push 目前为全量 checkpoint；单 line 可增量。
- `line_reset` 的旧版本 7 天 grace 由 GC 的 reset 日志保留；尚无独立后台清理任务。
- `sync_restore` 不恢复 workspace/lease；P34 重新 `ws_open`。
- 同一 `remote_prefix` 只支持单写者；P34 需串行化同一 app 的 push。
