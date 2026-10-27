# p37

**面向 AI agent 的版本控制系统。** 业务能力通过 MCP 暴露，以「整合事件」为版本
原子，以对象存储为唯一真相源，支持多仓库、命名线和堆叠工作区。不背 git 兼容包袱。

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

```bash
go build -o p37  ./cmd/p37
go build -o p37d ./cmd/p37d
go test ./...
```

> 状态：**v0.4.4 / format_ver=2**。多仓库、命名线、会话工作区、子工作区
> `ws_land_into`、`line_merge`、历史读取、多 line 同步与恢复、应用卷热插拔
> 生命周期均已接入。M1–M5 主体已在 P36 实例（Ubuntu 26.04 + ext4 +
> OverlayFS）上按 [`VERIFY.md`](VERIFY.md) 验证通过；v0.3.x 的当前状态和
> 已知缺口见 [`docs/status.md`](docs/status.md)，历史实现记录见
> [`PROGRESS.md`](PROGRESS.md)。

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
| 工作区 | worktree | OverlayFS：按树共享只读基底 + 各自可写层；不可用时回退 copy |
| 历史回收 | filter-repo 重写（破坏 hash） | 删根，不改写 |

## 快速开始

### 单仓库

```bash
# 开发机/非持久目录需要 --allow-nonpersistent；应用卷是真实挂载点时不要加
p37 init /srv/repo --allow-nonpersistent
p37 import /srv/repo ./my-project
p37d --repo /srv/repo          # daemon；MCP 客户端还要加 --stdio 或走 HTTP
```

### 多仓库（P34 目标形态）

```bash
mkdir -p /srv/repos
p37d --root /srv/repos
# 然后通过 MCP repo_init / line_create / ws_open / ws_land / line_merge 使用
```

`/srv/repos` 下每个应用一个 repo 目录。旧 `/srv/repo` 不移动，daemon 启动时
原地注册为 `default`。多仓库控制面在 `/srv/repos/.p37/`。

MCP 工具表与调用契约见 [`docs/mcp-contract.md`](docs/mcp-contract.md)；
概念和数据模型见 [`docs/architecture.md`](docs/architecture.md)；
P34 接入示例见 [`docs/p34-integration.md`](docs/p34-integration.md)；
验收清单见 [`docs/acceptance.md`](docs/acceptance.md)。

agent 侧 MCP 配置（stdio 代理 → 同实例 daemon 的 unix socket）：

```json
{ "mcpServers": { "p37": {
    "command": "p37d",
    "args": ["--stdio", "--root", "/srv/repos"] } } }
```

单仓库模式把 `--root /srv/repos` 换成 `--repo /srv/repo`。

> `p37d --root` / `p37d --repo` 是 daemon，不是 stdio MCP server。MCP 客户端
> 必须加 `--stdio`（P36 实例内用稳定包装 `/usr/local/bin/p37-mcp`）。stdio 代理
> 只连接已有 daemon，不取仓库锁，也不会自动启动第二个 daemon。

HTTP 模式（**必须**配置 `auth_token`）：

```json
{ "mcpServers": { "p37": {
    "transport": "http",
    "url": "http://127.0.0.1:7757/mcp",
    "headers": { "Authorization": "Bearer <auth_token>" } } } }
```

## P36 microVM 部署

P36 的 L0 保持不变，平台胶水放 L1，L2 每实例可写。p37/p37d **不烙进镜像**：
宿主投递一块只读 ext4 小盘（label `P37PAYLOAD`，固定 96 MiB），实例内的稳定
脚本挂到 `/run/p37-payload` 后执行。P36 中它是第三块 virtio-blk：
`vda=root`、`vdb=P36AGENT`、`vdc=P37PAYLOAD`。

**当前目标形态是多仓库**：仓库根是 `/srv/repos`（应用卷挂到
`/srv/repos/<app>`），旧 `/srv/repo` 仅作为 legacy 原地注册为 `default`。
p37d 以非 root 用户 `p36` 运行，systemd 只授予 `CAP_SYS_ADMIN`。

Ubuntu 26.04 非 root 下 OverlayFS 可能返回 `EPERM`（即使有 `CAP_SYS_ADMIN`）。
因此 P36 默认 `workspace.materialize = "auto"`：优先 overlay，失败自动回退
copy。copy 模式每个工作区是一份完整树，20GB L2 上需要更保守的
`max_active` / `warn_disk_bytes`。只有明确允许 root 时，才使用可选的
`config-overlay.toml` + `p37d-root-overlay.service` 走 OverlayFS。

```bash
make release VERSION=0.4.4       # 发布 tar + sha256
make p36-bootstrap               # L1 胶水：p37-run/p37-mcp/p37-mount/...
```

payload 布局、systemd unit、L2 前置、升级与已知限制见
[`packaging/p36/README.md`](packaging/p36/README.md)。部署自检分两种模式：

```bash
# 单仓库：完整健康检查（ext4/写探测/OverlayFS/inotify/磁盘）
p37d --check --strict --repo /srv/repo

# 多仓库：registry 级检查（每个 repo 的 .p37/p37.db 与 strict 下的 line 链）
p37d --check --strict --root /srv/repos
```

两者退出码一致：`0=通过`、`1=必需项失败`、`2=用法`。`--check --repo`
会做少量写探测；`--root` 模式的检查范围见 [`docs/operations.md`](docs/operations.md)。
两者都适合 daemon 启动前检查，不建议作为在线高频探针。

### P36 模板默认值（20GB L2）

下表是 [`packaging/p36/config.toml`](packaging/p36/config.toml) 的值；代码里的
`config.Default()` 在个别项上不同（例如 `warn_disk_bytes` 代码默认 12 GiB），
以实际模板为准。

| 配置 | P36 模板值 | 说明 |
|---|---:|---|
| `workspace.materialize` | `auto` | 优先 overlay，失败回退 copy |
| `workspace.base_cache_bytes` | 4 GiB | overlay 基底缓存上限（copy 不用） |
| `workspace.warn_disk_bytes` | 8 GiB | P36 模板值；代码默认 12 GiB |
| `workspace.max_active` | 8 | 主管 + 子会话至少 1+N；硬上限仍是 64 |
| `workspace.max_hidden` | 32 | 隐藏 merge/preview 工作区上限，不占 max_active |
| `repo.export_root` | `/srv/repos/.p37/tmp/p34-deploy` | 限制 `repo_export` 只能写到这里；部署导出用 |

完整配置字段和默认值见 [`docs/config.md`](docs/config.md)。

`p37 doctor` 在 copy 模式下仍会把 `OverlayFS` 标成 `FAIL`（这是检查器的既有
严格判定）；P36 的正式部署门禁应使用上面的 `p37d --check`。实际工作区模式看
`ws_open`/`ws_list` 的 `materialize`，或验收脚本输出的
`MATERIALIZE=copy|overlay`。

### 从 v1 仓库升级

`format_ver` 从 1 升到 2 时，`repo.Open` 会**自动迁移**，并在升级前把
`p37.db` 备份到 `<repo>/.p37/backup/p37-v1-<ts>.db`；一般不需要手工执行
`p37 migrate`。迁移细节和回滚方式见 [`docs/format-v2.md`](docs/format-v2.md)。

从 v0.1.12 升级时，已有 `.p37/config.toml` 若仍是
`materialize = "overlay"`，需要手工改成 `auto`（或 `copy`）；
`p37 init --if-missing` 不会覆盖旧配置。

发布与 P36 验收由 GitHub Actions 完成：

- `.github/workflows/release.yml`：tag `v*` → build → P36 验收 → 发布 Release；
- `.github/workflows/p36-acceptance.yml`：对已发布 Release 重新下载并跑 P36 验收；
- 可选 token 的配置位置见 [`.env.example`](.env.example)（不填则用
  Actions 自动的 `GITHUB_TOKEN`）。

## 维护窗口

`p37 gc` 会删除对象。daemon 的 `landMu` 只在 daemon 进程内有效；跨进程的
写保护靠 per-repo flock 和 `p37 gc` 的持锁检查。因此：

```bash
systemctl stop p37d
p37 --root /srv/repos --repo <id|name> gc --prune --pack
systemctl start p37d
```

P36 的 `/run/p37-payload` 由 `/etc/tmpfiles.d/p37.conf` 创建（不用
`RuntimeDirectory=`，避免 service 重启时在只读挂载点上重建目录）；停服后
payload 仍挂载，所以维护窗口里 `/run/p37-payload/p37` 仍可用；换 payload 盘
需重启实例。

`p37 gc` 检测到同仓库 daemon 持锁时会拒绝执行；`--force` 是显式逃生口，
会打印警告，不推荐在线使用。`fsck`、`migrate`、`import` 等写命令也应在
维护窗口执行。在线 GC 尚未实现。

## 工作方式

工作区就是一个普通目录，agent 用**它习惯的任何方式**直接改文件：

```text
ws_open / ws_ensure  →  得到一个干净目录  →  agent 直接编辑
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

完整参数、返回字段和错误码以 [`docs/mcp-contract.md`](docs/mcp-contract.md)
为准；当前注册 65 个工具。

| 分类 | 工具 |
|---|---|
| 仓库只读 | `repo_status` `repo_list` `repo_info` `repo_disk_usage` `repo_health` |
| 仓库管理 | `repo_create` `repo_delete` `repo_import` `repo_export` `repo_export_status` `repo_set_remote` |
| 应用卷生命周期 | `repo_init` `repo_register` `repo_unregister` `repo_scan` |
| 命名线 | `line_create` `line_list` `line_info` `line_delete` `line_protect` `line_reset` `line_log` `line_compare` `line_graph` `line_merge` |
| 工作区 | `ws_open` `ws_ensure` `ws_list` `ws_info` `ws_sync` `ws_diff` `ws_rebase` `ws_drop` `ws_quota` `ws_reap` `ws_disk_usage` `ws_lease` `lease_query` |
| 整合 | `land_preview` `ws_land` `ws_land_into` `land_history` |
| 冲突 | `conflict_list` `conflict_show` `conflict_resolve` `conflict_resolve_all` |
| 历史读取 | `version_log` `version_show` `version_diff` `version_tree` `version_file` `version_archive` `version_compare` `version_graph` `version_revert` `blame` |
| 同步 | `sync_status` `sync_prepare` `sync_list` `sync_show` `sync_push` `sync_fetch` `sync_pull` `sync_restore` `sync_prune` |

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
p37 sync /srv/repo prepare            # 单仓库；多仓库用 --root + --repo
p37 sync /srv/repo list
p37 sync /srv/repo show last --verify # 审查清单并逐对象校验
p37 sync /srv/repo push               # 上传并条件更新远端 ref
p37 sync /srv/repo pull               # 下载/导入对象 + 仅快进 head
p37 sync /srv/repo status --remote
```

多仓库模式：

```bash
p37 --root /srv/repos --repo <name> sync push
```

- 首次推送到空远端自动生成 **checkpoint**（全量）；之后是 **delta**。
- `fetch` 只导入对象并更新远端跟踪状态；`pull` 仅在可快进时移动本地 head，
  分叉时保持本地不动并报告 ahead/behind（不自动合并）。
- 远端布局：`<prefix>/<remote_key>/packages/<id>/{manifest.json,meta.json,payload-*.pack}`
  与 `<prefix>/<remote_key>/refs/lines.json`。
- 同一 `remote_prefix` 只支持单写者；P34 需串行化同一 app 的 push。

完整格式与恢复语义见 [`docs/sync-contract.md`](docs/sync-contract.md)。

## 文档

| 文件 | 内容 |
|---|---|
| [`docs/README.md`](docs/README.md) | 文档索引（建议从这里开始） |
| [`docs/status.md`](docs/status.md) | 当前状态、已知缺口与版本矩阵 |
| [`docs/architecture.md`](docs/architecture.md) | 概念、数据模型、整合生命周期 |
| [`docs/mcp-contract.md`](docs/mcp-contract.md) | MCP 工具、参数、返回字段与错误码 |
| [`docs/cli.md`](docs/cli.md) | `p37` / `p37d` 命令行参考 |
| [`docs/config.md`](docs/config.md) | 配置字段与默认值 |
| [`docs/operations.md`](docs/operations.md) | 升级、备份、GC、监控与排障 |
| [`docs/format-v2.md`](docs/format-v2.md) | 仓库格式 v2 与迁移 |
| [`docs/sync-contract.md`](docs/sync-contract.md) | 远端同步与恢复契约 |
| [`docs/concurrency.md`](docs/concurrency.md) | 实际并发模型与锁顺序 |
| [`docs/p34-integration.md`](docs/p34-integration.md) | P34 接入示例 |
| [`docs/acceptance.md`](docs/acceptance.md) | 验收清单 |
| [`VERIFY.md`](VERIFY.md) | 真机（Ubuntu + ext4）验证清单 |
| [`PROGRESS.md`](PROGRESS.md) | 实现历史、缺陷复盘、设计修正记录 |
| [`packaging/p36/`](packaging/p36/) | P36 microVM 部署包、payload 布局、升级与集成字段 |
| [`CONTRIBUTING.md`](CONTRIBUTING.md) | 开发、测试、发布与文档维护约定 |

## 安全

- HTTP 模式**强制** Bearer token；未配置时 fail-closed。
- unix socket 权限为 `0600`；`p37d --stdio` 只走该 socket，不额外引入 token。
- 发布物只含 `p37`、`p37d`、`manifest.json`，不含配置或密钥；S3 凭据只从
  环境变量读取。
- 所有可写状态只落在仓库根下；引导脚本把 `TMPDIR` 指到
  `/srv/repos/.p37/tmp`，p37/p37d 不做自更新。
- 仓库元数据与工作区目录权限为 `0700`，数据库文件 `0600`。
- 物化与重放**绝不穿过符号链接**（`internal/safefs`）：工作区里由 agent 创建的
  恶意链接无法把写入引到仓库之外。
- 路径严格校验（拒绝 `..`、绝对路径、空段、超长）。
- 解压有上限（防 zstd 炸弹）、解码有 fuzz 覆盖。

## 许可

MIT，见 [`LICENSE`](LICENSE)。
