# 外部文档同步接口

该接口供第三方系统主动向指定知识库创建、更新、查询和删除文档。第三方不需要维护版本号，只需为来源系统和文档提供稳定的唯一标识。

## 准备工作

在 WeKnora 中打开“知识库设置 → API 接入”，创建接入密钥。该密钥只绑定当前知识库，并且只包含 `ingest` 权限。

请求公共配置：

```text
Base URL: https://your-weknora.example.com
Header: X-API-Key: <接入密钥>
```

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
| `file` | 是 | 待同步的文档文件，大小受服务端 `MAX_FILE_SIZE_MB` 配置限制 |
| `metadata` | 否 | JSON 对象，值必须是字符串；元数据变化也会触发更新 |

调用示例：

```bash
curl --request PUT 'https://your-weknora.example.com/api/v1/knowledge-bases/<knowledge_base_id>/external-documents' \
  --header 'X-API-Key: <接入密钥>' \
  --form 'source_id=policy-system' \
  --form 'external_id=article-10001' \
  --form 'file=@/path/to/制度文件.pdf' \
  --form 'metadata={"department":"人力资源部","category":"管理制度"}'
```

创建或更新已受理时返回 HTTP `202`：

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
- `updated`：内容发生变化，旧文档已删除并创建新文档。
- `skipped`：文件名、文件内容和元数据均未变化，不会重复处理；此时返回 HTTP `200`。

文档创建后会自动进入 WeKnora 现有的解析、分块、摘要和向量化流程。更新操作会生成新的 `knowledge_id`，旧文档的分块、向量和知识图谱数据会由现有删除流程清理。

## 查询处理状态

```http
GET /api/v1/knowledge-bases/{knowledge_base_id}/external-documents?source_id={source_id}&external_id={external_id}
```

调用示例：

```bash
curl 'https://your-weknora.example.com/api/v1/knowledge-bases/<knowledge_base_id>/external-documents?source_id=policy-system&external_id=article-10001' \
  --header 'X-API-Key: <接入密钥>'
```

响应中的 `parse_status` 常用取值：

| 状态 | 说明 |
| --- | --- |
| `pending` | 等待处理 |
| `processing` | 正在解析、分块或向量化 |
| `finalizing` | 主体处理完成，正在生成摘要、问题或知识图谱 |
| `completed` | 全部处理完成 |
| `failed` | 处理失败，可重新调用 PUT 重试 |

处理失败时，响应会同时包含 `error_message`。文档不存在时返回 HTTP `404`。

## 删除文档

```http
DELETE /api/v1/knowledge-bases/{knowledge_base_id}/external-documents?source_id={source_id}&external_id={external_id}
```

调用示例：

```bash
curl --request DELETE 'https://your-weknora.example.com/api/v1/knowledge-bases/<knowledge_base_id>/external-documents?source_id=policy-system&external_id=article-10001' \
  --header 'X-API-Key: <接入密钥>'
```

删除成功时 `action` 为 `deleted`。文档原本不存在时仍返回成功，`action` 为 `skipped`，因此客户端可以安全重试。

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
    "message": "source_id is required and must not exceed 128 characters"
  }
}
```

常见 HTTP 状态码：

| 状态码 | 说明 |
| --- | --- |
| `400` | 参数、文件类型或元数据格式错误 |
| `401` | 未提供密钥或密钥无效 |
| `403` | 密钥没有当前知识库的 `ingest` 权限 |
| `404` | 知识库或查询的外部文档不存在 |
| `409` | 文档与现有记录冲突 |
| `429` | 当前空间的存储配额已用尽 |
| `500` | 文档存储、删除或其他内部处理失败 |
