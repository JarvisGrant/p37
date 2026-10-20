# 仓库格式 v2 与迁移

`format_ver = 2`。线性仓库的 CAS 版本对象与 id **完全不变**；v2 只增加多父
merge 版本、命名线、幂等与工作区扩展字段。

## 版本对象

- 线性版本：`TypeVersion (0x04)`，编码与 v1 相同。
- merge 版本：`TypeVersionMerge (0x05)`：

```text
u8  type = 0x05
uvarint parent_count
hash   parents[0..n-1]        # parents[0] = first parent = target head
hash   tree
uvarint epoch
str    agent
hash   task_hash
u64    ts
str    msg
```

merge 版本的 id 覆盖全部 parents。parents 除 first parent 外按 source head
排序，保证确定性。

## 元数据库增量

新增表：

- `line(name, head, protected, created_from, created_ts, updated_ts, deleted_ts)`
- `version_parent(version_id, parent_id, ordinal)`
- `idempotency_result(tool, key, result_json, created_ts, expires_ts)`
- `line_reset_log(...)`
- `land_into_journal(...)`（预留）

新增列：

- `repo.default_line`
- `version.idempotency_key / idempotency_tool / idempotency_ts`
- `workspace.base_line / hidden / kind / landed_ts`
- `landed_ws.target_kind / target_id`

`version.parent` 保留为 first parent 兼容旧查询；`version_parent` 是 DAG 的
真相。`mainline_head` 保留为 default line head 的兼容镜像。

## 迁移

- `repo.Open` 检测 `PRAGMA user_version=1` 时自动迁移；`p37 migrate <root>`
  也可显式执行。
- 迁移前用 `VACUUM INTO` 备份到 `<repo>/.p37/backup/p37-v1-<ts>.db`。
- 回填 `version_parent`（每个旧版本一条父边）。
- 用 `mainline_head` 建立 `main` line（protected=1）。
- 旧二进制遇到 v2 明确报错；新二进制在 v1 迁移完成前不写入。
- 历史 CAS 对象不重写，回滚只需恢复 `p37.db` 备份。

## 线语义

- `line.head` 是移动指针；一个版本可被多条 line 引用。
- `line_reset` 只移动 head，不产生版本；默认只允许祖先，`force+reason`
  才允许非祖先，并写 `line_reset_log`。
- `line_merge` 默认 fast-forward；`no_ff=true` 强制产生双亲 merge 版本。
- GC 的保留集合按每条 line 的祖先深度分别计算（`keep_versions`），并额外
  保留活跃工作区基准、7 天内幂等结果引用的版本。
