# 仓库格式 v3：typed object 地址

v3 在 v2 基础上只改**对象地址**，不改版本 id 的编码：

```text
v2:  id  = BLAKE3(raw)          key = objects/<id>
v3:  id  = BLAKE3(raw)          key = objects/<type>/<id>
```

`type` 是对象类型字节（`01=Blob`、`02=FileContent`、`03=Tree`、`04=Version`、
`05=VersionMerge`）的两位十六进制。对象索引 `object_index` 主键从 `id` 改为
`(id, type)`。

## 为什么需要

对象 id 只覆盖 raw。极少数情况下同一份 raw 可以同时是两种对象，例如空 Tree
的编码恰好是 `{0x03,0x00}`，用户文件也可以正好是这两个字节。v2 的去重会
让文件指向 Tree，之后所有读取都失败。v3 用类型命名空间让两种对象共存。

## 迁移

`repo.Open` 发现 `user_version < 3` 时：

1. 用 `VACUUM INTO` 备份到 `<repo>/.p37/backup/p37-v<旧版本>-<ts>.db`；
2. 删除 `object_index` 缓存（它是派生数据）；
3. 重建 `object_index` 为 `(id,type)` 复合主键；
4. 首次打开时扫描 `objects/` 回填索引；
5. 写入 `PRAGMA user_version=3`。

旧 `objects/<id>` 对象仍可读；新写对象使用 `objects/<type>/<id>`。不需要手工
搬动旧对象，也不需要重写 version/tree id。

## 回滚

旧二进制遇到 `format_ver=3` 会明确拒绝打开。VM 平台必须把最低 p37 版本作为硬
门槛，不允许回退到不认识 v3 的旧 L1。回滚只能恢复迁移前的 `p37.db` 备份，
并接受迁移后新写入的对象不可见。
