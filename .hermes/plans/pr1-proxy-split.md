# PR1: proxy.go 拆分 — 请求处理重构

> **For Hermes:** Use subagent-driven-development skill to implement this plan task-by-task.

**Goal:** 拆分 1299 行的 `api/proxy.go`，按职责拆到独立的 `api/` 文件中，不改变任何行为。

**Architecture:** `proxy.go` 拆为 5 个文件（handler/decode/encode/stream/error），usage 相关逻辑拆到 `api/usage.go`，辅助函数留在原文件或合入对应文件。所有函数保持在 `api` 包中，无需改包名或 import。

**Tech Stack:** Go 1.23+ / Gin / go test

---

## 当前 proxy.go 结构分析（1299 行）

| 行范围 | 职责 | 目标文件 |
|--------|------|----------|
| 1-47   | 三个 handler 入口 + proxyRequest 核心流程 | `proxy.go` (保留) |
| 49-233 | proxyRequest 主体逻辑 | `proxy.go` (保留) |
| 235-442 | proxySameProtocolResponse + proxyCrossProtocolResponse | `stream.go` |
| 444-461 | previewBody | `error.go` |
| 463-473 | upstreamPathFor | `proxy.go` (保留) |
| 475-539 | requestHead, inspectAndMapRequestBody, passthroughRoute | `decode.go` |
| 541-585 | rewriteModel, enableStreamUsage | `decode.go` |
| 587-623 | setContentLength, copyForwardHeaders, injectUpstreamAuth | `proxy.go` (保留) |
| 625-709 | shouldnark*, upstreamErrorType, genericUpstreamMessage, context helpers | `error.go` |
| 706-739 | markKeyFailure, isHopHeader, copyResponseHeaders, copyErrString | `proxy.go` (保留) |
| 741-815 | summarizeUpstreamError, extractUpstreamErrorMessage, redact, trim | `error.go` |
| 817-880 | markAndLog | `usage.go` |
| 882-1015 | usageAccounting, usageFromResponse, usageFromIRUsage, proxyStreamAndCaptureUsage | `usage.go` |
| 1016-1241 | usageFromSSELine, usageFromRawMap, cacheRead/CreationTokens, reasoningTokens, numberValue | `usage.go` |
| 1242-1299 | estimateUsageCost, pricingSnapshot, usagePricing, priceField, writeOpenAIError | `split: usage.go + error.go` |

---

### Task 1: 创建 `api/usage.go` — 抽取 usage 计算逻辑

**Objective:** 将所有 usage token 解析、成本计算相关函数移到独立文件。

**Files:**
- Create: `api/usage.go`
- Modify: `api/proxy.go` (删除被移走的函数)

**移出的函数:**
- `usageAccounting` struct
- `usageFromResponse`
- `usageFromIRUsage`
- `proxyStreamAndCaptureUsage`
- `isSSEDataLine`
- `mergeUsageAccounting`
- `usageFromSSEBuffer`
- `usageFromSSELine`
- `usageFromRawMap`
- `cacheReadTokens`
- `cacheCreationTokens`
- `reasoningTokens`
- `numberField`
- `firstNumberField`
- `firstNumberFieldWithKey`
- `objectField`
- `numberValue`
- `maxInt`
- `estimateUsageCost`
- `pricingSnapshot`
- `usagePricing`
- `priceField`
- `recomputeTotalIfNeeded` (method on usageAccounting)
- `markAndLog`
- `usageRequestID`

**Step 1:** 创建 `api/usage.go`，将上述函数从 proxy.go 原样复制过去。

**Step 2:** 从 `api/proxy.go` 中删除被移出的函数。

**Step 3:** 运行测试验证：`go test ./api/ -v`

**Step 4:** Commit

---

### Task 2: 创建 `api/error.go` — 抽取错误处理逻辑

**Objective:** 将错误分类、脱敏、输出逻辑移到独立文件。

**Files:**
- Create: `api/error.go`

**移出的函数:**
- `previewBody`
- `shouldMarkUpstreamFailure`
- `shouldRetryWithNextKey`
- `upstreamErrorType`
- `genericUpstreamMessage`
- `classifyProxyContextError`
- `summarizeUpstreamError`
- `extractUpstreamErrorMessage`
- `findErrorMessage`
- `redactUsageError`
- `trimUsageError`
- `writeOpenAIError`
- `sensitiveErrorPatterns`
- `maxUsageErrorLen`
- `copyErrString`

**Step 1:** 创建 `api/error.go`，移入函数。
**Step 2:** 从 proxy.go 删除。
**Step 3:** `go test ./api/ -v`
**Step 4:** Commit

---

### Task 3: 创建 `api/decode.go` — 抽取请求解析逻辑

**Objective:** 将请求体解析、model mapping、model rewrite 逻辑移到独立文件。

**Files:**
- Create: `api/decode.go`

**移出的函数:**
- `requestHead` struct
- `inspectAndMapRequestBody`
- `passthroughRoute`
- `rewriteModel`
- `enableStreamUsage`

**Step 1:** 创建 `api/decode.go`，移入函数。
**Step 2:** 从 proxy.go 删除。
**Step 3:** `go test ./api/ -v`
**Step 4:** Commit

---

### Task 4: 创建 `api/stream.go` — 抽取流式响应处理逻辑

**Objective:** 将同协议/跨协议响应处理逻辑移到独立文件。

**Files:**
- Create: `api/stream.go`

**移出的函数:**
- `proxySameProtocolResponse`
- `proxyCrossProtocolResponse`

**Step 1:** 创建 `api/stream.go`，移入函数。
**Step 2:** 从 proxy.go 删除。
**Step 3:** `go test ./api/ -v`
**Step 4:** Commit

---

### Task 5: 清理 proxy.go — 只保留核心入口和直接辅助

**Objective:** proxy.go 只保留 handler 入口、proxyRequest 核心、upstreamPathFor、HTTP 头相关辅助。

**保留的函数:**
- `proxyChat`
- `proxyMessages`
- `proxyResponses`
- `proxyRequest`
- `upstreamPathFor`
- `setContentLength`
- `copyForwardHeaders`
- `injectUpstreamAuth`
- `isHopHeader`
- `copyResponseHeaders`
- `upstreamRequestContext`
- `markKeyFailure`

**Step 1:** 确认 proxy.go 只含上述函数。
**Step 2:** `go build ./... && go test ./... -v`
**Step 3:** Commit + push