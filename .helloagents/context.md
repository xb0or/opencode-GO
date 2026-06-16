# 项目上下文

本项目是 Go + Gin 实现的 OpenCode Go 多 KEY 管理网关。

代理链路位于 `api/proxy.go`：请求先解析/可选改写 top-level `model`，再按模型路由进行协议转换、选择 KEY、重写上游真实模型并转发；普通 JSON 与 SSE 流式响应按同协议透传或跨协议转换。

Model Mapping 配置位于 `config/model_mapping.go`：支持 `MODEL_MAPPINGS` JSON、`MODEL_MAPPING_FILE` JSON 文件和内存 `RegisterModelMappings`，用于将客户端模型名改写为上游模型名，并在转发前重算 `Content-Length`。

后台管理已提供 Model Mappings 页面：前端模块为 `web/js/pages/mappings.js`，管理接口为 `/admin/model-mappings`，持久化模型为 `store.ModelMappingRow`。

总览仪表盘统计由 `admin/router.go` 的 `/admin/stats` 聚合输出，前端渲染位于 `web/admin.html` 与 `web/js/pages/dashboard.js`。当前总览卡片覆盖访问令牌/API 密钥启用数、今日/累计请求、今日/累计消费、今日/累计 Token、RPM/TPM 和平均响应时间。`UsageLog` 已持久化 `input_tokens`、`output_tokens`、`cache_tokens`、`cache_read_tokens`、`cache_creation_tokens`、`total_tokens`、`total_cost`、`actual_cost`、`account_cost`，非流式同协议和跨协议 JSON 响应会尽力解析 usage 写入。缓存 Token 兼容 OpenAI/Responses 的 `prompt_tokens_details.cached_tokens` 与 Anthropic 的 `cache_read_input_tokens`、`cache_creation_input_tokens` 等字段；使用记录页会展示本页 Token、缓存 Token 和消费。

