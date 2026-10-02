# qry-partition-compactor

把数据表的分区键、分区清单、文件数量与压缩策略记录成可查询的服务，支持按表评估小文件比例、给出压缩建议并跟踪压缩任务进度。

## 运行要求

- Go 1.26 或以上
- SQLite（本服务自带存储，不需要外部数据库）

## 构建、测试与启动

```bash
go build ./...
go test ./...
go run .
```

服务默认监听 `127.0.0.1:8080`。可用环境变量覆盖：

| 变量 | 默认值 | 用途 |
|---|---|---|
| `ADDR` | `127.0.0.1:8080` | HTTP 监听地址 |
| `DB_PATH` | `qry-partition-compactor.db` | SQLite 数据库文件路径 |

## 已公开的入口

### `GET /healthz`

返回服务与存储状态。正常时 HTTP 200：

```json
{"status":"ok","database":"ok"}
```

存储不可用时 HTTP 503：

```json
{"error":{"code":"storage_unavailable","message":"database is not available"}}
```

### `POST /api/v1/tables/{id}/partitions`

上报一个分区的快照。路径 `{id}` 是表编号，首次上报会登记该表。请求体：

```json
{"partition_key":"dt","partition_value":"2026-10-01","file_count":7,"target_file_size":134217728}
```

- 字段缺失、`file_count` 小于 0、`target_file_size` 不大于 0 或类型错误时返回 400 与 `invalid_input`，`message` 说明具体缺失或无效项。
- 成功返回 200 与存储后的快照。
- 同一表编号与分区值再次上报只更新原记录，不新增行。

### `GET /api/v1/tables/{id}/advice`

按分区值升序返回该表每个分区的压缩建议。表不存在返回 404 与 `table_not_found`。成功时 HTTP 200：

```json
{
  "table_id": 42,
  "advice": [
    {"partition_key":"dt","partition_value":"2026-09-30","file_count":1,"target_file_size":134217728,"action":"no_compaction","reason":"already_compact"},
    {"partition_key":"dt","partition_value":"2026-10-01","file_count":9,"target_file_size":134217728,"action":"compact","reason":"multiple_files"}
  ]
}
```

`action` 与 `reason` 的取值由 `file_count` 决定，建议以 `target_file_size` 为压缩目标：

| `file_count` | `action` | `reason` |
|---|---|---|
| 0 | `no_compaction` | `empty_partition` |
| 1 | `no_compaction` | `already_compact` |
| 大于 1 | `compact` | `multiple_files` |

建议是只读操作，不会创建或修改任何压缩任务。

### `POST /api/v1/compactions`

基于已上报的分区快照创建压缩任务。请求体：

```json
{"table_id":42,"partition_value":"2026-10-01"}
```

- 成功返回 201，包含任务 `id`、`status`（初始为 `running`）与快照字段。
- 输入缺失或类型错误返回 400 与 `invalid_input`。
- 表不存在返回 404 与 `table_not_found`；分区不存在返回 404 与 `partition_not_found`。
- 同一表与分区已有 `running` 任务时返回 409 与 `compaction_in_progress`，不覆盖也不新建任务。

### `GET /api/v1/compactions/{id}`

查询压缩任务，返回任务 `id`、`status` 与快照字段。未知 id 返回 404 与 `compaction_not_found`。查询是只读操作，不改变任务状态。

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；`message` 不包含 SQL、堆栈或文件路径。
