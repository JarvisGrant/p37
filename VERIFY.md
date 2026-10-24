# P37 真机验证清单

> 适用版本：**v0.4.0 / format_ver=2**。在目标机器（Ubuntu 26.04 + ext4）上
> 按顺序执行。**§1 是门禁：不通过就不要做后面的**。
>
> 本文不再引用未随仓库发布的内部规格编号；所有判据都写成可执行的命令和观察项。

## 0. 部署前置

```bash
# 0.1 文件系统确认（必须是 ext4；workspaces/ 与 .p37/ 必须同一文件系统）
df -T /srv/repos

# 0.2 inotify 限额（64 工作区 × 约 8k 目录 ≈ 51 万 watch）
sudo tee /etc/sysctl.d/90-p37.conf >/dev/null <<'EOF'
fs.inotify.max_user_watches = 1048576
fs.inotify.max_user_instances = 4096
EOF
sudo sysctl --system

# 0.3 构建
cd /path/to/p37 && go build -o /usr/local/bin/p37 ./cmd/p37 \
                      && go build -o /usr/local/bin/p37d ./cmd/p37d
```

P36 部署还需要 `/srv/repos` 存在、属主 `p36:p36`（建议 `0750`），以及可选的
systemd unit（`AmbientCapabilities=CAP_SYS_ADMIN`）。

## 1. 门禁 spike（必须先过）

在动 `mountmgr` / 工作区之前先验四件事。任一失败就要调整方案。

> P36 默认 `workspace.materialize = "auto"`。Ubuntu 26.04 非 root 下 overlay
> mount 可能返回 `EPERM`；此时自动回退 copy。`p37 doctor` 的
> `[FAIL] OverlayFS` 在 copy 模式下是**预期**，不要误判为部署失败；正式部署
> 门禁用 `p37d --check`（见 §6.2）。真实工作区模式看 `ws_open`/`ws_list` 的
> `materialize`，或验收脚本输出的 `MATERIALIZE=copy|overlay`。

```bash
p37 init /srv/repo
p37 doctor /srv/repo        # copy 模式下 OverlayFS FAIL 可接受，记录即可
```

| # | 验证项 | 通过判据 | 失败时的动作 |
|---|---|---|---|
| 1.1 | **能否挂载 OverlayFS** | `doctor` 输出 `[OK] OverlayFS 可用`，或 `materialize=auto` 下记录 `MATERIALIZE=copy` | 若报 `overlay_unavailable`：确认 `CAP_SYS_ADMIN`；仍失败则接受 `copy` 并重估 `max_active`/`warn_disk_bytes` |
| 1.2 | **换 lower 重挂** | `ws-rebase` 后工作区内容正确 | 记录失败细节，检查 `mountmgr.Relower` 的回落路径 |
| 1.3 | **stat 的 inode 是否可靠** | 连续两次采集之间，未改动的文件不触发哈希；且 daemon 重启后仍成立 | 若不成立：把 `InodeReliable` 置 false（配置走 `workspace` 相关开关） |
| 1.4 | **fsnotify 是否可靠** | 直接改文件后，采集能在 `scan_interval` 内发现；批量操作（`git checkout` 式）不丢事件 | 若不可靠：提高触发源 b/c 权重，并把 `scan_interval` 调小 |

具体复现步骤：

```bash
# 建一个工作区（overlay 模式）
mkdir -p /srv/src && echo 'l0
l1
l2' > /srv/src/a.txt
p37 import /srv/repo /srv/src
WS=$(p37 ws-open /srv/repo "spike" --materialize overlay | awk '/^ws_id/{print $2}')
echo "WS=$WS"

# 1.1 挂载是否真的是 overlay
findmnt -T /srv/repo/workspaces/$WS

# 1.3 inode 是否跨挂载稳定
stat -c '%i' /srv/repo/workspaces/$WS/a.txt
```

## 2. 功能验收

```bash
# 2.1 最小闭环
printf 'l0\nL1\nl2\n' > /srv/repo/workspaces/$WS/a.txt
printf 'brand new\n'   > /srv/repo/workspaces/$WS/new.txt
p37 ws-sync  /srv/repo $WS
p37 ws-diff  /srv/repo $WS      # 期望：2 个 op，可落地 true
p37 ws-land  /srv/repo $WS      # 期望：产出 1 个 version
p37 log /srv/repo

# 2.2 两个工作区并行改不同区域 → land 后零冲突；
#     两个工作区改同一行     → 得到结构化冲突。

# 2.3 崩溃恢复：在 land 提交 head 前 kill -9
p37 ws-sync /srv/repo $WS2
( p37 ws-land /srv/repo $WS2 & sleep 0.05; kill -9 %1 ) 2>/dev/null || true
p37d --repo /srv/repo &   # 重启应自动完成修复（ReconcileLand）
sleep 1; kill %1
p37 status /srv/repo      # 期望：head 要么是旧值要么是新值，无中间态；工作区状态一致

# 2.4 64 并发
#     在同一 version 上开 64 个工作区，断言 ws_open 全部 < 300ms，
#     且 workspaces/ 与 .p37/overlay/ 的磁盘增量符合预算（≈35GB 而非 640GB）。

# 2.5 preview 无副作用
#     记录 .p37/p37.db 的行数指纹，调用 land_preview，指纹不变。
```

## 3. 性能基线

| 操作 | 目标 | 测量方式 |
|---|---|---|
| `init` + `import`（1 万文件） | < 30 s | `time p37 import` |
| 基底物化（10 万文件，首次） | < 60 s | 第一个 `ws-open` 的耗时 |
| `ws_open`（基底已就绪，overlay） | **< 300 ms** | `time p37 ws-open` |
| `capture` 无变更（1 万文件） | < 100 ms | `time p37 ws-sync` |
| `capture` 改 10 个文件 | < 300 ms | 同上 |
| 单工作区全量 scan（10 万文件） | < 3 s | 一轮 `ws-sync` |
| `land` 单层 | < 1 s | `time p37 ws-land` |
| daemon 常驻内存 | < 1 GiB | `ps -o rss` |

## 4. 需要回报的内容

1. `p37 doctor` 的完整输出；
2. §1 四项的结论（尤其 1.2 / 1.3 / 1.4）；
3. §2 各项是否通过；失败项的完整错误信息；
4. §3 的实测数字与目标值的差距；
5. `p37 fsck /srv/repo` 的输出；
6. `uname -a`、`df -T`、`findmnt -T` 相关行；
7. `p37d --check --strict --repo /srv/repo --json` 或
   `p37d --check --strict --root /srv/repos --json` 全文。

## 5. 已知的「到真机才能定」的点

| 点 | 影响 |
|---|---|
| OverlayFS 上的 **inode 稳定性** | 决定 `stat_cache` 是否能用快路径 |
| OverlayFS 上的 **fsnotify 可靠性** | 决定采集三重触发的权重分配 |
| **换 lower 重挂**的实际行为 | 决定 `ws_rebase` 的增量重挂是否需要改 |
| `CAP_SYS_ADMIN` 下的挂载语义 | 决定 P36 是否只能走 copy |
| 真实代码库上的 **place 命中率** | 决定冲突率是否可接受 |

## 6. P36 部署验收

前置：

- L0 不改；平台胶水在 L1；`p37`/`p37d` 只来自 payload；
- L2 的 `/srv/repos` 为 ext4、属主 `p36:p36`（建议 `0750`）；
- 宿主已投递 label `P37PAYLOAD` 的只读 ext4 盘，固定 **96 MiB**；
- P36 设备顺序：`vda=root`、`vdb=P36AGENT`、`vdc=P37PAYLOAD`；
- `p37d.service` 使用 `User=p36` + `AmbientCapabilities=CAP_SYS_ADMIN`，
  **不要**加会创建私有 mount namespace 的 `PrivateTmp`/`Protect*`；
- `workspace.materialize = "auto"`；非 root overlay 失败时接受 copy 回退；
- copy 模式建议按 L2 容量设置 `max_active` / `warn_disk_bytes`；
- `/run/p37-payload` 由 `/etc/tmpfiles.d/p37.conf` 创建；不要用
  `RuntimeDirectory=`（service 重启时会在只读 payload 挂载点上重建目录而失败）。
  停服后 payload 仍挂载。

GitHub Actions 的 `release.yml` 会自动跑 §6.1–§6.6；也可以在已发布 Release 上
手动触发 `p36-acceptance.yml`。

```bash
# 6.1 服务外 findmnt：payload 与 keeper workspace
findmnt -T /run/p37-payload                  # 应为 ext4
find /run/p37-payload -maxdepth 1 -mindepth 1 -printf '%f\n' | sort
findmnt -T /srv/repos/workspaces/<keeper_ws> # overlay 或 copy 下的 /srv/repos ext4
# 记录 MATERIALIZE=copy|overlay

# 6.2 启动前检查（daemon 停止时；--check 有写探测）
systemctl stop p37d

# 单仓库 / legacy：完整健康检查（ext4/写探测/OverlayFS/inotify/磁盘）
/run/p37-payload/p37d --check --strict --repo /srv/repo
/run/p37-payload/p37d --check --strict --json --repo /srv/repo | jq .

# 多仓库：registry 级检查（每个 repo 的 DB，strict 下走 line 链）
/run/p37-payload/p37d --check --strict --json --root /srv/repos | jq .

# auto 模式下 doctor 的 OverlayFS FAIL 是预期；正式门禁看上面的 check
/run/p37-payload/p37 doctor /srv/repo || true
systemctl start p37d

# 6.3 幂等 init（第二次必须返回 0 且不改 config）
/run/p37-payload/p37 init --if-missing --config /usr/local/share/p37/config.toml /srv/repo
sha256sum /srv/repo/.p37/config.toml
/run/p37-payload/p37 init --if-missing /srv/repo
sha256sum /srv/repo/.p37/config.toml

# 6.4 两个 agent 经 stdio 并发（MCP 客户端 command=/usr/local/bin/p37-mcp）
#     各自 ws_open → 改文件 → ws_sync → ws_land，断言两个 ws_id 不同、
#     两次 land 都成功、version_log 至少 3 个版本。
#     另外开一个 keeper 工作区不落地，用于 6.1/6.5 的 findmnt。
#     读取 keeper 的 materialize，输出 MATERIALIZE=copy|overlay。

# 6.5 重启恢复
systemctl restart p37d
findmnt -T /srv/repos/workspaces/<keeper_ws>  # overlay 或 /srv/repos ext4
/run/p37-payload/p37 --root /srv/repos --repo <name> ws-list

# 6.6 GC 保护（停服后 payload 仍挂载）
systemctl start p37d
/run/p37-payload/p37 --root /srv/repos --repo <name> gc --prune; echo $?   # 期望非 0
systemctl stop p37d
/run/p37-payload/p37 --root /srv/repos --repo <name> gc --prune --pack     # 期望 0
```

v0.1.13 起默认验收**不再自动 root fallback**：P36 在 `materialize=auto` 下
走 copy，日志输出 `MATERIALIZE=copy`。只有显式 `P37_ALLOW_ROOT_FALLBACK=1`
才退化 root（用于对照/调试）；它不能替代真实 P36 的 `User=p36` 验收。

root + overlay 是可选变体：`config-overlay.toml` +
`p37d-root-overlay.service`；仅当 P36 明确允许 root 时使用。

## 7. v0.3.x 补充验收

### 7.1 应用卷热插拔

```bash
# attach
mount /dev/vdX /srv/repos/app-a
p37 --root /srv/repos repo register app-a /srv/repos/app-a --json
p37 --root /srv/repos repo list --json | jq '.[] | select(.name=="app-a")'

# 运行中创建 repo / 落地一个版本
p37 --root /srv/repos --repo app-a status

# detach：先注销，再 umount
p37 --root /srv/repos repo unregister app-a --json
test -f /srv/repos/app-a/.p37/p37.db && echo "data kept"
umount /srv/repos/app-a

# 重新挂载后恢复原 id/name
mount /dev/vdX /srv/repos/app-a
p37 --root /srv/repos repo register app-a /srv/repos/app-a --json

# 挂载点不能被 repo_delete
p37 --root /srv/repos repo delete app-a --force   # 期望 repo_is_mountpoint
```

### 7.2 异步导出与目录护栏

```text
repo_export(ref, dest_dir, async=true) → job_id
repo_export_status(job_id)             → running/done/failed
```

- 大仓库用 `async=true`，避免 Cloudflare 100s / MCP 超时；
- `dest_dir` 必须是绝对路径、仓库外、且在 `export_root` 下；
- 非空目录返回 `destination_not_empty`；
- 导出目录由调用方清理。

### 7.3 配额与子工作区

```text
ws_quota → active_available 应随 ws_drop 立即回收
ws_land_into 冲突 → child 状态变为 conflicted，
                     conflict_list(ws_id=child) 可发现，
                     解决后带 preview_id + expected_parent_tree 重试成功
父 ws_land(fold=self) → ws_has_active_children，
                        detail.blocked_by_children 列出阻塞项
```

验收记录请附：`p37d --check --json` 全文、`MATERIALIZE=copy|overlay`、
`findmnt`、两个 stdio 客户端的 `ws_id` 与 `version_log`、GC 拒绝的退出码与
stderr、`manifest.json` 与发布 tar 的 sha256，以及 `ROOT_FALLBACK`/`NNP_FAILED`
（默认都应为 0）。
