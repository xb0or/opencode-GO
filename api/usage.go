package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xb0or/opencode-GO/config"
	streampkg "github.com/xb0or/opencode-GO/internal/stream"
	"github.com/xb0or/opencode-GO/pool"
	"github.com/xb0or/opencode-GO/protocol"
	"github.com/xb0or/opencode-GO/store"
	"gorm.io/gorm"
)

// markAndLog writes a usage log row. It never blocks the response path on DB errors.
func markAndLog(c *gin.Context, p *pool.Picker, key *store.Key, route config.ModelRoute,
	proto config.Protocol, status int, start time.Time, stream bool, usage *usageAccounting, errMsg string, firstResponseMs ...int64) {
	var tokenID uint
	var tokenName string
	if tokAny, exists := c.Get("token"); exists {
		if tok, ok := tokAny.(*store.Token); ok {
			tokenID = tok.ID
			tokenName = tok.Name
		}
	}
	if usage == nil {
		usage = &usageAccounting{}
	}
	baseCost := estimateUsageCost(route, usage)
	groupMultiplier := config.GroupMultiplier(route.Group)
	finalCost := baseCost * groupMultiplier
	if groupMultiplier <= 0 || math.IsNaN(finalCost) || math.IsInf(finalCost, 0) {
		groupMultiplier = 1
		finalCost = baseCost
	}
	frt := int64(0)
	ttft := int64(0)
	if len(firstResponseMs) > 0 && firstResponseMs[0] > 0 {
		frt = firstResponseMs[0]
	}
	if len(firstResponseMs) > 1 && firstResponseMs[1] > 0 {
		ttft = firstResponseMs[1]
	}
	pricing := usagePricing(route)
	entry := store.UsageLog{
		RequestID:           usageRequestID(c, key, start),
		TokenID:             tokenID,
		TokenName:           tokenName,
		KeyID:               key.ID,
		Model:               route.ID,
		Group:               route.Group,
		Protocol:            string(proto),
		IPAddress:           c.ClientIP(),
		StatusCode:          status,
		DurationMs:          time.Since(start).Milliseconds(),
		FirstResponseMs:     frt,
		TTFTMs:              ttft,
		Stream:              stream,
		InputTokens:         usage.InputTokens,
		OutputTokens:        usage.OutputTokens,
		ReasoningTokens:     usage.ReasoningTokens,
		CacheTokens:         usage.CacheTokens,
		CacheReadTokens:     usage.CacheReadTokens,
		CacheCreationTokens: usage.CacheCreationTokens,
		TotalTokens:         usage.TotalTokens,
		TotalCost:           baseCost,
		ActualCost:          finalCost,
		AccountCost:         finalCost,
		InputUnitPrice:      pricing.Prompt,
		OutputUnitPrice:     pricing.Completion,
		CacheReadUnitPrice:  pricing.CacheRead,
		CacheWriteUnitPrice: pricing.CacheCreation,
		GroupMultiplier:     groupMultiplier,
		BillingMode:         "token",
		Error:               errMsg,
	}
	_ = store.DB().Create(&entry).Error
}

// responseTiming contains the two stream milestones shown in the usage UI.
// FRT is the time to the first upstream SSE event; TTFT is the time to the
// first actual generated token/content delta. Both are zero when the stream
// did not reach the corresponding milestone.
type responseTiming struct {
	FirstResponseMs int64
	TTFTMs          int64
}

func (m responseTiming) args() []int64 {
	return []int64{m.FirstResponseMs, m.TTFTMs}
}

// streamTimingCapture observes SSE bytes without changing the bytes written
// to the client. It is deliberately protocol-aware only for identifying the
// first generated token; usage extraction remains in the existing parsers.
type streamTimingCapture struct {
	proto   config.Protocol
	start   time.Time
	pending []byte
	timing  responseTiming
}

func newStreamTimingCapture(proto config.Protocol, start time.Time) *streamTimingCapture {
	return &streamTimingCapture{proto: proto, start: start}
}

func (c *streamTimingCapture) Write(p []byte) (int, error) {
	c.pending = append(c.pending, p...)
	for {
		i := bytes.IndexByte(c.pending, '\n')
		if i < 0 {
			break
		}
		line := append([]byte(nil), c.pending[:i+1]...)
		c.pending = c.pending[i+1:]
		c.observeLine(line)
	}
	return len(p), nil
}

func (c *streamTimingCapture) Observe(p []byte) {
	_, _ = c.Write(p)
}

func (c *streamTimingCapture) observeLine(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	if c.timing.FirstResponseMs == 0 {
		c.timing.FirstResponseMs = maxInt64(1, time.Since(c.start).Milliseconds())
	}
	if c.timing.TTFTMs == 0 && sseLineHasGeneratedToken(c.proto, payload) {
		c.timing.TTFTMs = maxInt64(1, time.Since(c.start).Milliseconds())
	}
}

func (c *streamTimingCapture) Metrics() responseTiming {
	// The final SSE line is allowed to omit its trailing newline. Flush the
	// pending fragment before persisting metrics so a last data event cannot
	// make FRT/TTFT appear empty.
	if len(c.pending) > 0 {
		pending := c.pending
		c.pending = nil
		c.observeLine(pending)
	}
	return c.timing
}

func (c *streamTimingCapture) SetFirstResponseNow() {
	if c.timing.FirstResponseMs == 0 {
		c.timing.FirstResponseMs = maxInt64(1, time.Since(c.start).Milliseconds())
	}
}

// firstByteTimingReader records the time at which the upstream first returns
// response bytes. This is the stable FRT boundary even when the first read
// contains Responses lifecycle events that are intentionally buffered before
// the first content event is forwarded.
type firstByteTimingReader struct {
	src     io.Reader
	capture *streamTimingCapture
}

func (r *firstByteTimingReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if n > 0 {
		r.capture.SetFirstResponseNow()
	}
	return n, err
}

type timingWriter struct {
	dst     io.Writer
	capture *streamTimingCapture
}

func (w *timingWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	if n > 0 {
		w.capture.Observe(p[:n])
	}
	return n, err
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func sseLineHasGeneratedToken(proto config.Protocol, payload []byte) bool {
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return false
	}
	nonEmpty := func(v any) bool {
		s, ok := v.(string)
		return ok && strings.TrimSpace(s) != ""
	}
	nonEmptyContent := func(v any) bool {
		if nonEmpty(v) {
			return true
		}
		items, ok := v.([]any)
		if !ok {
			return false
		}
		for _, item := range items {
			m, _ := item.(map[string]any)
			if nonEmpty(m["text"]) || nonEmpty(m["value"]) || nonEmpty(m["content"]) {
				return true
			}
		}
		return false
	}
	if proto == config.ProtocolChat {
		choices, _ := raw["choices"].([]any)
		for _, item := range choices {
			choice, _ := item.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if nonEmptyContent(delta["content"]) || nonEmptyContent(delta["reasoning_content"]) || nonEmptyContent(delta["text"]) {
				return true
			}
			if calls, ok := delta["tool_calls"].([]any); ok {
				for _, call := range calls {
					m, _ := call.(map[string]any)
					fn, _ := m["function"].(map[string]any)
					if nonEmpty(m["arguments"]) || nonEmpty(fn["arguments"]) || nonEmpty(fn["name"]) {
						return true
					}
				}
			}
		}
		return false
	}
	if proto == config.ProtocolMessages {
		if raw["type"] == "content_block_delta" {
			delta, _ := raw["delta"].(map[string]any)
			return nonEmptyContent(delta["text"]) || nonEmptyContent(delta["partial_json"]) || nonEmptyContent(delta["thinking"])
		}
		return false
	}
	if proto == config.ProtocolResponses {
		typ, _ := raw["type"].(string)
		return strings.HasSuffix(typ, ".delta") && (strings.Contains(typ, "text") || strings.Contains(typ, "arguments") || strings.Contains(typ, "reasoning")) && nonEmptyContent(raw["delta"])
	}
	return false
}

// incrementRequestsUsed increments the request counter for the token
// associated with this request. It should only be called once per client
// request, on the final successful upstream attempt — not on intermediate
// key-level or upstream-level retries.
//
// Semantics: This is called when result.Handled is true, which means the
// response was committed to the client (ResponseStarted == true). Due to the
// response handlers' early-return on resp.StatusCode >= 400, ResponseStarted
// is only set for 2xx/3xx responses. Therefore, incrementRequestsUsed is only
// called for successful responses — failed upstreams that trigger failover
// do not consume a request count.
func incrementRequestsUsed(c *gin.Context) {
	if tokAny, exists := c.Get("token"); exists {
		if tok, ok := tokAny.(*store.Token); ok {
			if tok.MaxRequests > 0 {
				store.DB().Model(&store.Token{}).Where("id = ?", tok.ID).
					UpdateColumn("requests_used", gorm.Expr("requests_used + 1"))
			}
		}
	}
}

func usageRequestID(c *gin.Context, key *store.Key, start time.Time) string {
	for _, header := range []string{"X-Request-Id", "X-Request-ID", "Request-Id", "Request-ID"} {
		if v := strings.TrimSpace(c.Writer.Header().Get(header)); v != "" {
			return v
		}
		if v := strings.TrimSpace(c.GetHeader(header)); v != "" {
			return v
		}
	}
	keyID := uint(0)
	if key != nil {
		keyID = key.ID
	}
	return fmt.Sprintf("req_%d_%d", start.UnixNano(), keyID)
}

type usageAccounting struct {
	InputTokens          int
	OutputTokens         int
	ReasoningTokens      int
	CacheTokens          int
	CacheReadTokens      int
	CacheCreationTokens  int
	TotalTokens          int
	CacheIncludedInInput bool
	TotalExplicit        bool
}

func usageFromResponse(proto config.Protocol, body []byte) *usageAccounting {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	u, _ := raw["usage"].(map[string]any)
	return usageFromRawMap(u, proto)
}

func usageFromIRUsage(resp *protocol.IRResponse) *usageAccounting {
	if resp == nil || resp.Usage == nil {
		return nil
	}
	u := resp.Usage
	acct := &usageAccounting{
		InputTokens:         u.PromptTokens,
		OutputTokens:        u.CompletionTokens,
		TotalTokens:         u.TotalTokens,
		TotalExplicit:       u.TotalTokens > 0,
		CacheReadTokens:     u.CacheReadTokens,
		CacheCreationTokens: u.CacheCreationTokens,
		ReasoningTokens:     u.ReasoningTokens,
	}
	if acct.CacheReadTokens > 0 {
		acct.CacheTokens = acct.CacheReadTokens
	}
	acct.recomputeTotalIfNeeded()
	if acct.InputTokens == 0 && acct.OutputTokens == 0 && acct.TotalTokens == 0 {
		return nil
	}
	return acct
}

func proxyStreamAndCaptureUsage(dst io.Writer, src io.Reader, proto config.Protocol, start time.Time) (*usageAccounting, int64, error) {
	var usage *usageAccounting
	pipeResult := streampkg.Pipe(dst, src, start, func(rawLine []byte) error {
		// Snapshot the line since scanner.Bytes() is reused on next Scan.
		line := append([]byte(nil), rawLine...)
		if nextUsage := usageFromSSELine(proto, line); nextUsage != nil {
			usage = mergeUsageAccounting(usage, nextUsage)
		}
		return nil
	})
	return usage, pipeResult.FirstByteMs, pipeResult.Err
}

func isSSEDataLine(line []byte) bool {
	if !bytes.HasPrefix(line, []byte("data: ")) {
		return false
	}
	payload := bytes.TrimSpace(line[6:])
	return len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]"))
}

func mergeUsageAccounting(base, next *usageAccounting) *usageAccounting {
	if base == nil {
		return next
	}
	if next == nil {
		return base
	}
	if next.InputTokens > 0 {
		base.InputTokens = next.InputTokens
	}
	if next.OutputTokens > 0 {
		base.OutputTokens = next.OutputTokens
	}
	if next.ReasoningTokens > 0 {
		base.ReasoningTokens = next.ReasoningTokens
	}
	if next.CacheTokens > 0 {
		base.CacheTokens = next.CacheTokens
	}
	if next.CacheReadTokens > 0 {
		base.CacheReadTokens = next.CacheReadTokens
	}
	if next.CacheCreationTokens > 0 {
		base.CacheCreationTokens = next.CacheCreationTokens
	}
	base.CacheIncludedInInput = base.CacheIncludedInInput || next.CacheIncludedInInput
	if next.TotalExplicit {
		base.TotalTokens = next.TotalTokens
		base.TotalExplicit = true
	} else {
		base.recomputeTotalIfNeeded()
	}
	return base
}

func (u *usageAccounting) recomputeTotalIfNeeded() {
	if u == nil || u.TotalExplicit {
		return
	}
	u.TotalTokens = u.InputTokens + u.OutputTokens + u.CacheReadTokens
}

func usageFromSSEBuffer(proto config.Protocol, body []byte) *usageAccounting {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 256*1024), 4*1024*1024)
	var usage *usageAccounting
	for scanner.Scan() {
		if next := usageFromSSELine(proto, scanner.Bytes()); next != nil {
			usage = mergeUsageAccounting(usage, next)
		}
	}
	return usage
}

func usageFromSSELine(proto config.Protocol, line []byte) *usageAccounting {
	if !bytes.HasPrefix(line, []byte("data: ")) {
		return nil
	}
	payload := bytes.TrimSpace(line[6:])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil
	}
	switch proto {
	case config.ProtocolChat:
		return usageFromRawMap(objectField(raw, "usage"), proto)
	case config.ProtocolMessages:
		if usage := usageFromRawMap(objectField(raw, "usage"), proto); usage != nil {
			return usage
		}
		if msg := objectField(raw, "message"); msg != nil {
			return usageFromRawMap(objectField(msg, "usage"), proto)
		}
	case config.ProtocolResponses:
		if response := objectField(raw, "response"); response != nil {
			if usage := usageFromRawMap(objectField(response, "usage"), proto); usage != nil {
				return usage
			}
		}
		return usageFromRawMap(objectField(raw, "usage"), proto)
	}
	return nil
}

func usageFromRawMap(u map[string]any, _ config.Protocol) *usageAccounting {
	if len(u) == 0 {
		return nil
	}
	acct := &usageAccounting{}
	rawInputTokens := firstNumberField(u, "prompt_tokens", "input_tokens")
	acct.OutputTokens = firstNumberField(u, "completion_tokens", "output_tokens")
	acct.ReasoningTokens = reasoningTokens(u)
	acct.CacheReadTokens, acct.CacheIncludedInInput = cacheReadTokens(u)
	var cacheCreationIncluded bool
	acct.CacheCreationTokens, cacheCreationIncluded = cacheCreationTokens(u)
	acct.InputTokens = rawInputTokens
	if acct.CacheIncludedInInput && acct.CacheReadTokens > 0 {
		acct.InputTokens = maxInt(0, acct.InputTokens-acct.CacheReadTokens)
	}
	if !cacheCreationIncluded && acct.CacheCreationTokens > 0 {
		acct.InputTokens += acct.CacheCreationTokens
	}
	// CacheTokens is intentionally the cache-read/hit amount only. Cache
	// creation/write tokens are tracked separately but billed as regular input,
	// so they must not be mixed into cache-hit counters.
	acct.CacheTokens = acct.CacheReadTokens
	acct.TotalTokens = numberField(u, "total_tokens")
	acct.TotalExplicit = acct.TotalTokens > 0
	acct.recomputeTotalIfNeeded()
	return acct
}

func cacheReadTokens(u map[string]any) (int, bool) {
	direct, directKey := firstNumberFieldWithKey(u,
		"cache_read_input_tokens",
		"input_cache_read_tokens",
		"cache_read_tokens",
		"prompt_cache_hit_tokens",
		"prompt_cache_read_tokens",
		"cached_tokens",
	)
	directIncluded := directKey == "prompt_cache_hit_tokens" ||
		directKey == "prompt_cache_read_tokens" ||
		directKey == "cached_tokens"
	nested := 0
	for _, key := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if details := objectField(u, key); details != nil {
			nested = maxInt(nested, firstNumberField(details,
				"cached_tokens",
				"cache_read_tokens",
				"cache_read_input_tokens",
				"input_cache_read_tokens",
				"read_tokens",
			))
		}
	}
	if nested > 0 {
		return maxInt(direct, nested), true
	}
	return direct, directIncluded
}

func cacheCreationTokens(u map[string]any) (int, bool) {
	total, directKey := firstNumberFieldWithKey(u,
		"cache_creation_input_tokens",
		"cache_write_input_tokens",
		"input_cache_write_tokens",
		"cache_creation_tokens",
		"prompt_cache_miss_tokens",
		"prompt_cache_write_tokens",
	)
	directIncluded := directKey == "prompt_cache_miss_tokens" ||
		directKey == "prompt_cache_write_tokens"
	detailTotal := 0
	if details := objectField(u, "cache_creation"); details != nil {
		for _, v := range details {
			detailTotal += numberValue(v)
		}
	}
	if details := objectField(u, "cache_creation_input_tokens_details"); details != nil {
		for _, v := range details {
			detailTotal += numberValue(v)
		}
	}
	total = maxInt(total, detailTotal)
	nested := 0
	for _, key := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if details := objectField(u, key); details != nil {
			nested = maxInt(nested, firstNumberField(details,
				"cache_creation_tokens",
				"cache_creation_input_tokens",
				"cache_write_tokens",
				"cache_write_input_tokens",
				"input_cache_write_tokens",
				"created_tokens",
			))
		}
	}
	if nested > 0 {
		return maxInt(total, nested), true
	}
	return total, directIncluded
}

// reasoningTokens extracts the reasoning/thinking token count reported by
// upstream. OpenAI-style providers expose it as
// `completion_tokens_details.reasoning_tokens`; some providers emit it at the
// top level. It is already included in completion_tokens by the upstream, so
// it is tracked separately for visibility and not added to totals.
func reasoningTokens(u map[string]any) int {
	direct := firstNumberField(u,
		"reasoning_tokens",
		"reasoning",
		"thinking_tokens",
		"reasoning_output_tokens",
	)
	if direct > 0 {
		return direct
	}
	for _, key := range []string{"completion_tokens_details", "output_tokens_details"} {
		if details := objectField(u, key); details != nil {
			if n := firstNumberField(details,
				"reasoning_tokens",
				"reasoning",
				"thinking_tokens",
				"reasoning_output_tokens",
			); n > 0 {
				return n
			}
		}
	}
	return 0
}

func numberField(m map[string]any, key string) int {
	return numberValue(m[key])
}

func firstNumberField(m map[string]any, keys ...string) int {
	n, _ := firstNumberFieldWithKey(m, keys...)
	return n
}

func firstNumberFieldWithKey(m map[string]any, keys ...string) (int, string) {
	for _, key := range keys {
		if n := numberField(m, key); n > 0 {
			return n, key
		}
	}
	return 0, ""
}

func objectField(m map[string]any, key string) map[string]any {
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return nil
}

func numberValue(v any) int {
	switch value := v.(type) {
	case float64:
		return int(value)
	case int:
		return value
	case int64:
		return int(value)
	case json.Number:
		n, _ := value.Int64()
		return int(n)
	case string:
		n, _ := strconv.ParseFloat(strings.TrimSpace(value), 64)
		return int(n)
	default:
		return 0
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func estimateUsageCost(route config.ModelRoute, usage *usageAccounting) float64 {
	if usage == nil || route.Pricing == nil {
		return 0
	}
	pricing := usagePricing(route)
	inputTokens := usage.InputTokens
	cost := float64(inputTokens)*pricing.Prompt +
		float64(usage.OutputTokens)*pricing.Completion +
		float64(usage.CacheReadTokens)*pricing.CacheRead
	if cost <= 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
		return 0
	}
	return cost
}

type pricingSnapshot struct {
	Prompt        float64
	Completion    float64
	CacheRead     float64
	CacheCreation float64
}

func usagePricing(route config.ModelRoute) pricingSnapshot {
	if route.Pricing == nil {
		return pricingSnapshot{}
	}
	return pricingSnapshot{
		Prompt:        priceField(route.Pricing, "prompt"),
		Completion:    priceField(route.Pricing, "completion"),
		CacheRead:     priceField(route.Pricing, "input_cache_read", "cache_read", "prompt_cache_read"),
		CacheCreation: priceField(route.Pricing, "input_cache_write", "cache_write", "prompt_cache_write", "input_cache_creation"),
	}
}

func priceField(pricing map[string]string, keys ...string) float64 {
	for _, key := range keys {
		raw := strings.TrimSpace(pricing[key])
		if raw == "" {
			continue
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v < 0 {
			continue
		}
		return v
	}
	return 0
}
