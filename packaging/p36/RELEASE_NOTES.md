# p37 @VERSION@ — 工作区配额与子会话整合修复

## 发布物

- 平台：`linux/amd64`，静态链接（`CGO_ENABLED=0`）
- 资产：
  - `p37-linux-amd64-@VERSION@.tar.gz`
  - `p37-linux-amd64-@VERSION@.tar.gz.sha256`
- tar 根目录：`p37`、`p37d`、`manifest.json`

## manifest

```json
{
  "version": "@VERSION@",
  "commit": "@COMMIT@",
  "buildTime": "@BUILD_TIME@",
  "goVersion": "@GO_VERSION@"
}
```

## 本版重点（v0.3.0）

P34「主管会话 + 子会话」在沙盒连续运行时暴露的工作区生命周期问题：

- **配额不再泄漏**：`max_active` 只统计 `active/conflicted/rebasing`；
  `drop_workspace=false` 的只读 landed、`integrating`、`hidden`、`broken`
  不再占用额度。
- **Drop 以元数据为准**：overlay `umount` 遇到 `EBUSY`（agent 的 exec 还留着
  进程/工作目录）时，工作区行照常删除、配额立即回收；目录清理失败降级为
  `ErrCleanupFailed` 警告（`ws_drop.dir_cleanup_warning` 或 land 结果的
  `warnings[]`）。
- **land 之后不再静默吞错**：`ws_land_into`、`line_merge`、`ws_reap` 的清理
  失败会记录日志并出现在返回体里。
- **冲突挂在子工作区上**：`ws_land_into` 冲突后 `conflict_list(ws_id=child)`
  直接可查；返回的 `preview_id` 等于子工作区 id；不再为每次冲突创建 hidden
  preview 工作区。
- **冲突重试真正可用**：conflict key 恒用子工作区 id，`conflict_resolve*`
  之后带 `preview_id` 重试会命中解决记录（此前每次重试都会换 key，冲突无法
  收敛）。
- **父工作区不再死锁**：直接 `ws_land_into` 冲突会把子工作区置为
  `conflicted`；`ws_land(fold=self)` 返回 `ws_has_active_children` 时，
  `detail.blocked_by_children` 列出阻塞项，调用方可解冲突或显式 `ws_drop`。
- **配额可见**：新增 `ws_quota`；`quota_exceeded.detail` 带
  `scope/used/limit`，供 P34 事前限流和退避。
- **fanout 修正**：隐藏 preview / 已落地子工作区不再占用 `max_fanout`。
- **启动兜底清理**：daemon 启动会清理无行引用的孤儿工作区目录和超过 24h 的
  隐藏 merge/preview 工作区；`ws_reap` 支持 `state=broken` 与
  `include_orphan_dirs=true`。
- **`resolution` 表随工作区删除一起清理**，避免长期增长。

## 配置变化

- `workspace.max_active` 默认从 **4** 调整为 **8**。4 正好等于「主管 + 3
  子会话」，没有任何缓冲；P34 建议按 `ws_quota.active_available` 动态限流。
- 新增 `workspace.max_hidden`（默认 32），隐藏 merge/preview 工作区上限。
- copy 模式每个工作区仍是完整树：20 GiB L2 上请结合 `repo_disk_usage` 与
  `warn_disk_bytes` 复核 8 是否合适，必要时收紧。

## 行为/接口变化（P34 对接）

- `ws_land_into` 冲突时返回 `state="conflict"`、`can_land=false`，且
  `preview_id == 子工作区 id`；子工作区状态为 `conflicted`。
- 重试序列：`conflict_list(ws_id=child)` →
  `conflict_resolve(_all)` → `ws_land_into(child, parent,
  expected_parent_tree=<最新 target_head>, idempotency_key=<同一个 key>)`。
- 不要再为每次重试新建子工作区；冲突 key 与子工作区绑定。
- `ws_drop(cascade=true)` 的语义仍是「子工作区重挂到祖父」，不是递归删除；
  子工作区不会因此释放配额。
- `quota_exceeded` 应视为平台容量问题（暂停 + 退避），不是模型空转。

## P36 集成要点

- 平台胶水放在 **L1**，不改 L0；只有 `p37`/`p37d` 在 payload。
- payload：ext4、label `P37PAYLOAD`、**固定 96 MiB**、只读挂载到
  `/run/p37-payload`。
- P36 设备顺序：`vda=root`、`vdb=P36AGENT`、`vdc=P37PAYLOAD`；按 label 挂载，
  不依赖顺序。
- v0.2 起默认多仓库：根目录 `/srv/repos`（`p36:p36`，`0750`），
  `p37d --root /srv/repos`；旧 `/srv/repo` 原地注册为 `default`，不移动目录。
- 默认 `workspace.materialize = "auto"`：Ubuntu 26.04 非 root 下 OverlayFS
  可能 `EPERM`，自动回退 copy；copy 模式默认 `max_active=8`、
  `max_hidden=32`、`warn_disk_bytes=8 GiB`。
- MCP 客户端使用 `/usr/local/bin/p37-mcp`（内部 `p37d --stdio --root /srv/repos`）。
- 新增 `/usr/local/bin/p37` CLI 稳定入口。
- `p37d.service` 以 `User=p36` 运行，授予 `CAP_SYS_ADMIN`；**不要**加会创建
  私有 mount namespace 的 `PrivateTmp`/`Protect*` 选项。

部署细节与验收步骤见仓库 `packaging/p36/README.md` 与 `VERIFY.md`。

## 已知限制

- 在线 GC 未实现；`p37 gc` 在 daemon 运行时拒绝，需维护窗口。
- copy 模式下 `p37 doctor` 仍会把 OverlayFS 标成 `FAIL`；正式门禁用
  `p37d --check --strict`（auto 下 overlay 只 WARN）。
- payload 换盘需要重启实例；服务级 restart 不保证换盘。
- `p37d --check` 有少量写探测，适合启动前检查，不建议作为在线高频探针。
- 配额检查是「先 count 再物化」的软准入；P34 并发 open 时建议由单仓
  dispatcher 串行化 open，或预留 1 个名额。
- `ws_land_into` 的幂等只保证同一 VM 生命周期（restore 不恢复 workspace）。
- 仓库格式 `format_ver=2`；从旧版本升级执行 `p37 migrate`。
