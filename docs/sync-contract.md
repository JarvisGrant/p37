# 远端同步与恢复契约

## 远端布局

每个仓库使用稳定的 `remote_key`（`rk_<32 hex>`），与本地 repo id 解耦：

```text
<base_prefix>/<remote_key>/
  packages/<pkg_id>/{manifest.json, meta.json, payload-*.pack}
  refs/lines.json
```

`refs/lines.json` 是唯一提交点，包含：

```json
{
  "head": "<default line head>",
  "package": "<last package>",
  "ts": 0,
  "remote_key": "rk_...",
  "remote_prefix": "<base_prefix>/<rk_...>",
  "repo_name": "app",
  "default_line": "main",
  "generation": 12,
  "lines": {
    "main": { "head": "...", "protected": true },
    "p34/session/s1": { "head": "...", "protected": false }
  }
}
```

旧远端 `refs/mainline.json` 仍可读；新写入使用 `refs/lines.json`。

## 包内容

- `manifest.json`：可审查清单，含分片名/大小/哈希、对象数、版本数。
- `meta.json`：version / journal 等元数据行；空仓库也能恢复完整历史与 blame。
- `payload-*.pack`：按 `max_package_bytes` 切分的同步 payload；每片内部是
  拼接后的对象原始字节 + 索引，再整体 zstd 压缩。

包先写到本地 outbox（默认 `.p37/sync/outbox`），不会自动上传。上传顺序是：
包对象 → 条件更新 `refs/lines.json` → 标记本地包已上传。

## 工具与参数

| 工具 | 参数 | 说明 |
|---|---|---|
| `sync_status` | — | local/remote head、pending、ahead/behind、remote_key/prefix |
| `sync_prepare` | `force?`, `full?` | 只生成本地 outbox 包；`force` 忽略阈值，`full` 强制 checkpoint |
| `sync_list` | — | 列出暂存包 |
| `sync_show` | `id?`, `verify?` | 查看清单；`verify=true` 逐分片/逐对象校验 |
| `sync_push` | `all?`, `full?`, `lines?`, `expected_remote_generation?`, `idempotency_key?`, `async?` | 显式、同步、durable 推送 |
| `sync_fetch` | — | 下载包和 refs，不移动本地 head |
| `sync_pull` | — | 下载并快进 line；分叉保留本地 |
| `sync_restore` | `repo`, `remote_prefix`, `name?`, `remote?`, `force?` | 从远端前缀恢复（可自建 repo） |
| `sync_prune` | `dry_run?`, `remote?`, `local?` | 回收远端旧包 / 本地已上传包 |

CLI 对应命令：

```bash
p37 sync <repo> prepare [--force] [--full]
p37 sync <repo> list
p37 sync <repo> show <id|last> [--verify]
p37 sync <repo> status [--remote]
p37 sync <repo> push [--all|<id>]
p37 sync <repo> fetch
p37 sync <repo> pull
p37 sync <repo> prune [--local] [--remote]
```

## push

- 显式 `sync_push` 是同步、durable 的：包上传完成 + ref 条件更新成功后才返回。
- 返回 `durable=true`、`remote_key`、`remote_prefix`、`remote_generation`、
  `packages`、`bytes`、`remote_head`。
- `auto_push` 只是异步便利功能；VM 删除前必须显式 `sync_push`。
- `async=true` 立即返回 `{durable:false, async:true}` 并在后台推送；失败不会
  回传，不能作为删 VM 的依据。
- 多 line 仓库当前使用**全量 checkpoint**（包含所有 line 可达版本），保证恢复
  时不会遗漏不在 default head 链上的 session line；单 line 可增量。
- `expected_remote_generation` 不匹配返回 `sync_remote_moved`。
- 同一 repo 的 `prepare/push/fetch/pull/restore/prune` 互斥；并发返回
  `sync_in_progress`。
- `all` / `lines` 参数当前保留但未参与包选择；实现按 line 可达版本生成包。
- `auto_push` 可在 `[remote]` 中开启；它只做异步便利推送，不承担 durable 语义。

## fetch / pull

- `sync_fetch` 只下载包和 refs，不移动本地 head。
- `sync_pull` 对 ref 中的每条 line 做快进：
  - 本地是远端祖先 → `fast_forward`
  - 远端是本地祖先 → `local_ahead`
  - 分叉 → `diverged`，保留本地
- 本地只有空 v0 时视为空仓库，允许直接对齐远端。
- 导入顺序：校验 manifest/meta/payload 哈希与逐对象内容哈希，全部通过才写库。

## restore

```text
sync_restore(repo, remote_prefix, name?, remote?, force?)
```

- 可自建 repo；如果同名 repo 已存在且只有空 v0 + 默认 line，直接替换。
- 非空仓库必须 `force=true`；旧数据移到 `.p37/trash/`。
- 恢复所有 line/protected/default_line，不恢复 workspace/lease。
- 完成后调用方应执行 `fsck` / `p37d --check --strict --repo <repo>`。

## prune

- `sync_prune(dry_run=true)` 只返回计划，不删除。
- `remote=true` 时保留最新 checkpoint 及其之后的 delta，删除更早的包。
- `local=true` 时清理本地已上传的 outbox 包；两者都不传时默认只清本地。
- 同一 repo 的 prune 与 push/fetch/pull 互斥。

## 每回合删 VM 的约束

- 本地 repo 的 DB+CAS 是在线语义真相源；S3 是跨 VM 灾备真相源。
- `sync_push` 必须在删 VM 前成功；失败不得删 VM。
- 建议 checkpoint 周期：每 N 个版本或 N 字节；远端保留最新 checkpoint + 后续
  delta。
- 目标（P36 2 vCPU / 1 GiB / copy）：1 GiB restore < 30s，10 GiB < 120s；
  以真机为准。达不到时优先改 P34 生命周期（VM 按会话保留 / 持久卷 /
  snapshot），而不是继续压 p37。
