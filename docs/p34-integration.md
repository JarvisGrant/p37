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

```json
{ "repo": "my-app", "task": "...",
  "parent": { "kind": "line", "id": "p34/session/<sessionID>" } }
```

返回 `ws_id`、`dir`、`base_line`、`base_version`、`materialize`。

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
        idempotency_key=<turn-id>)
```

下回合重新 `ws_open(parent=session line)`。

## 5. 子任务

```text
parent = ws_open(parent=session line)
child  = ws_open(parent={kind:"workspace", id:parent.ws_id})
... worker 改文件 ...
p = land_preview(child, target={kind:"workspace", id:parent.ws_id})
ws_land_into(child, parent.ws_id,
             expected_parent_tree=p.target_head,
             preview_id=p.preview_id,
             idempotency_key=...)
```

子工作区默认销毁；父保留。`ws_land_into` 只允许直接父，不产生版本。

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

`sync_push` 必须 durable 成功后才能删 VM。`sync_restore` 恢复所有 line；
workspace 不恢复，P34 重新 `ws_open`。

## 注意

- 同一 `remote_prefix` 只允许单写者；P34 需要保证同一 app 的 push 串行。
- 每个 app 持久化 `remote_prefix` / `remote_key`，不要从 repo name/id 推导。
- `ws_land` / `line_merge` 的 `idempotency_key` 写入 version 并随 sync 恢复；
  `ws_land_into` 的幂等只保证同一 VM。
- 子任务成果必须在 VM 删除前落入某条 line。
- 多 line 仓库当前每次 push 生成全量 checkpoint；大仓库建议按会话保留 VM。
