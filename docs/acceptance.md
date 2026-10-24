# 验收清单（v0.4.0 / format_ver=2）

## 自动化

```bash
go test ./... -count=1
go vet ./...
make cross
```

并发回归建议在发布前单独跑：

```bash
go test -race ./internal/hub ./internal/repo ./internal/mcpsrv
```

CI 使用 `P37_FORCE_HASH=1` 关闭 stat 快路径（GitHub runner 的 mtime 精度不足），
并跑 `gofmt`、`vet`、`go test ./... -count=1`、`make cross`。

## P36 真机验收（多仓库）

```bash
P37_ROOT=/srv/repos P37_REPO=/srv/repo P37_REPO_NAME=accept \
  bash packaging/p36/ci/acceptance.sh
```

脚本会创建 `/srv/repos`、初始化 legacy `/srv/repo`、创建 `accept` repo，
验证 legacy 自动注册为 `default`，并跑两个 stdio 客户端、重启重挂、GC 拒绝。
它主要覆盖**部署形态**；v0.3.x 新增的能力由下表的单元/集成测试覆盖。

## 验收矩阵

| # | 场景 | 覆盖 |
|---|---|---|
| 1 | 多仓库创建/隔离/删除/重启恢复 | `internal/hub`（`TestMultiRepoLifecycle`、`TestMultiRepoRestart`）、`internal/mcpsrv/multi_test.go` |
| 2 | 命名线创建、land、main 不变、log/compare/graph、reset/protect | `internal/repo/line_v2_test.go`（`TestLineSessionLoopAndMerge`、`TestResetAndProtect`、`TestLineCompareAndGraph`） |
| 3 | `ws_open(parent=line)` + `ws_land(target=line)` + 销毁重开 | `TestLineSessionLoopAndMerge`、`TestMultiRepoMCPLineLoop` |
| 4 | 子任务 `ws_land_into`（含删除/编辑冲突） | `TestLandIntoWorkspace`、`TestLandIntoDeleteVsEditConflict` |
| 5 | 会话 line 两回合 + 应用线合并 | `TestLineSessionLoopAndMerge` |
| 6 | `line_merge` 双亲版本/冲突解决/幂等 | `TestMergeConflictPreviewAndResolve`、`TestLineMergeIdempotent` |
| 7 | `version_tree/file/archive`、路径穿越、二进制 | `TestVersionReadAPIs`、`TestExportTree` |
| 8 | Web 编辑 = 专用 workspace + line land | 同 3 |
| 9 | 目录远端 push/prune/restore、DAG 恢复 | `TestSyncPushRestoreDir` |
| 10 | 多 repo 并行 land | `TestMultiRepoLandParallel`、`TestConcurrentOpenSameRepo` |
| 11 | land 崩溃恢复（version/line head/integrating） | `internal/meta` reconcile + repo 测试 |
| 12 | MCP stdio 真实客户端 / EOF | `internal/mcpstdio`（含 `TestProxyRespondsBeforeEOF`） |
| 13 | copy/overlay、磁盘统计 | 既有 mount/workspace 测试、`ws_disk_usage` |
| 14 | P36 新 VM / 多仓库会话 / legacy default | `packaging/p36/acceptance` + `ci/acceptance.sh`（`P37_ROOT`） |
| 15 | 幂等/错误码/受保护 line/非祖先 reset | `idem_v2_test.go`、`line_v2_test.go`、MCP 测试 |
| 16 | 配额回收：landed 不占额度、Drop 清理失败仍删行、fanout 忽略 hidden/landed | `internal/repo/quota_recovery_test.go` |
| 17 | 子工作区冲突可发现/可重试/父 land 不死锁 + `ws_quota`/quota detail | `quota_recovery_test.go`、`internal/mcpsrv/quota_test.go` |
| 18 | `repo_register` 路径安全、init_if_missing、repo.json 身份、幂等 | `internal/hub/lifecycle_test.go`、`internal/mcpsrv/repo_lifecycle_test.go` |
| 19 | `repo_unregister` busy/force、tombstone、重启不复活、数据保留 | `TestUnregisterKeepsDataAndRestores`、`TestUnregisterBusyThenForce`、`TestUnregisterSurvivesRestart`、`TestUnregisterForceUnmountsOverlay` |
| 20 | `repo_scan` 空/非空 registry、dry-run、prune_missing、跳过隐藏/symlink | `TestScanNonEmptyRegistryAndPrune`、`TestScanDryRunAndTombstone`、`TestScanSkipsHiddenAndSymlink` |
| 21 | `repo_delete` 挂载点/root 自身/root 外路径保护 | `TestDeleteMountpointGuard`、`TestDeleteRejectsOutsideAndRoot`、`TestRepoDeleteMountpointMCP` |
| 22 | `repo_export` 同步/异步、流式写入、export_root、目录非空、大文件 | `internal/repo/export_test.go`、`internal/mcpsrv/export_test.go` |

## v0.4.0 工作区协议验收

```bash
go test ./internal/mcpsrv -run 'TestWS(EnsureConcurrent|OpenIdempotencyConcurrent|GoneLandReplay|GoneAbandonedAndTombstoneList|LandDurability|PersistenceReopen|QuotaAndReapObservability|ErrorContext)' -count=1 -v
P37_BIN=./p37 bash packaging/p36/ci/ws-contract.sh
```

脚本用 5 个独立进程并发 `ws-ensure`，断言只产生 1 个工作区、5 个响应
`ws_id` 相同；再并发 ensure 不变；drop 后 tombstone 可查。

## v0.3.x 真机补充（建议在 P36 上手工走一遍）

自动化测试覆盖了逻辑，但下面这些依赖真实挂载/卷，建议每次发布至少验一次：

```bash
# 1. 应用卷 attach
mount /dev/vdX /srv/repos/app-a
p37 --root /srv/repos repo register app-a /srv/repos/app-a --json
p37 --root /srv/repos repo list --json | jq '.[] | select(.name=="app-a")'

# 2. 运行中创建 repo 并落地一个版本
P37_ROOT=/srv/repos p37 --root /srv/repos --repo app-a status

# 3. 注销后数据保留，重新挂载恢复原 id
p37 --root /srv/repos repo unregister app-a --json
test -f /srv/repos/app-a/.p37/p37.db && echo "data kept"
p37 --root /srv/repos repo register app-a /srv/repos/app-a --json

# 4. 挂载点不能被 repo_delete
p37 --root /srv/repos repo delete app-a --force   # 期望 repo_is_mountpoint

# 5. 部署导出
p37 --root /srv/repos --repo app-a status
# 通过 MCP repo_export 导出到 /srv/repos/.p37/tmp/p34-deploy/<jobId>
# 非空目录应返回 destination_not_empty

# 6. 重启 daemon 后 registry 与工作区挂载恢复
systemctl restart p37d
p37 --root /srv/repos repo list --json
p37 --root /srv/repos --repo app-a ws-list
```

## 已知未做 / 限制

- 在线 GC 未实现；维护窗口 `p37 gc`。
- 写时打包与 repack 压实未实现；只有离线 `p37 gc --prune --pack`。
- 多 line push 目前为全量 checkpoint；单 line 可增量。
- `line_reset` 的旧版本 7 天 grace 由 GC 的 reset 日志保留；尚无独立后台清理任务。
- `sync_restore` 不恢复 workspace/lease；P34 重新 `ws_open`。
- 同一 `remote_prefix` 只支持单写者；P34 需串行化同一 app 的 push。
- `p37d --check --root` 只做 registry/DB/line 链检查；完整健康检查用
  `--check --repo <root>`。
- `repo_unregister` 不等待已经进入执行中的 MCP 调用结束；detach 前由上层确认
  应用空闲。
- `repo_delete` 的 `delete_remote` 参数当前保留但未删除远端。
