# 架构与概念

p37 是「以整合事件为版本原子」的版本控制系统。本文给出阅读其他文档所需的最小
概念集；格式细节见 [`format-v2.md`](format-v2.md)，MCP 接口见
[`mcp-contract.md`](mcp-contract.md)，实际锁模型见 [`concurrency.md`](concurrency.md)。

## 1. 仓库布局

```text
<repo>/
  .p37/
    p37.db                 # SQLite 元数据（WAL）
    config.toml            # 可选配置
    objects/               # 松散 CAS 对象
    packs/                 # 离线打包后的不可变 pack
    workspaces/            # 工作区挂载点 / copy 目录
    sync/outbox/           # 待上传的同步包
    tmp/                   # 临时文件（P36 把 TMPDIR 指到这里）
    backup/                # 格式迁移前的 DB 备份
    trash/                 # repo 删除/恢复时的旧数据
    repo.json              # 多仓库模式下的身份锚（id/name/default_line）
    p37d.sock              # 多仓库 daemon 的 unix socket
```

多仓库模式的控制面在 `/srv/repos/.p37/`；每个应用是 `/srv/repos/<repo>`。
`repo_id` 形如 `repo_<32 hex>`，与目录名解耦。

## 2. 对象模型

| 对象 | 作用 |
|---|---|
| **Blob** | 大文件分块后的原始字节块 |
| **FileContent** | 文件内容描述：blob 列表、大小、EOL 属性、可执行位等 |
| **Tree** | 目录快照：路径 → 条目（普通文件 / symlink / 子树） |
| **Version** | 不可变版本：一个 Tree + 父版本 + epoch/agent/task/ts/msg |

所有对象按内容寻址（BLAKE3-256）。**version id 是 Merkle 链的一部分，一旦产生
永不改写**；GC 可以删对象、剪版本行，但不能重写 version 身份。

线性版本使用 `TypeVersion`，多父合并版本使用 `TypeVersionMerge`。`parents[0]`
是 first parent（target head）；merge 版本的 id 覆盖全部 parents。

## 3. 命名线与 DAG

- `line(name, head, protected, ...)` 是移动指针；一个版本可被多条 line 引用。
- `version_parent(version_id, parent_id, ordinal)` 是 DAG 的真相。
- `mainline_head` 保留为 default line head 的兼容镜像。
- `line_reset` 只移动 head，不产生版本；默认只允许祖先，`force + reason` 才允许
  非祖先并写 `line_reset_log`。
- `line_merge` 默认 fast-forward；`no_ff=true` 强制产生双亲 merge 版本。
- GC 的保留集合按每条 line 的祖先深度分别计算。

## 4. 工作区

工作区是 agent 直接编辑的普通目录。两种物化模式：

| 模式 | 做法 | 适用 |
|---|---|---|
| `overlay` | 只读基底 + 每个工作区独立 upper；按树共享基底 | Linux + OverlayFS 可用 |
| `copy` | 每个工作区一份完整树 | overlay 不可用 / 跨平台 |
| `auto` | 先试 overlay，失败回退 copy | 默认 |

关键状态：

```text
active / conflicted / rebasing / integrating / landed / merging / reaping / broken
```

- `landed` 通过 `landed_ts` 表达，对外显示为 `landed`。
- `hidden=1` 的 merge/preview 工作区不占用户 `max_active`，单独受 `max_hidden`
  限制，TTL 24h。
- `max_active` 只统计 `active/conflicted/rebasing`；`landed`、`integrating`、
  `hidden`、`broken` 不占额度。
- `ws_drop` 先删元数据（配额立即回收），目录清理失败只返回
  `dir_cleanup_warning`。
- `ws_reap` 默认 dry-run，不删除有 lease 的工作区。

工作区目录由 `workspace.Manager` 管理；物化和重放都经 `internal/safefs`，绝不
穿过 symlink 写到仓库之外。

## 5. op 与整合

agent 改文件后，`ws_sync` 把磁盘变化结算成 op：

```text
create / modify / delete / rename / attr / blob_put
```

op 携带 `mode_before` / `mode_after` 和 `entry_kind`，因此可执行位、文件 ↔
symlink 的类型变化、符号链接目标变化都能正确重放。op 按
`delete → rename → create → modify → attr → blob_put` 排序，避免同一路径的类型
变化被误判为「先建后删」。

`ws_land` 的流水线：

```text
capture → fold（栈折叠）→ replay（hunk rebase）→ conflict / auto-merge
        → commit（version + line head + journal + landed_ws 同一事务）
        → drop workspace（可选）
```

提交是「可恢复的三段协议」：`CommitLand` 写版本/血缘并标记 integrating，
`FinishLand` 推进 line head，`RollbackLand`/`ReconcileLand` 处理崩溃恢复。
daemon 启动时会修复未完成的 land。

## 6. 冲突

冲突不是标记行，而是结构化对象：

- `ConflictKey` 由路径 + 内容/位置信息构成，`op_id` / hunk 序号变化不影响 key；
- resolution 表按 key 保存，`take_lower` / `take_upper` / `both` / `custom`；
- 同一 `(base, ops)` 重放逐字节确定；
- 不重叠行自动合并；同锚点插入按 seq 确定性拼接；歧义不自动合并；
- `land_preview` 只记录冲突、不改工作区状态；直接 `ws_land` 冲突会把工作区置为
  `conflicted`。

## 7. 多仓库与应用卷

`internal/hub` 管 `/srv/repos/.p37/registry.db`，按需打开 repo。每个 repo 有
per-repo flock。应用卷生命周期：

```text
attach: 宿主热加 qcow2 → guest mount /srv/repos/<app> → repo_register / repo_scan
detach: repo_unregister → guest umount → 宿主 remove-device
删除卷: repo_unregister → guest umount → 宿主删卷（不要用 repo_delete）
```

`repo_unregister` 写 tombstone，不删数据；重新挂载后显式 `repo_register` 恢复原
id/name。`repo_delete` 只用于 p37 自己创建的目录，并拒绝挂载点、root 自身和
root 外路径。

## 8. 同步

同步单位是增量包：

```text
<base_prefix>/<remote_key>/
  packages/<pkg_id>/{manifest.json, meta.json, payload-*.pack}
  refs/lines.json          # 唯一提交点
```

`remote_key` 与本地 repo id 解耦。首次推送是 checkpoint（全量闭包），之后是
delta；多 line 仓库当前每次生成全量 checkpoint。包先落本地 `outbox`，显式
`sync_push` 才上传并条件更新 ref。`fetch` 不移动 head；`pull` 只快进，分叉保留
本地。详见 [`sync-contract.md`](sync-contract.md)。

## 9. 关键不变量

1. version id 永不改写；压缩/回收只作用于对象和版本行。
2. `version_parent` 是 DAG 真相；`version.parent` 只是 first parent 兼容字段。
3. op-log 可由磁盘树和 base 重建；stat_cache 只是缓存。
4. pack 不可变；先写 pack 再删松散对象，任何时刻崩溃读者都能取到对象。
5. 物化/重放不穿过 symlink；所有可写状态落在仓库根下。
6. 读路径把「祖先不可读」当作链的终点，而不是报错（GC 剪枝后仍可用）。
7. 冲突解决按内容 key 键控；内容变了冲突会重新浮现。
