# 并发模型与锁顺序

## 锁

```text
repo_registry 锁
  → repo_mutation 锁（每个 repo 一把；保护 line head / 版本提交）
    → workspace 锁（多个时按 ws_id 升序）
      → lease 检查（不是锁）
```

规则：

- 会改 line head 的操作（`ws_land(target=line)`、`line_merge`、`line_reset`、
  `line_delete`、`repo_delete`、`version_revert`）先拿 `repo_mutation`。
- 只改工作区的操作（`ws_sync`、`ws_diff`、`ws_rebase`、`ws_land_into`、
  `ws_drop`）只拿 workspace 锁，不拿 `repo_mutation`。
- 严禁反向顺序（先 workspace 后 repo_mutation）。
- 多工作区操作按 `ws_id` 升序取锁。
- land 流水线与 GC 共用 `repo_mutation`；不同 repo 互不影响。
- `land_into` 当前在 repo 级 mutation 锁内串行执行，父工作区目录写入与 op
  重算在同一临界区完成。

## 工作区状态

`active` / `conflicted` / `rebasing` / `integrating` / `landed` / `merging` /
`reaping` / `broken`。

- `landed` 用 `landed_ts` 表达（state 仍存 active），`ws_info`/`ws_list`
  对外显示 `landed`。
- 隐藏 merge/preview 工作区 `hidden=1`，不占用户 `max_active`，单独
  `max_hidden`（默认 32），默认不出现在 `ws_list`。
- `ws_reap` 不删除有 lease 的工作区；用户 active 工作区不自动回收；隐藏
  merge/preview TTL 24h；landed/broken 可回收。

## 幂等

- `ws_land` / `line_merge` 的 key 写入 version（`idempotency_key/tool/ts`）。
- 无版本结果（FF merge / no-op）写入 `idempotency_result`。
- `sync_push` 也有 `idempotency_result`；重复调用返回原结果。
- 幂等记录 7 天 TTL；只有未过期记录引用的版本进入 GC 根。
- `ws_land_into` 只保证同一 VM 生命周期（restore 不恢复 workspace/lease）。
