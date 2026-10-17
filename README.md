# p37

**面向 AI agent 的版本控制系统。** 只通过 MCP 暴露，以「整合事件」为版本原子，
以对象存储为唯一真相源，支持堆叠工作区。不背 git 兼容包袱。

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

```bash
go build -o p37  ./cmd/p37
go build -o p37d ./cmd/p37d
go test ./...
```

> 状态：**M1–M3 主体完成，M4 核心完成，M5 部分完成。尚未经过真机（Ubuntu + ext4）
> 验证。** 详见 [`PROGRESS.md`](PROGRESS.md) 与 [`VERIFY.md`](VERIFY.md)。

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
| 存储 | 快照树 + delta 链 pack | 快照树 + CDC 分块 CAS |
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

# 起 daemon，agent 通过 MCP 连接
p37d --repo /srv/repo
```

agent 侧 MCP 配置（unix socket）：

```json
{ "mcpServers": { "p37": { "command": "p37d", "args": ["--repo", "/srv/repo"] } } }
```

HTTP 模式（**必须**配置 `auth_token`）：

```json
{ "mcpServers": { "p37": {
    "transport": "http",
    "url": "http://127.0.0.1:7757/mcp",
    "headers": { "Authorization": "Bearer <auth_token>" } } } }
```

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
同步：`sync_status`

## 文档

| 文件 | 内容 |
|---|---|
| [`docs/DESIGN.md`](docs/DESIGN.md) | 完整实现规格（含编码格式、CDC 规范、histogram diff 规格、SQLite DDL、MCP Schema、测试清单、9+ 处实现反馈） |
| [`PROGRESS.md`](PROGRESS.md) | 实现进度、已知缺口、规格修正记录 |
| [`VERIFY.md`](VERIFY.md) | 真机（Ubuntu + ext4）验证清单 |

## 安全

- HTTP 模式**强制** Bearer token；未配置时 fail-closed。
- 仓库元数据与工作区目录权限为 `0700`，数据库文件 `0600`。
- 物化与重放**绝不穿过符号链接**（`internal/safefs`）：工作区里由 agent 创建的
  恶意链接无法把写入引到仓库之外。
- 路径严格校验（拒绝 `..`、绝对路径、空段、超长）。
- 解压有上限（防 zstd 炸弹）、解码有 fuzz 覆盖。

## 许可

MIT，见 [`LICENSE`](LICENSE)。
