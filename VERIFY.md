# P37 真机验证清单

> 在目标机器（Ubuntu + ext4）上按顺序执行。**§1 是门禁：不通过就不要做后面的**。
> 对应规格 §16 的「M3 门禁」与附录 L 的验收项。

## 0. 部署前置

```bash
# 0.1 文件系统确认（必须是 ext4；workspaces/ 与 .p37/ 必须同一文件系统）
df -T /srv/repo

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

可选：按附录 G.2 方案 A 配置 systemd unit（`AmbientCapabilities=CAP_SYS_ADMIN`）。

## 1. 门禁 spike（必须先过）

规格 §16 要求动 `mountmgr` 之前先验四件事。任一失败就要调整方案。

```bash
p37 init /srv/repo
p37 doctor /srv/repo
```

| # | 验证项 | 通过判据 | 失败时的动作 |
|---|---|---|---|
| 1.1 | **能否挂载 OverlayFS** | `doctor` 输出 `[OK] OverlayFS 可用` | 若报 `OVERLAY_UNAVAILABLE`：按附录 G.2 给 `CAP_SYS_ADMIN`；仍失败则接受 `copy` 模式并重估并发上限 |
| 1.2 | **换 lower 重挂**（§4.9.2 步骤 5b） | `ws-rebase` 后工作区内容正确 | 记录失败细节，需调整 §4.9.2 的实现（见 §3） |
| 1.3 | **stat 的 inode 是否可靠** | 连续两次采集之间，未改动的文件不触发哈希；且 daemon 重启后仍成立 | 若不成立：按 §4.7 的降级开关把 `InodeReliable` 置 false（配置 `workspace` 下加开关） |
| 1.4 | **fsnotify 是否可靠** | 直接改文件后，采集能在 `scan_interval` 内发现；批量操作（`git checkout` 式）不丢事件 | 若不可靠：提高触发源 b/c 权重，并把 `scan_interval` 调小 |

具体的 1.2–1.4 复现步骤：

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

## 2. 功能验收（附录 L.3 / §14.3）

```bash
# 2.1 最小闭环（§14.3.1）
printf 'l0\nL1\nl2\n' > /srv/repo/workspaces/$WS/a.txt
printf 'brand new\n'   > /srv/repo/workspaces/$WS/new.txt
p37 ws-sync  /srv/repo $WS
p37 ws-diff  /srv/repo $WS      # 期望：2 个 op，可落地 true
p37 ws-land  /srv/repo $WS      # 期望：产出 1 个 version
p37 log /srv/repo

# 2.2 两个工作区并行改不同区域 → land 后零冲突（§14.3.1）
#     两个工作区改同一行   → 得到结构化冲突（§14.3.2）

# 2.3 崩溃恢复（§14.3.5）：在 land 提交 head 前 kill -9
p37 ws-sync /srv/repo $WS2
( p37 ws-land /srv/repo $WS2 & sleep 0.05; kill -9 %1 ) 2>/dev/null || true
p37d --repo /srv/repo &   # 重启应自动完成修复（ReconcileLand）
sleep 1; kill %1
p37 status /srv/repo      # 期望：head 要么是旧值要么是新值，无中间态；工作区状态一致

# 2.4 64 并发（§14.3.6）
#     在同一 version 上开 64 个工作区，断言 ws_open 全部 < 300ms，
#     且 workspaces/ 与 .p37/overlay/ 的磁盘增量符合 §4.8 预算（≈35GB 而非 640GB）

# 2.5 preview 无副作用（§14.3.7）
#     记录 .p37/p37.db 的行数指纹，调用 land_preview，指纹不变
```

## 3. 性能基线（附录 D.2）

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

跑完后请把这些贴回来，我据此做针对性修正：

1. `p37 doctor` 的完整输出
2. §1 四项的结论（尤其 1.2 / 1.3 / 1.4）
3. §2 各项是否通过；失败项的完整错误信息
4. §3 的实测数字与目标值的差距
5. `p37 fsck /srv/repo` 的输出
6. `uname -a`、`df -T`、`findmnt -T` 相关行

## 5. 已知的「到真机才能定」的点

这些在开发机上无法验证，结论只能从真机来：

| 点 | 影响 | 相关规格 |
|---|---|---|
| OverlayFS 上的 **inode 稳定性** | 决定 `stat_cache` 是否能用快路径 | §4.7 |
| OverlayFS 上的 **fsnotify 可靠性** | 决定采集三重触发的权重分配 | §5.2 |
| **换 lower 重挂**的实际行为 | 决定 `ws_rebase`（§4.9.2）的实现是否需要改 | §4.9.2 |
| `CAP_SYS_ADMIN` 下的挂载语义 | 决定附录 G.2 采用方案 A 还是 B | 附录 G.2 |
| 真实代码库上的 **place 命中率** | M2 出口指标（≥95%） | §16 |

## 6. P36 部署验收（本轮新增）

前置：

- L0 不改；平台胶水在 L1；`p37`/`p37d` 只来自 payload；
- L2 的 `/srv/repo` 为 ext4、属主 `p36:p36`（建议 `0750`）；
- 宿主已投递 label `P37PAYLOAD` 的只读 ext4 盘，固定 **96 MiB**；
- P36 设备顺序：`vda=root`、`vdb=P36AGENT`、`vdc=P37PAYLOAD`；
- `p37d.service` 使用 `User=p36` + `AmbientCapabilities=CAP_SYS_ADMIN`，
  **不要**加会创建私有 mount namespace 的 `PrivateTmp`/`Protect*`；
- `/run/p37-payload` 由 `/etc/tmpfiles.d/p37.conf` 创建；不要用
  `RuntimeDirectory=`（service 重启时会在只读 payload 挂载点上重建目录而失败）。
  停服后 payload 仍挂载。

GitHub Actions 的 `release.yml` 会自动跑第 6.1–6.6；也可以在已发布 Release 上
手动触发 `p36-acceptance.yml`。

```bash
# 6.1 服务外 findmnt：payload 与 keeper workspace
#     ws_land 会删除工作区，所以需要一个不落地的工作区做挂载检查。
#     payload 根目录应严格只有 p37/p37d/manifest.json；如果 mkfs.ext4 -d
#     生成了 lost+found，用 debugfs -w -R "rmdir /lost+found" 删除。
findmnt -T /run/p37-payload                  # 应为 ext4
find /run/p37-payload -maxdepth 1 -mindepth 1 -printf '%f\n' | sort
findmnt -T /srv/repo/workspaces/<keeper_ws>  # 应为 overlay

# 6.2 启动前检查（daemon 停止时；--check 有写探测）
systemctl stop p37d
/run/p37-payload/p37d --check --strict --repo /srv/repo
/run/p37-payload/p37d --check --strict --json --repo /srv/repo | jq .
/run/p37-payload/p37 doctor /srv/repo
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

# 6.5 重启恢复
systemctl restart p37d
findmnt -T /srv/repo/workspaces/<keeper_ws>  # 应为 overlay
/run/p37-payload/p37 ws-list /srv/repo

# 6.6 GC 保护（停服后 payload 仍挂载）
systemctl start p37d
/run/p37-payload/p37 gc /srv/repo --prune; echo $?   # 期望非 0
systemctl stop p37d
/run/p37-payload/p37 gc /srv/repo --prune --pack     # 期望 0
```

如果是在 GitHub-hosted runner 上跑，遇到 OverlayFS `permission denied` 时脚本会
退化 `User=root` 并在日志里写 `ROOT_FALLBACK=1`；这只是 CI 环境限制，不能替代
真实 P36 的 `User=p36` 验收。真实验收请用 P36 实例或 self-hosted runner
（`vars.P37_RUNNER`）。

验收记录请附：`p37d --check --json` 全文、`findmnt`、两个 stdio 客户端的
`ws_id` 与 `version_log`、GC 拒绝的退出码与 stderr、`manifest.json` 与
发布 tar 的 sha256，以及 `NNP_FAILED`（若 NNP 下 mount 失败，附去掉 NNP 的
第二次结果）。
