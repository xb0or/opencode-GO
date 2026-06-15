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

