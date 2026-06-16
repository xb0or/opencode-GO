## 2026-06-16

- 修复代理转发时透传客户端 `Accept-Encoding` 导致上游压缩响应未被 Go Transport 自动解压的问题；跨协议与同协议响应现在都能正确解码上游压缩体，避免 `invalid character '\\x1b'` 之类的乱码 JSON 解析失败。
- 新增 `Accept-Encoding` 过滤与 gzip 上游响应回归测试，验证代理能正确处理压缩后的 JSON/SSE 响应。
- 修复管理后台侧边栏“模型映射”菜单重复渲染，仅保留单一入口。
- 修复 `page-hero` 装饰伪元素拦截点击导致刷新按钮无反应的问题，并将流式/空错误显示从问号改为明确状态与空值占位。
- 流式同协议响应现在会透传 SSE 的同时捕获最终 usage；Chat 流式请求自动追加 `stream_options.include_usage=true`，调用记录可写入输入/输出/总 Token。
- 跨协议流式转换返回缓冲后的 usage 供日志统计使用，并新增流式 usage 回归测试。
## 2026-06-16

- Token 统计继续补齐缓存口径：`UsageLog` 新增缓存总量、缓存读、缓存写字段，兼容 OpenAI cached_tokens 与 Anthropic cache_*_input_tokens。
- `/admin/stats` 新增今日/累计缓存 Token、缓存读 Token、缓存写 Token 聚合；TPM 与总览卡片可展示缓存细分。
- 使用记录页新增本页 Token、本页缓存 Token、本页消费摘要，并在明细表展示输入/输出/缓存读写与单次消费。

## 2026-06-16

- 总览仪表盘补齐截图中的 8 类统计卡：访问令牌、API 密钥、今日请求、今日消费、今日 Token、累计 Token、性能指标、平均响应。
- `/admin/stats` 新增启用密钥/令牌、Token 汇总、消费汇总、TPM 等聚合字段；非流式同协议响应会解析 usage 并写入 `UsageLog`。
- 新增 `UsageLog` token/cost 字段与后台统计回归测试，验证 `go test ./...` 与前端入口语法检查通过。

## 2026-06-16

- 新增后台“模型映射”管理 UI：支持查看、新增、编辑、删除客户端模型到上游模型的改写规则。
- 新增 `/admin/model-mappings` 管理接口与 SQLite 持久化表，UI 保存后立即同步运行时 `config` 映射。
- 管理后台侧边栏与中英文文案新增 Model Mappings 页面入口，并补充 CRUD 回归测试。

## 2026-06-16

- 新增 Model Mapping 功能：支持通过 `MODEL_MAPPINGS` JSON 或 `MODEL_MAPPING_FILE` 文件配置请求模型改写规则。
- 代理转发在改写 JSON Body 后会重算并覆盖上游请求的 `Content-Length`，同时保留非 hop-by-hop 的原始请求头并注入上游 KEY。
- 非法 JSON、缺失 `model` 或未命中映射的请求改为记录 warning 并原样透传；标准 JSON 响应与 SSE 流式响应继续透传。

## 2026-06-16

- 管理后台模型列表移除真实模型与 OpenRouter 匹配 ID 展示，仅保留上下文、价格与能力标签。
- 模型能力改为中英文标签展示，中文显示“文本、视觉、视频、工具、结构化、推理”。
- API 密钥支持在后台修改密钥值、标签、权重和代理设置；分组固定为 Go，不在前端配置。
- 网关访问令牌改为 `sk-` 前缀，令牌列表提供复制按钮，仍仅支持创建和删除。
## 2026-06-16

- 启动时通过 OpenRouter `/api/v1/models` 尽力补全 Go 模型上下文长度、价格、架构、能力参数、描述与知识截止信息；网络或解析失败仅记录 warning，不阻断服务。
- `/v1/models` 增加 OpenRouter 匹配 ID/名称、上下文、价格、能力和描述等元数据字段，方便客户端发现模型能力。
- 管理后台模型路由表新增上下文、输入/输出价格、缓存读写价格、能力与 OpenRouter 匹配信息展示。

## 2026-06-16

- 按当前 OpenCode Go 官方清单更新默认模型路由：8 个 Chat Completions 模型与 6 个 Messages 模型。
- 管理后台新增模型时自动根据真实 Go 模型同步模型 ID、显示名与协议，移除上游/分组可编辑入口和列表展示列。
- 清理 `.env.example` 和文档中残留的 Zen 配置说明；仅保留 Go 实际 API URL 中的 `/zen/go` 路径；启动时过滤旧数据库中的历史非当前 Go 模型。
## 2026-06-16

- 修复管理后台语言/主题下拉菜单被遮罩拦截点击的问题。
- 修复管理后台 Key/Token/Recent Log 使用错误 JSON 字段名导致删除、开关、重置冷却和展示失效的问题。
- 修复 API 分组鉴权执行顺序：在模型路由解析后按实际 group 校验 token 权限。
- 修复 CORS middleware 注册顺序，确保已注册 API 路由也返回 CORS 响应头。
- 移除 Zen 产品/分组相关默认配置、模型、前端选项、部署变量和文档说明，仅保留 Go 上游默认路径；管理 API 禁止新建非 go 分组/上游，启动时非破坏性跳过旧数据库里的非 go 模型路由。
- 新增分组鉴权和 CORS 回归测试，验证 `go test ./...` 通过。
# CHANGELOG

## 2026-06-16

- 修复 Zeabur Docker 构建失败：builder 阶段补充 `build-base`，为 `github.com/mattn/go-sqlite3` 的 CGO 编译提供 C 工具链。
- 为 Dockerfile 增加 Zeabur `LABEL "language"="go"` 标识。
## 2026-06-16

- 修复代理上游鉴权注入：转发前剥离客户端凭证，并使用选中的 KEY 写入 `Authorization: Bearer` 与 `X-Api-Key`。
- 修复 Admin 路由挂载未绑定 `Picker` 导致 `/admin/health`、重置冷却接口潜在 panic 的问题。
- 将上游 `401/403/429/5xx` 计入 KEY 失败统计，支持失效或限流 KEY 进入冷却流程。
- 修复 Docker builder Go 版本与 `go.mod` 不一致的问题，升级为 `golang:1.25-alpine`。
- 新增 `api/proxy_test.go` 与 `admin/router_test.go` 回归测试。

## 2026-06-16 — UI 完全重写

- 对 `web/admin.html` 管理面板进行完全重做，保持单文件内嵌交付形态（`go:embed`，零构建）。
- 新增明暗主题切换：两套 CSS 变量调色板，启动时读 `localStorage`，无则跟随 `prefers-color-scheme`，顶栏按钮手动切换并持久化，Chart.js 颜色随主题联动。
- 重构布局：顶部 Header，左侧 Sidebar（inline SVG 图标），主内容卡片化，移动端侧边栏变抽屉 + 汉堡菜单。
- 四个页面全部重做：Dashboard（统计卡 + 池健康区块 + 双图表 + 最近调用表）、API Keys（行内表单 + toggle/reset/delete + 加载/空态）、Tokens（创建表单含 datetime-local 过期 + 明文 token 复制模态 +「仅此一次」警示）、Models（新增/编辑 upsert 表单 + 确认弹窗）。
- 体验增强：自定义 Modal 替代浏览器原生 confirm，全局 Toast、按钮 loading、空状态占位、复制反馈、数字格式化、耗时自动 ms/s 转换。
- 修复字段对齐：统一使用小写 JSON tag（`id`/`created_at`/`cooldown_until`/`real_model`/`context_len`），与后端 GORM 序列化完全一致。
- 新增 `GET /admin/health` 池健康状态展示，models 删除时 `encodeURIComponent(id)` 兼容含点号的 id。
## 2026-06-16

- 修复代理遇到上游 KEY 相关错误时直接返回的问题：`401/402/403/429/5xx` 会先记录失败 KEY，再自动尝试同组其它可用 KEY。
- 最近调用记录现在会从上游非 2xx 响应体提取并脱敏错误摘要，写入 `UsageLog.Error`，便于后台定位具体失败原因。
- 新增 API 与 KEY 池回归测试，覆盖 fallback 成功、最终上游错误展示、候选 KEY 顺序。
