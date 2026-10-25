# P34 接入示例

## 1. 新建应用仓库

```json
POST tools/call repo_create
{ "name": "my-app", "default_line": "main",
  "remote": { "enabled": true, "backend": "s3",
              "endpoint": "https://...", "region": "auto",
              "bucket": "p37", "prefix": "users/u1/apps",
              "path_style": true } }
```

凭证只从环境变量读取：

```text
P37_S3_ACCESS_KEY / P37_S3_SECRET_KEY / P37_S3_SESSION_TOKEN
```

## 2. 每个会话一条 line

```json
{ "name": "p34/session/<sessionID>", "from": "line:main" }
```

## 3. 每回合 root workspace

推荐用 `ws_ensure`（原子 open-or-get），而不是“先 ws_list 再 ws_open”：

```json
{ "repo": "my-app", "name": "p34s-<sessionID>",
  "parent": { "kind": "line", "id": "p34/session/<sessionID>" },
  "task_desc": "...",
  "idempotency_key": "<session>/<turn>/ws" }
```

同名同 parent 的并发调用只产生一个工作区；同一 key 重放返回同一个 `ws_id`。
返回 `ws_id/dir/base_line/base_version/materialize/state/created/reused`。
`ws_open` 仍可用，并新增 `idempotency_key`；不传 key 时保持旧语义。

**命名规则：**
- `name` 是**会话级稳定**，例如 `p34s-<sessionID>`，不要带 turn 编号。
- `idempotency_key` 是 **turn 级**，例如 `<sessionID>/<turn>/ws`、`<sessionID>/<turn>/land`。
- 上一 turn 未 land 的 live ws 会被同 name+parent 的新 key 复用；land+drop 后
  新 key 会创建新一代。
- 把 turn 编号放进 name 会让上一 turn 的 ws 永远命中不了，导致配额泄漏。

agent 直接读写 `dir`。不要调用 `line_write_file`。

## 4. 回合结束

```text
ws_sync(repo, ws_id)
land_preview(repo, ws_id, target={kind:"line", id:"p34/session/<id>"})
→ target_head, ws_tree, ws_ops_epoch, conflicts

P34 跑验收

ws_land(repo, ws_id,
        target={kind:"line", id:"p34/session/<id>"},
        expected_head=<target_head>,
        expected_ws_tree=<ws_tree>,
        expected_ws_ops_epoch=<ws_ops_epoch>,
        drop_workspace=true,
        idempotency_key=<session>/<turn>/land)
```

下回合用新的 turn key 调 `ws_ensure`；不要复用上一回合的 land key。

`ws_land` 返回 `durable=false` + `durability.local_committed=true`：本地真相源
已提交；只有同步 `sync_push` 成功后才 `durable=true`，此时才可删 VM。

## 5. 子任务

```text
parent = ws_ensure(name="p34s-<sessionID>", parent=session line)
child  = ws_ensure(name="p34s-<sessionID>-task-1",
                   parent={kind:"workspace", id:parent.ws_id})
# child.base_tree = 父工作区创建子时的有效树（不可变血缘坐标）。
... worker 改文件 ...

# 推荐：先 preview 拿父树令牌 + 越界清单，再 land。
p = land_preview(child.ws_id, target={kind:"workspace", id:parent.ws_id},
                 allowed_prefixes=<工作卡目录>)
r = ws_land_into(child.ws_id, parent.ws_id,
                 expected_parent_tree=p.parent_tree_before,
                 expected_base_tree=child.base_tree,
                 allowed_prefixes=<工作卡目录>,
                 merge_policy="strict",       # 或不传，默认 auto
                 idempotency_key=<child-task-id>)
# r.state="conflict"：冲突挂在子工作区，按 key 解决后带同一个 key 重试。
# r.code="target_moved"/"parent_moved"：父被并发改动，P37 没写盘；重读 p 再试。
# r.code="path_not_allowed"：子越界改了工作卡外的路径，不要重试。
# r.code="ws_unstable"：文件正在被写，退避重试即可。
```

- 子工作区也建议用 `ws_ensure(name=..., parent={workspace:parentId})`，并且**父会话必须按
  `head_sha`/缓存句柄精确认领**，不能只按 name 在 `ws_list` 里猜；同名多活工作区
  会让子会话拿到旧快照（会话 #90）。
- 冲突挂在子工作区上，`preview_id` 等于子工作区 id；不要再为每次重试新建子工作区。
- 解决记录按冲突 key 保存，key 含工作区 id；同一子工作区重试才会命中。
- 子工作区默认销毁；父保留。`ws_land_into` 只允许直接父，不产生版本。
- **父的并发改动不会被覆盖**：v0.4.2 起只写 `applied_paths`（子改动集合），
  父在子运行期间改过、子没改的路径列在 `parent_only_changed_paths` 且保持原样。
- 子工作区冲突未解决时，父工作区 `ws_land(fold=self)` 会返回
  `ws_has_active_children`，`detail.blocked_by_children` 列出阻塞项；此时要么
  解冲突，要么 `ws_drop(child)`（配额立即回收），不要无限重试。
- 配额：派发前读 `ws_quota`；可用子任务数 = `active_available`（主管工作区
  已计入 `active_used`）。`quota_exceeded` 的 `detail` 带 `used/limit/scope`，
  应作为平台容量问题暂停+退避，而不是判定模型空转。

## 6. 用户并入应用线

```text
line_merge(source="p34/session/<id>", target="main", preview=true)
→ merge_id, conflicts, target_head

conflict_list(ws_id=merge_id)
conflict_resolve(key, resolution)

line_merge(source=..., target="main", merge_id=merge_id,
           preview=false, expected_head=<target_head>,
           idempotency_key=...)
```

默认 fast-forward；需要保留 merge 点用 `no_ff=true`。

## 7. Web 编辑

专用 `web` workspace：

```text
ws_open(parent={kind:"line", id:"main"}, task="web edit")
编辑 dir
ws_sync
ws_land(target={kind:"line", id:"main"})
```

## 8. 历史浏览

```text
version_tree(repo, ref="line:main", path="src", recursive=true)
version_file(repo, ref="version:<id>", path="src/a.go")
version_archive(repo, ref="version:<id>", path="", format="tar.gz")
```

`version_tree` 的 `hash` 是 blob id，可直接用于判断文件是否变化。

## 9. 回滚

```text
line_reset(repo, name="main", version="<ancestor>",
           expected_head=<current>, force=true, reason="user rollback")
```

默认 line 受保护，回滚必须 `force=true + reason`。

## 10. 备份 / 新 VM

```text
sync_push(repo, full=true, idempotency_key=...)
→ durable=true, remote_key, remote_prefix, remote_generation

新 VM：
sync_restore(repo="my-app", name="my-app", remote_prefix="<...>/rk_...",
             remote={enabled,backend,endpoint,bucket,prefix,path_style})
```

`sync_push` 必须 durable 成功后才能删 VM；`async=true` 不算 durable。
`sync_restore` 恢复所有 line；
workspace 不恢复，P34 重新 `ws_open`。

## 11. 自动部署（不产生版本，不占 workspace 配额）

推荐用 `repo_export`，不要用 `ws_open` 做构建：

```text
line_info(repo, name="main")            # 取 head，固定版本
repo_export(repo,
            ref="version:"+head,
            dest_dir="/srv/repos/.p37/tmp/p34-deploy/<jobId>",
            async=false)                 # 大仓库可用 true + repo_export_status
P36 exec: cd dest_dir && npm ci && npm run build
P34 直接读 dest_dir/dist/**（P37 没有读 workspace 文件的工具）
finally: rm -rf dest_dir
```

- `repo_export` 不创建 workspace、不占 `max_active`、不出现在 `ws_list`，也不会
  被 fs watcher 采集；构建产物不会进入 CAS/op/版本。
- `dest_dir` 由 P34 服务端生成，必须是绝对路径、仓库外、且（P36 默认配置）
  位于 `/srv/repos/.p37/tmp/p34-deploy` 之下；不要使用用户输入。
- 每次 job 用唯一目录，finally 删除；P37 不清理导出目录。
- 大仓库/Cloudflare 100s 场景用 `async=true` + `repo_export_status(job_id)`；
  任务只在当前 daemon 进程内有效。
- 如果必须用 workspace，固定 `parent={kind:"version",id:head}`，构建后
  `ws_drop`，**绝不调用 `ws_land`**；但 watcher 仍会采集未忽略文件。

## 工作区“消失”的兜底

```text
ws_info/ws_sync/land_preview 对已落地并回收的 ws → ok=true, state=already_landed
                                                    + version_id/line/line_head
显式 drop / 过期回收 → error.code=ws_gone
                        detail.reason=abandoned|expired|landed_and_dropped|unknown
完全未知 id → ws_not_found
目录/挂载异常 → ws_broken / ws_not_mounted（不是 gone）
```

P34 的会话级缓存建议保存 `{repo,name,parent,key,wsId}`：

```text
on ws_gone(reason=landed_and_dropped): 视为成功，刷新 line_head
on ws_gone(reason=expired|abandoned): ForgetWorkspace() → ws_ensure(新 key)
on ws_not_found: 最多重试一次（新 key），仍失败则上报
```

不要循环 `ws_open`，也不要用重试次数赌状态。

## 启动自检

会话/VM 启动时优先调用一次 `repo_health(repo)`，它聚合 repo 路径/挂载、
`persistence`、`quota`、workspace 计数、line heads 与 warnings；比
`repo_info + ws_quota + sync_status + ws_list` 少 3~4 次往返。

应用卷迁移（旧 `repo_<hash>` → `/srv/repos/<name>`）见
[`operations.md`](operations.md) 的“应用卷迁移”章节；`repo_register` 对同名
旧路径默认返回 `repo_path_mismatch`，使用 `rebind=true` 或先 unregister。

## 注意

- 同一 `remote_prefix` 只允许单写者；P34 需要保证同一 app 的 push 串行。
- 每个 app 持久化 `remote_prefix` / `remote_key`，不要从 repo name/id 推导。
- `ws_land` / `line_merge` 的 `idempotency_key` 写入 version 并随 sync 恢复；
  `ws_land_into` 的幂等只保证同一 VM。
- 子任务成果必须在 VM 删除前落入某条 line。
- 多 line 仓库当前每次 push 生成全量 checkpoint；大仓库建议按会话保留 VM。
