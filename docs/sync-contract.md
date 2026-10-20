# 远端同步与恢复契约

## 远端布局

每个仓库使用稳定的 `remote_key`（`rk_<32 hex>`），与本地 repo id 解耦：

```text
<base_prefix>/<remote_key>/
  packages/<pkg_id>/{manifest.json,meta.json,payload.*}
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

## push

- 显式 `sync_push` 是同步、durable 的：包上传完成 + ref 条件更新成功后才返回。
- 返回 `durable=true`、`remote_key`、`remote_prefix`、`remote_generation`、
  `packages`、`bytes`、`remote_head`。
- `auto_push` 只是异步便利功能；VM 删除前必须显式 `sync_push`。
- 多 line 仓库本轮使用全量 checkpoint 包（包含所有 line 可达版本），保证
  恢复时不会遗漏不在 default head 链上的 session line。
- `expected_remote_generation` 不匹配返回 `sync_remote_moved`。

## fetch / pull

- `sync_fetch` 只下载包和 refs，不移动本地 head。
- `sync_pull` 对 ref 中的每条 line 做快进：
  - 本地是远端祖先 → `fast_forward`
  - 远端是本地祖先 → `local_ahead`
  - 分叉 → `diverged`，保留本地
- 本地只有空 v0 时视为空仓库，允许直接对齐远端。

## restore

```text
sync_restore(name, remote_prefix, remote?, force?)
```

- 可自建 repo；如果同名 repo 已存在且只有空 v0 + 默认 line，直接替换。
- 非空仓库必须 `force=true`；旧数据移到 `.p37/trash/`。
- 恢复所有 line/protected/default_line，不恢复 workspace/lease。
- 完成后调用方应执行 `fsck`/`--check --strict`。

## 每回合删 VM 的约束

- 本地 repo 是缓存；S3 是唯一真相源。
- `sync_push` 必须在删 VM 前成功；失败不得删 VM。
- 建议 checkpoint 周期：每 N 个版本或 N 字节；远端保留最新 checkpoint + 后续
  delta。
- 目标（P36 2 vCPU / 1 GiB / copy）：1 GiB restore < 30s，10 GiB < 120s；
  以真机为准。达不到时优先改 P34 生命周期（VM 按会话保留 / 持久卷 /
  snapshot），而不是继续压 p37。
