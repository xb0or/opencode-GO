# 项目上下文

本项目是 Go + Gin 实现的 OpenCode Go 多 KEY 管理网关。

代理链路位于 `api/proxy.go`：请求先解析/可选改写 top-level `model`，再按模型路由进行协议转换、选择 KEY、重写上游真实模型并转发；普通 JSON 与 SSE 流式响应按同协议透传或跨协议转换。

代理转发请求头时必须剥离客户端凭证、`Content-Length`、`Host` 和 `Accept-Encoding`。尤其不能透传客户端 `Accept-Encoding`：如果显式带上该头，Go 的 `http.Transport` 不会自动解压上游 gzip 响应，协议转换层会把压缩字节当 JSON/SSE 解码并触发 `invalid character` 乱码错误，还可能误标 KEY 失败导致 `no available upstream key for group go`。

Model Mapping 配置位于 `config/model_mapping.go`：支持 `MODEL_MAPPINGS` JSON、`MODEL_MAPPING_FILE` JSON 文件和内存 `RegisterModelMappings`，用于将客户端模型名改写为上游模型名，并在转发前重算 `Content-Length`。

后台管理已提供 Model Mappings 页面：前端模块为 `web/js/pages/mappings.js`，管理接口为 `/admin/model-mappings`，持久化模型为 `store.ModelMappingRow`。

总览仪表盘统计由 `admin/router.go` 的 `/admin/stats` 聚合输出，前端渲染位于 `web/admin.html` 与 `web/js/pages/dashboard.js`。当前总览卡片覆盖访问令牌/API 密钥启用数、今日/累计请求、今日/累计消费、今日/累计 Token、RPM/TPM 和平均响应时间。`UsageLog` 已持久化 `input_tokens`、`output_tokens`、`cache_tokens`、`cache_read_tokens`、`cache_creation_tokens`、`total_tokens`、`total_cost`、`actual_cost`、`account_cost`，非流式同协议和跨协议 JSON 响应会尽力解析 usage 写入。缓存 Token 兼容 OpenAI/Responses 的 `prompt_tokens_details.cached_tokens` 与 Anthropic 的 `cache_read_input_tokens`、`cache_creation_input_tokens` 等字段；使用记录页会展示本页 Token、缓存 Token 和消费。

???????? SQLite ????????`modelsync/sync.go` ?? `https://opencode.ai/zen/go/v1/models` ??????????? `https://openrouter.ai/api/v1/models` ? ID/name/slug ???????????????????????????????????? 6 ????????? Models ???????????

`store.ModelRouteRow` ??? `status`?`priority`?`tags_json`?`pricing_json`?OpenRouter ???? `customized_fields_json`????????? display name?protocol?real_model?context_len?priority?pricing?tags ????? customized field???????????????`status=0` ??????????? `/v1/models` ??????????? `model_disabled`?
