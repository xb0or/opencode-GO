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
