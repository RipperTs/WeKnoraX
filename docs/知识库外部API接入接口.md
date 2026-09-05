# 外部文档同步接口

该接口供第三方系统主动向指定知识库创建、更新、查询和删除文档。第三方不需要维护版本号，只需为来源系统和文档提供稳定的唯一标识。

## 准备工作

1. 打开“知识库设置 → API 接入 → 接入密钥”，创建并保存密钥。
2. 在“接口文档”页签复制已带入当前站点和知识库 ID 的地址与调用示例。
3. 将示例中的 `<API_KEY>` 替换为接入密钥，使用自己的来源标识、文档标识和文件路径发起请求。

密钥只绑定当前知识库，授予 `ingest` 权限，包含外部文档的新增、更新、状态查询和删除操作。这组接口的 GET 同样使用 `ingest` 权限。仅支持当前空间中的知识库。

请求公共配置：

```text
Base URL: https://your-weknora.example.com
Header: X-API-Key: <API_KEY>
```

## 接口总览

三种操作使用相同路径 `/api/v1/knowledge-bases/{knowledge_base_id}/external-documents`：

| 方法 | 用途 | 参数位置 | 成功状态码 |
| --- | --- | --- | --- |
| `PUT` | 创建或更新文档 | `multipart/form-data` 表单 | `202`：已受理；`200`：跳过重复内容 |
| `GET` | 查询处理状态 | URL 查询参数 | `200`；文档不存在为 `404` |
| `DELETE` | 删除文档 | URL 查询参数 | `200`，文档原本不存在也返回成功 |

在同一知识库中，`source_id + external_id` 唯一定位一份文档。三种操作必须使用相同标识，无需先查询内部 `knowledge_id`。两个标识均需为有效 UTF-8，服务端会去除首尾空白。

## 创建或更新文档

```http
PUT /api/v1/knowledge-bases/{knowledge_base_id}/external-documents
Content-Type: multipart/form-data
```

表单字段：

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `source_id` | 是 | 来源系统的稳定标识，最长 128 个字符，例如 `policy-system` |
| `external_id` | 是 | 文档在来源系统中的稳定唯一标识，最长 512 个字符 |
| `file` | 是 | 完整文档文件，更新时也必须上传；大小受服务端 `MAX_FILE_SIZE_MB` 配置限制 |
| `metadata` | 否 | JSON 对象，值必须是字符串；元数据变化也会触发更新 |

调用示例：

```bash
curl --request PUT 'https://your-weknora.example.com/api/v1/knowledge-bases/<knowledge_base_id>/external-documents' \
  --header 'X-API-Key: <API_KEY>' \
  --form 'source_id=policy-system' \
  --form 'external_id=article-10001' \
  --form 'file=@/path/to/制度文件.pdf' \
  --form 'metadata={"department":"人力资源部","category":"管理制度"}'
```

使用 `--form` 时，由 cURL 自动设置包含 boundary 的 `Content-Type`，无需手动填写该请求头。

创建或更新已受理时返回 HTTP `202`。这表示请求已受理，解析仍在后台进行，应继续通过 GET 查询处理状态：

```json
{
  "success": true,
  "data": {
    "action": "created",
    "source_id": "policy-system",
    "external_id": "article-10001",
    "knowledge_id": "7dd32f36-5ad6-40ea-a735-e0a809146dea",
    "parse_status": "pending",
    "content_fingerprint": "...",
    "updated_at": "2026-09-03T10:00:00Z"
  }
}
```

`action` 取值：

- `created`：首次创建。
- `updated`：已创建新文档替换旧文档，并触发旧文档清理。
- `skipped`：文件名、文件内容和元数据均未变化，且文档正在处理或已经完成，不会重复处理；此时返回 HTTP `200`。

文档创建后会自动进入 WeKnora 现有的解析、分块、摘要和向量化流程。更新操作会生成新的 `knowledge_id`，旧文档的分块、向量和知识图谱数据会由现有删除流程清理。

## 查询处理状态

```http
GET /api/v1/knowledge-bases/{knowledge_base_id}/external-documents?source_id={source_id}&external_id={external_id}
```

必填查询参数与 PUT 的标识规则一致：`source_id` 最长 128 个字符，`external_id` 最长 512 个字符。GET 不需要请求体或文件。

使用 `--get` 将参数放入 URL，`--data-urlencode` 负责中文、空格及特殊字符的编码：

```bash
curl --get --request GET 'https://your-weknora.example.com/api/v1/knowledge-bases/<knowledge_base_id>/external-documents' \
  --header 'X-API-Key: <API_KEY>' \
  --data-urlencode 'source_id=policy-system' \
  --data-urlencode 'external_id=article-10001'
```

查询成功返回 HTTP `200`，响应中没有 `action` 字段：

```json
{
  "success": true,
  "data": {
    "source_id": "policy-system",
    "external_id": "article-10001",
    "knowledge_id": "7dd32f36-5ad6-40ea-a735-e0a809146dea",
    "parse_status": "completed",
    "content_fingerprint": "...",
    "updated_at": "2026-09-03T10:00:00Z"
  }
}
```

响应中的 `parse_status` 常用取值：

| 状态 | 说明 |
| --- | --- |
| `pending` | 等待处理 |
| `processing` | 正在解析、分块或向量化 |
| `finalizing` | 主体处理完成，正在生成摘要、问题或知识图谱 |
| `completed` | 全部处理完成 |
| `failed` | 处理失败，可重新调用 PUT 提交文档 |
| `cancelled` | 处理已取消，可重新调用 PUT 提交文档 |

处理失败时，响应会同时包含 `error_message`。文档不存在时返回 HTTP `404`。

## 删除文档

```http
DELETE /api/v1/knowledge-bases/{knowledge_base_id}/external-documents?source_id={source_id}&external_id={external_id}
```

来源系统中的文档被删除或下架时，使用相同标识删除对应知识库文档。`source_id` 和 `external_id` 都是必填查询参数，长度限制与 PUT 一致，不需要上传文件或发送请求体。

```bash
curl --get --request DELETE 'https://your-weknora.example.com/api/v1/knowledge-bases/<knowledge_base_id>/external-documents' \
  --header 'X-API-Key: <API_KEY>' \
  --data-urlencode 'source_id=policy-system' \
  --data-urlencode 'external_id=article-10001'
```

此处 `--get` 将参数放入查询字符串，`--request DELETE` 指定实际 HTTP 方法为 DELETE。

删除成功返回 HTTP `200`：

```json
{
  "success": true,
  "data": {
    "action": "deleted",
    "source_id": "policy-system",
    "external_id": "article-10001",
    "knowledge_id": "7dd32f36-5ad6-40ea-a735-e0a809146dea",
    "parse_status": "completed",
    "content_fingerprint": "...",
    "updated_at": "2026-09-03T10:00:00Z"
  }
}
```

删除沿用现有文档删除流程，清理文档及关联索引。响应中的 `parse_status` 和 `updated_at` 是删除前的文档信息，不是删除进度。

文档原本不存在时，返回 HTTP `200`：

```json
{
  "success": true,
  "data": {
    "action": "skipped",
    "source_id": "policy-system",
    "external_id": "article-10001"
  }
}
```

删除成功时 `action` 为 `deleted`。文档原本不存在时仍返回成功，`action` 为 `skipped`，因此客户端可以安全重试。

## 响应字段

| 字段 | 含义 |
| --- | --- |
| `success` | 请求是否成功；创建、更新成功受理不代表解析完成 |
| `data.action` | PUT：`created`、`updated` 或 `skipped`；DELETE：`deleted` 或 `skipped`；GET 不返回此字段 |
| `data.source_id` / `data.external_id` | 来源系统与外部文档标识 |
| `data.knowledge_id` | 当前内部文档 ID，更新时会变化；删除不存在的文档时省略 |
| `data.parse_status` | 解析状态；删除成功时为删除前状态 |
| `data.error_message` | 处理错误信息，没有错误时省略 |
| `data.content_fingerprint` | 服务端计算的内容指纹，客户端无需计算 |
| `data.updated_at` | 文档更新时间，RFC 3339 格式；DELETE 返回删除前的值 |

## 幂等与并发规则

- 服务端根据文件名、文件内容和业务元数据计算 SHA-256 指纹，客户端不需要计算或保存版本号。
- 同一个 `knowledge_base_id + source_id + external_id` 的请求会串行执行，不会同时修改同一文档。
- 内容未变化且文档正在处理或已经完成时，重复 PUT 会直接跳过。
- 上一次处理状态为 `failed` 或 `cancelled` 时，重复 PUT 会重新创建并处理文档。
- 没有客户端版本号时，服务端无法判断业务上的新旧顺序；同一文档按获得锁后的执行顺序处理，最后成功执行的请求生效。来源系统应按文档顺序发送更新，不要主动并发发送同一文档的不同历史内容。

## 错误响应

```json
{
  "success": false,
  "error": {
    "code": 1000,
    "message": "source_id is required, must be valid UTF-8, and must not exceed 128 characters"
  }
}
```

常见 HTTP 状态码：

| 状态码 | 说明 |
| --- | --- |
| `400` | 参数、文件类型、文件大小或元数据格式错误 |
| `401` | 未提供密钥或密钥无效 |
| `403` | 密钥没有当前知识库的 `ingest` 权限 |
| `404` | 知识库或查询的外部文档不存在 |
| `409` | 文档与现有记录冲突 |
| `429` | 当前空间的存储配额已用尽 |
| `500` | 文档存储、删除或其他内部处理失败 |
