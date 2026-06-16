# 项目上下文

本项目是 Go + Gin 实现的 OpenCode Go 多 KEY 管理网关。

代理链路位于 `api/proxy.go`：请求先解析/可选改写 top-level `model`，再按模型路由进行协议转换、选择 KEY、重写上游真实模型并转发；普通 JSON 与 SSE 流式响应按同协议透传或跨协议转换。

Model Mapping 配置位于 `config/model_mapping.go`：支持 `MODEL_MAPPINGS` JSON、`MODEL_MAPPING_FILE` JSON 文件和内存 `RegisterModelMappings`，用于将客户端模型名改写为上游模型名，并在转发前重算 `Content-Length`。

后台管理已提供 Model Mappings 页面：前端模块为 `web/js/pages/mappings.js`，管理接口为 `/admin/model-mappings`，持久化模型为 `store.ModelMappingRow`。

