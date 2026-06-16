# OpenCode 多 KEY 管理网关（opencode-sw）实施方案

## 一、项目定位

构建一个 **Go + Gin** 实现的 OpenCode Go 多 KEY 聚合网关，部署在 **Zeabur**。核心价值：
- 把 **Go（订阅套餐）** 上游、多账号多 KEY 聚合成**统一的通用端点**
- 客户端用任意一种协议（Claude `/v1/messages`、OpenAI `/v1/chat/completions`、OpenAI Responses `/v1/responses`）调用，网关自动路由到正确上游并完成**全协议互转（含流式）**
- 提供 **Web 管理面板**（增删改 KEY、统计、健康检查）和**多用户 Token 鉴权**

---

## 二、上游事实依据（已核实）

| 项 | Go |
|---|---|
| 性质 | 低成本订阅套餐 |
| 鉴权 | `OPENCODE_API_KEY`（`Authorization: Bearer`，部分模型走 `x-api-key`） |
| 模型协议 | 主要 ChatCompletions + Messages |
| 模型来源 | 约 40 个，见 [models.dev/providers/opencode](https://models.dev/providers/opencode) | 开源编程模型子集 |

**参考实现**：
- [new-api](https://github.com/QuantumNous/new-api)、[one-api](https://github.com/songquanpeng/one-api) — 已验证 `/v1/messages`+`/chat/completions` 多格式 + 多渠道 KEY 池可行
- Kiro-Go 系列（[hj01857655](https://github.com/hj01857655/Kiro-Go)、[Quorinex](https://github.com/Quorinex/Kiro-Go)、[jwadow/kiro-gateway](https://github.com/jwadow/kiro-gateway)）— 多账号池 / 自动 token 刷新 / Web 面板

---

## 三、目录结构

```
opencode-sw/
├── main.go                      # 入口，加载配置、启动 Gin + 后台任务
├── go.mod
├── Dockerfile                   # Zeabur 部署（多阶段构建，scratch/distroless）
├── zeabur.json                  # Zeabur 服务描述
├── README.md
├── PLAN.md                      # 本文件
├── config/
│   ├── config.go                # 配置加载（env + 文件），热重载
│   └── models.go                # 模型注册表：model_id -> {upstream(go), protocol, real_model}
├── store/
│   └── sqlite.go                # 内嵌 SQLite（key、token、用量、日志），GORM
├── pool/
│   ├── key.go                   # KEY 池：按分组(Go)+模型选择，轮询/加权/最少使用
│   └── token.go                 # 网关多用户 Token 管理 + 限流
├── upstream/
│   └── client.go                # 上游 HTTP 客户端，透明转发 header
├── protocol/                    # [Phase 3] 全协议互转 IR + 流式 SSE
│   ├── types.go                 # 三种协议的统一内部表示(IR)：Message/Content/Tool
│   ├── chat.go                  # OpenAI ChatCompletions <-> IR
│   ├── messages.go              # Anthropic Messages <-> IR
│   ├── responses.go             # OpenAI Responses <-> IR
│   ├── stream_chat.go           # SSE 互转：ChatCompletions chunk 事件
│   ├── stream_messages.go       # SSE 互转：message_start/content_block_delta/...
│   ├── stream_responses.go      # SSE 互转：response.created/output_text.delta/...
│   └── convert.go               # 任意(from,to) 配对转换 + 流式生成器
├── api/
│   ├── router.go                # /v1/chat/completions, /v1/messages, /v1/responses, /v1/models
│   ├── proxy.go                 # 通用处理：鉴权->选model->选key->转协议->转发->转回
│   ├── models.go                # GET /v1/models 返回聚合模型清单
│   └── middleware.go            # 网关 Token 校验、限流、日志
├── admin/
│   └── router.go                # /admin 面板 API（JWT 登录、KEY/Token/模型/统计 CRUD）
├── web/                         # [Phase 4] 前端静态资源（嵌入式 embed）
│   └── (Vite + Vue SPA, 产物 embed 进二进制)
└── tests/
```

---

## 四、核心设计

### 1. 协议转换（全互转，最大工作量）

采用 **中间表示（IR）** 策略，N 种协议只需写 2N 个转换函数而非 N²：
- 定义统一内部结构：`IRRequest{Model, Messages[], System, Tools[], Stream, Temperature...}`
- 每个协议实现 `Decode(raw)->IR` 和 `Encode(IR)->raw`，流式同理（`StreamDecoder`/`StreamEncoder`）
- `convert.go` 按 `(入站协议, 上游协议)` 配对：相同协议直接透传（零拷贝，最稳）；不同协议走 IR 转换
- **Responses API** 单独标注风险：它是事件式/有状态的，工具调用与 reasoning 字段映射最易出 bug，先实现核心 text + tool，复杂字段降级透传

### 2. 模型路由表（`config/models.go`）

```
"glm-5.1"          -> {upstream: go, protocol: chat,     real: "glm-5.1"}
"kimi-k2.7-code"  -> {upstream: go, protocol: chat,     real: "kimi-k2.7-code"}
"deepseek-v4-flash" -> {upstream: go, protocol: chat,  real: "deepseek-v4-flash"}
"minimax-m3"       -> {upstream: go, protocol: messages, real: "minimax-m3"}
"qwen3.7-plus"    -> {upstream: go, protocol: messages, real: "qwen3.7-plus"}
```

- 可在 Web 面板编辑，写回 SQLite，热生效
- 客户端用任意协议请求任一模型，网关查表确定真实上游协议并转码
- 启动时会尽力从 `https://openrouter.ai/api/v1/models` 按 model id/name/slug 匹配并补全上下文长度、价格、架构、能力参数等元数据；拉取失败不影响本地目录启动

### 3. KEY 池（`pool/key.go`）

- 当前管理面板固定使用 Go KEY 池；后端仍按 group 字段调度，默认值为 `go`
- 调度策略：轮询 / 加权 / 最少使用 / 故障优先回避
- 健康检查：失败计数达阈值→冷却（退避）→自动探活恢复；并发安全（`sync.Mutex`）

### 4. 多用户 Token 鉴权（`api/middleware.go`）

- 每个网关 Token：`{token, name, enabled, rate_limit, expires_at}`；新生成 token 使用 `sk-` 前缀
- 客户端 `Authorization: Bearer <gateway_token>` 访问（也兼容 `x-api-key`）
- 中间件校验→注入用户上下文→按限流放行→记录用量

### 5. Web 管理面板（`admin/` + `web/`）

- 后端：JWT 登录；KEY/Token/模型/统计 的 REST API
- 前端：轻量 SPA（Vite + Vue3），`go:embed` 打进单二进制
- 页面：登录 / Dashboard（调用数/成功率/延迟）/ KEY 管理（增删改、设置、冷却重置）/ Token 管理（创建、复制、删除）/ 模型路由表 / 调用日志

### 6. 通用端点

| 客户端端点 | 说明 |
|---|---|
| `POST /v1/chat/completions` | OpenAI Chat，兼容 `@ai-sdk/openai-compatible` |
| `POST /v1/messages` | Anthropic Messages，兼容 `@ai-sdk/anthropic` |
| `POST /v1/responses` | OpenAI Responses，兼容 `@ai-sdk/openai` |
| `GET  /v1/models` | 返回聚合模型清单 |

> opencode 对自定义网关不会自动拉 `/models`，需在 `opencode.json` 显式声明模型；但提供该端点便于其他客户端。

---

## 五、Zeabur 部署

- **Dockerfile**：多阶段（`golang:1.23-alpine` 构建 → `alpine` 运行），暴露 `PORT`（Zeabur 注入）
- **数据持久化**：SQLite 文件挂载到 Zeabur 持久存储目录（`/data`）；高并发可选 Postgres 插件
- **环境变量**：`ADMIN_PASSWORD`、`JWT_SECRET`、`UPSTREAM_TIMEOUT`、`DB_PATH` 等
- `zeabur.json` 描述服务（build/dockerfile + healthcheck `/health`）

---

## 六、分阶段交付

### 阶段 1 — 骨架 + 单协议透传（MVP）✅ 已完成
- [x] 项目脚手架、配置、SQLite、健康端点
- [x] KEY 池（轮询）+ 网关 Token 中间件
- [x] `/v1/chat/completions`、`/v1/messages`、`/v1/responses` 透明透传
- [x] 基础 Dockerfile + Zeabur 部署文档
- 交付物：能用 opencode 跑通所有三种协议的模型（各自对应协议）

### 阶段 2 — 健康检查 + 故障冷却 + 模型路由表展示
- [ ] 完善健康检查 + 指数退避冷却
- [ ] 模型路由表在面板只读展示
- [ ] 三种协议各自的模型都能稳定使用

### 阶段 3 — 全协议互转（IR + 流式 SSE 转换）
- [ ] IR 定义 + Chat/Messages/Responses 双向编解码
- [ ] 流式 SSE 互转（三对）
- [ ] Responses 复杂字段降级策略
- 交付物：任意协议可调任意模型（含流式）

### 阶段 4 — Web 管理面板
- [ ] JWT 登录、KEY/Token/模型/统计 CRUD
- [ ] SPA 前端 embed 进二进制
- [ ] 调用日志与监控图表
- 交付物：完整可视化管理

### 阶段 5 — 加固
- [ ] Go 套餐额度/到期感知、加权调度、限流（令牌桶）
- [ ] 测试覆盖（转换正确性、并发、故障转移）
- [ ] README + 客户端配置示例 + Zeabur 一键部署说明

---

## 七、风险与对策

| 风险 | 对策 |
|---|---|
| Responses API 事件式/有状态，转换易出错 | 阶段3单独验证；复杂字段先降级透传，逐步完善；写足转换单测 |
| 上游协议与模型映射可能变动 | 模型路由表可在面板编辑，热生效；不硬编码 |
| opencode 客户端对自定义 baseURL 有已知 bug（key 丢失） | README 给出规避配置；网关同时兼容 Bearer 与 x-api-key |
| Zeabur SQLite 持久化 | 用持久卷挂 `/data`；高并发可选 Postgres 插件 |
| KEY 池并发与故障抖动 | sync.Mutex + 原子计数 + 指数退避冷却 |

---

## 八、opencode 客户端配置示例

`opencode.json`（项目级或 `~/.config/opencode/opencode.json`）：

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "opencode-sw-chat": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "opencode-sw (Chat)",
      "options": {
        "baseURL": "https://<your-zeabur-domain>/v1",
        "apiKey": "{env:OCSW_TOKEN}"
      },
      "models": {
        "glm-5.1":          { "name": "GLM-5.1" },
        "kimi-k2.7-code":  { "name": "Kimi K2.7 Code" },
        "deepseek-v4-flash": { "name": "DeepSeek V4 Flash" }
      }
    },
    "opencode-sw-messages": {
      "npm": "@ai-sdk/anthropic",
      "name": "opencode-sw (Messages)",
      "options": {
        "baseURL": "https://<your-zeabur-domain>",
        "apiKey": "{env:OCSW_TOKEN}",
        "headers": { "anthropic-version": "2023-06-01" }
      },
      "models": {
        "minimax-m3":    { "name": "MiniMax M3" },
        "qwen3.7-plus": { "name": "Qwen3.7 Plus" },
        "qwen3.6-plus": { "name": "Qwen3.6 Plus" }
      }
    }
  },
  "model": "opencode-sw-chat/glm-5.1"
}
```

> 注：Anthropic provider 的 `baseURL` 不要加 `/v1`（SDK 自动追加 `/v1/messages`）。OpenAI 兼容/Responses 的 `baseURL` 必须以 `/v1` 结尾。
