# p37

**面向 AI agent 的版本控制系统。** 只通过 MCP 暴露，以「整合事件」为版本原子，
以对象存储为唯一真相源，支持堆叠工作区。不背 git 兼容包袱。

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

```bash
go build -o p37  ./cmd/p37
go build -o p37d ./cmd/p37d
go test ./...
```

> 状态：**M1–M5 主体完成，已在真机（P36 实例 · Ubuntu 26.04 + ext4 + OverlayFS）上按
> `VERIFY.md` 验证通过**（门禁、功能闭环、并发、崩溃恢复、重启重挂、CDC 去重等）。
> 真机第二遍修掉了权限位丢失、rename 跨落地不跟随、`.p37ignore` 未读取、CLI 短 id
> 不可回喂、老仓库不自动补列等缺陷，并优化了采集路径。详见 [`PROGRESS.md`](PROGRESS.md)
> 与 [`VERIFY.md`](VERIFY.md)。

---

## 它解决什么问题

AI agent 的版本操作频率与人类完全不同：几十个 agent 并行、以秒计地改文件，
每一次「任务完成并整合进主干」才是一个有意义的版本点。用 git 承载这种负载会
同时撞上三件事——海量的无意义提交、随时爆发的合并冲突、不断膨胀的仓库体积。

p37 的三个核心取舍：

| 取舍 | 做法 |
|---|---|
| **过程不留** | agent 的工作区不产生 git 对象；只有整合到主干时才写一个版本。合并后的中间记录即焚，血缘另存 journal |
| **合并以内容为单位** | 不抛 `<<<<<<<` 标记行，而是给出结构化冲突（两侧任务描述 + 精确改动 + 候选解）；`land_preview` 让 AI 在落地前就看到冲突 |
| **存储按内容去重** | CDC 分块 + 内容寻址（CAS）。改一行只新增一个分块；增量包天然幂等、可乱序应用 |

## 与 git 的关键差异

| 维度 | git | p37 |
|---|---|---|
| 提交单元 | 人工 commit | 一次整合（折叠集合 → 恰好一个版本） |
| 中间过程 | 保留在历史中 | 不产生对象，落栈即焚 |
| 存储 | 快照树 + delta 链 pack | 快照树 + CDC 分块 CAS（小对象可打包） |
| 同步 | push/fetch 协议 | 幂等增量包 + 对象存储 |
| 合并 | 三方 diff3，抛标记行 | 带上下文 hunk rebase + 结构化冲突 + 候选解 |
| 冲突解决 | 无记忆 | resolution 表，按内容键控、确定性、可重放 |
| 大文件 | 需 LFS | CAS 天然处理 |
| 工作区 | worktree | OverlayFS：按树共享只读基底 + 各自可写层 |
| 历史回收 | filter-repo 重写（破坏 hash） | 删根，不改写 |

## 快速开始

```bash
p37 init /srv/repo
p37 import /srv/repo ./my-project     # 作为 v0 导入
p37 status /srv/repo
p37 doctor /srv/repo                  # 部署自检
p37 gc /srv/repo --prune --pack       # 保留策略 + 回收 + 小对象打包（§9 P2）

# 起 daemon（常驻进程，持有仓库排他锁）
p37d --repo /srv/repo
```

agent 侧 MCP 配置（stdio 代理 → 同仓库 daemon 的 unix socket）：

```json
{ "mcpServers": { "p37": {
    "command": "p37d",
    "args": ["--stdio", "--repo", "/srv/repo"] } } }
```

> `p37d --repo` 是 daemon，不是 stdio MCP server。MCP 客户端必须加
> `--stdio`（P36 实例内用稳定包装 `/usr/local/bin/p37-mcp`）。stdio 代理
> 只连接已有 daemon，不取仓库锁，也不会自动启动第二个 daemon。

HTTP 模式（**必须**配置 `auth_token`）：

```json
{ "mcpServers": { "p37": {
    "transport": "http",
    "url": "http://127.0.0.1:7757/mcp",
    "headers": { "Authorization": "Bearer <auth_token>" } } } }
```

## P36 microVM 部署

P36 的 L0/L1 只读，L2 每实例可写。p37/p37d **不烙进镜像**：宿主投递一块只读
ext4 小盘（label `P37PAYLOAD`），实例内的稳定脚本挂到 `/run/p37-payload` 后
执行。仓库固定在 `/srv/repo`，工作区用 OverlayFS；p37d 以非 root 用户 `p36`
运行，systemd 只授予 `CAP_SYS_ADMIN`。

```bash
make release VERSION=0.1.0       # 发布 tar + sha256
make p36-bootstrap               # L0 胶水：p37-run/p37-mcp/p37-mount/...
```

payload 布局、systemd unit、L2 前置、升级与已知限制见
[`packaging/p36/README.md`](packaging/p36/README.md)。部署自检用
`p37d --check --strict --repo /srv/repo [--json]`（0=通过，1=必需项失败，
2=用法），systemd/监控可直接消费退出码。

### 默认配置（20GB L2）

| 配置 | 默认值 | 说明 |
|---|---:|---|
| `workspace.base_cache_bytes` | 4 GiB | overlay 基底缓存上限 |
| `workspace.warn_disk_bytes` | 12 GiB | 剩余空间低于此值时 doctor/check 告警 |
| `workspace.max_active` | 8 | 活跃工作区默认上限（硬上限仍是 64） |

P36 模板显式写 `materialize = "overlay"`：20GB 盘上静默回退 copy 会迅速耗尽
磁盘，因此宁可让启动失败并要求修复 OverlayFS。

本轮适配**不改变仓库格式**：`format_ver` 仍为 1，无需 `p37 migrate`；配置新增
项都是可选的，旧仓库直接打开即可。

## 维护窗口

`p37 gc` 会删除对象；daemon 的 `landMu` 只在 daemon 进程内有效，CLI 与
daemon 同时跑没有共享锁。因此：

```bash
systemctl stop p37d
p37 gc /srv/repo --prune --pack
systemctl start p37d
```

`p37 gc` 检测到同仓库 daemon 持锁时会拒绝执行；`--force` 是显式逃生口，
会打印警告，不推荐在线使用。`fsck`、`migrate`、`import` 等写命令也应在
维护窗口执行。在线 GC 尚未实现。

## 工作方式

工作区就是一个普通目录，agent 用**它习惯的任何方式**直接改文件：

```
ws_open(task_desc)  →  得到一个干净目录  →  agent 直接编辑
     ↓
ws_sync             →  把磁盘变更结算成 op
     ↓
land_preview        →  预演：能看到将产生的冲突（纯只读）
     ↓
ws_land             →  折叠为一个版本；或返回结构化冲突
     ↓
conflict_resolve    →  逐条决策（take_lower / take_upper / both / custom）
```

## MCP 工具面

只读：`repo_status` `version_log` `version_show` `version_diff` `blame` `lease_query`
工作区：`ws_open` `ws_list` `ws_info` `ws_lease` `ws_sync` `ws_drop` `ws_rebase`
冲突：`conflict_list` `conflict_show` `conflict_resolve` `conflict_resolve_all`
整合：`land_preview` `ws_land` `version_revert`
同步：`sync_status` `sync_prepare` `sync_list` `sync_show`

## 远端同步（P3）

同步单位是**增量包**：包内是「版本/血缘元数据 + 对象闭包」，先落在本地
`outbox` 供审查，再显式上传到 S3 兼容对象存储。包按「上次已上传包之后的所有
版本」合并生成（低频），并对整段 payload 做 zstd 压缩（小文件多时收益明显）。

```toml
[remote]
enabled         = true
backend         = "s3"            # s3 | dir（dir 仅用于本地演练/测试）
endpoint        = "https://s3.example.com"
region          = "us-east-1"
bucket          = "p37"
prefix          = "repos/demo"
path_style      = true

# 低频 + 压缩
min_versions      = 10
min_bytes         = 4194304
compress_level    = 9
max_package_bytes = 536870912
# auto_prepare_every = "1h"       # 可选：daemon 定时打包（只暂存，不上传）
```

凭证**只从环境变量读**：`P37_S3_ACCESS_KEY` / `P37_S3_SECRET_KEY`
（可选 `P37_S3_SESSION_TOKEN`）。

```bash
p37 sync /srv/repo prepare            # 生成包到 .p37/sync/outbox（不上传）
p37 sync /srv/repo list
p37 sync /srv/repo show last --verify # 审查清单并逐对象校验
p37 sync /srv/repo push               # 上传并条件更新远端 ref
p37 sync /srv/repo pull               # 下载/导入对象 + 仅快进 head
p37 sync /srv/repo status --remote
```

- 首次推送到空远端自动生成 **checkpoint**（全量）；之后是 **delta**。
- `fetch` 只导入对象并更新远端跟踪状态；`pull` 仅在可快进时移动本地 head，
  分叉时保持本地不动并报告 ahead/behind（不自动合并）。
- 远端布局：`<prefix>/packages/<id>/{manifest.json,meta.json,payload.pack}` 与
  `<prefix>/refs/mainline.json`。

## 文档
| 文件 | 内容 |
|---|---|
| [`PROGRESS.md`](PROGRESS.md) | 实现进度、已知缺口、规格修正记录 |
| [`VERIFY.md`](VERIFY.md) | 真机（Ubuntu + ext4）验证清单 |
| [`packaging/p36/`](packaging/p36/) | P36 microVM 部署包、payload 布局、升级与集成字段 |

## 安全

- HTTP 模式**强制** Bearer token；未配置时 fail-closed。
- unix socket 权限为 `0600`；`p37d --stdio` 只走该 socket，不额外引入 token。
- 发布物只含 `p37`、`p37d`、`manifest.json`，不含配置或密钥；S3 凭据只从
  环境变量读取。
- 所有可写状态只落在仓库根下；引导脚本把 `TMPDIR` 指到
  `/srv/repo/.p37/tmp`，p37/p37d 不做自更新。
- 仓库元数据与工作区目录权限为 `0700`，数据库文件 `0600`。
- 物化与重放**绝不穿过符号链接**（`internal/safefs`）：工作区里由 agent 创建的
  恶意链接无法把写入引到仓库之外。
- 路径严格校验（拒绝 `..`、绝对路径、空段、超长）。
- 解压有上限（防 zstd 炸弹）、解码有 fuzz 覆盖。

## 许可

MIT，见 [`LICENSE`](LICENSE)。
