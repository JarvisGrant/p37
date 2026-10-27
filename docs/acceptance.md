# 验收清单（v0.4.4 / format_ver=2）

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
| 4a | **父改动不被子会话回退 / 有效树缓存 / 只写子改动路径** | `TestLandIntoDoesNotRevertParentEdit`、`TestEffectiveTreeCacheInvalidatedOnSameOpCount`、`TestLandIntoPreservesParentOnlyChanges` |
| 4b | 同文件不同位置自动合并；同一行冲突；strict 策略 | `TestLandIntoDisjointHunksAutoMerge`、`TestLandIntoSameLineConflict`、`TestLandIntoStrictMergeConflictsOnBothChanged` |
| 4c | 越界路径、expected_parent_tree、幂等重放 | `TestLandIntoAllowedPaths`、`TestLandIntoExpectedParentTreeMoved`、`TestLandIntoIdempotentReplay` |
| 4d | 写前逐路径校验 / MCP 字段与错误码 | `TestVerifyBeforeWriteDetectsConcurrentEdit`、`TestLandIntoAPIRevisionFields`、`TestLandIntoAPIParentMoved` |
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

## 工作区协议验收（v0.4.1 起）

```bash
go test ./internal/mcpsrv -run 'TestWS(EnsureConcurrent|OpenIdempotencyConcurrent|GoneLandReplay|GoneAbandonedAndTombstoneList|LandDurability|PersistenceReopen|QuotaAndReapObservability|ErrorContext)' -count=1 -v
P37_BIN=./p37 bash packaging/p36/ci/ws-contract.sh
```

脚本用 5 个独立进程并发 `ws-ensure`，断言只产生 1 个工作区、5 个响应
`ws_id` 相同；再并发 ensure 不变；drop 后 tombstone 可查。

## v0.4.4 半初始化 + 幂等验收

```bash
go test ./internal/repo -run 'TestEnsureHalf|TestEnsureImportHalf|TestImportV0|TestImportFault|TestInitIdempotency|TestDefaultLineLandUpdatesRefs' -count=1 -v
go test ./internal/hub -run 'TestInitIdempotency' -count=1 -v
go test ./internal/mcpsrv -run 'TestRepoInitUserFile|TestRepoHealthUserFile|TestRepoInitIdempotency|TestRepoRegisterInitIfMissingUserFile' -count=1 -v
```

必须覆盖：

1. 半 DB + v0 空 + user.txt + `mode=empty/repair` → `repo_not_empty`，
   文件不变；`mode=import` → 导入成功，v0 包含 user.txt。
2. 半 DB + v0 非空 + worktree 与 v0 相同 + `mode=import` → 只 repair，
   不新建 v0；不同 + force=false → `repo_not_empty` +
   `next_action=repo_init_import_force`；force=true → 替换 v0，version_count=1。
3. 半 DB + v0 非空 + worktree 空 + force=false → 不自动覆盖；force=true 才
   替换为空树。
4. 有历史版本/命名 line → `repo_history_not_empty`；有活跃 workspace/lease →
   `repo_busy`；两者 force 都无效。
5. `repo_register(init_if_missing)` 对上述含用户文件的目录返回
   `repo_not_empty`，不静默 repair。
6. 同 key 同请求幂等；同 key 改 mode/name/default_line/force/path →
   `idempotency_conflict`；并发同 key → `idempotency_in_progress` 或幂等；
   重启 p37d 后仍然有效；root 索引删除后同 path 仍幂等，跨 path 降级 warning。
7. `P37_IMPORT_FAULT_AT=swap_after_delete|swap_after_insert|swap_before_commit`
   → 旧 v0/head/line/refs/version_count 不变；`after_commit_before_refs` →
   DB head 新、refs 旧、`refs_match=false`，`repo_init(mode=repair)` 对齐 refs。
8. default line 的 `ws_land` / `line_merge` / `line_reset` / `version_revert` /
   `sync fetch` 后 `repo_health.refs_match=true`。

## v0.4.3 仓库初始化验收

```bash
go test ./internal/repo -run 'TestEnsure|TestStrict|TestImportFailure' -count=1 -v
go test ./internal/mcpsrv -run 'TestRepoInit|TestRepoHealthUninitialized|TestRepoRegisterEmpty|TestRepoRegisterInitIfMissing|TestRepoImportUninitialized|TestRepoScanRepair' -count=1 -v
go test ./cmd/p37 -run 'TestInit|TestInitImportAndRepairFlags|TestInitAcceptsPrecreatedEmptyControlDir' -count=1 -v
go test ./internal/hub -run 'TestRegisterPath|TestRebind|TestScan' -count=1 -v
```

真机/沙盒必须额外覆盖：

1. 干净 ext4 持久目录 + `p37 init --if-missing`：`version_count=1`、
   `line_list` 有 main、`mainline_head` 非零、`.p37` 在 `/srv/repos/p34-<id>` 内、
   `lost+found` 不影响 empty 判定。
2. 在 Init 的建库后/写 v0 后/写 refs 后/SetMainlineHead 前/rename 前/父 fsync 前
   注入失败：重跑 `p37 init --if-missing` 必须 repair 成功，不出现第二个 v0。
3. 用 `bad.db`（repo 行 + 一个 root v0，无 line/head/refs）做 fixture：
   repair 认领该 v0，不新建根。
4. 半成品 + `--strict`/`--no-repair`：返回 `repo_not_initialized`/
   `repo_db_corrupt`，DB 不变。
5. 两个并发 `repo_init`/`p37 init`：一个创建，一个幂等返回；DB 无重复 v0/line。
6. `repo_import` 失败注入：旧 v0/head 仍可用，不出现 head 指向已删除版本。
7. `repo_health {path, with_quota:false}` 对半成品返回 200 +
   `initialized=false` + `next_action`，不写库、不 migrate。
8. P36 attach 初始化失败：卷保持挂载，409 + `mounted=true` +
   `initialized=false`，可重试 repair；不 umount、不删 `.p37`。
9. VM stop/start 同 volumeId 重挂：`repo_info`/`line_list` 仍在，
   `version_count` 不变。
10. `repo_register(rebind=true)` 有活跃 workspace/lease：`repo_busy`，
    registry 不变。

## v0.4.2 land_into 一致性验收

```bash
go test ./internal/repo -run 'TestLandInto|TestEffectiveTreeCache' -count=1 -v
go test ./internal/workspace -run 'TestVerifyBeforeWrite|TestApplyDelta' -count=1 -v
go test ./internal/mcpsrv -run 'TestLandIntoAPI' -count=1 -v
```

真机场景建议按 `docs/p34-integration.md` §5 的调用序列跑一遍，并额外断言：

- 父写 `A=v1` → 派子只改 `B` → 父改 `A=v2` → land 后 `A` 仍是 `v2`；
- 父子同文件不同位置 → `both_changed_paths` + 明确结果；
- 父子同一行 → `state=conflict`，父目录哈希不变；
- 子越界改 `C` → `land_preview.out_of_scope_paths` 含 `C`，land 返回
  `path_not_allowed`；
- 重复 landing 同一 `idempotency_key` → `replayed=true`；
- preview 后父再改 → 带旧 `expected_parent_tree` 的 land 返回
  `target_moved`，父目录不变。

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
