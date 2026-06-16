package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opencode-sw/gateway/config"
	"github.com/opencode-sw/gateway/pool"
	"github.com/opencode-sw/gateway/protocol"
	"github.com/opencode-sw/gateway/store"
	"github.com/opencode-sw/gateway/upstream"
)

// proxyChat handles POST /v1/chat/completions (OpenAI Chat Completions).
func proxyChat(p *pool.Picker) gin.HandlerFunc {
	return func(c *gin.Context) {
		proxyRequest(c, p, config.ProtocolChat, "/v1/chat/completions")
	}
}

// proxyMessages handles POST /v1/messages (Anthropic Messages).
func proxyMessages(p *pool.Picker) gin.HandlerFunc {
	return func(c *gin.Context) {
		proxyRequest(c, p, config.ProtocolMessages, "/v1/messages")
	}
}

// proxyResponses handles POST /v1/responses (OpenAI Responses API).
func proxyResponses(p *pool.Picker) gin.HandlerFunc {
	return func(c *gin.Context) {
		proxyRequest(c, p, config.ProtocolResponses, "/v1/responses")
	}
}

// proxyRequest is the shared handler with full cross-protocol conversion.
//
// Flow:
//  1. Read & parse the client body to find the requested model.
//  2. Resolve the model route.
//  3. If the inbound protocol differs from the upstream protocol, convert the
//     request body through the IR intermediate representation.
//  4. Pick a KEY from the model's group.
//  5. Forward to the upstream with the KEY injected.
//  6. If cross-protocol: buffer the upstream response and convert it back.
//     If same-protocol: stream verbatim.
//  7. Bookkeeping: mark the KEY success/failure, write a usage log.
func proxyRequest(c *gin.Context, p *pool.Picker, inbound config.Protocol, upstreamPath string) {
	start := time.Now()

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}
	_ = c.Request.Body.Close()

	head := inspectAndMapRequestBody(c.Request.URL.Path, body)
	upstreamBody := head.Body

	route, routed := config.LookupModel(head.Model)
	if !routed {
		route = passthroughRoute(head.Model, inbound)
	}

	if tokAny, exists := c.Get("token"); exists {
		if tok, ok := tokAny.(*store.Token); ok && !pool.GroupAllowed(tok, route.Group) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": gin.H{
					"type":    "permission_denied",
					"message": "this token is not allowed to use group: " + route.Group,
				},
			})
			return
		}
	}

	// Stash group for logging/debugging after the model route is known.
	c.Set("group", route.Group)

	// Cross-protocol request conversion.
	// If the inbound protocol differs from the upstream model's protocol,
	// convert the request body through the IR.
	upstreamProto := route.Protocol
	crossProtocol := routed && head.HasModel && inbound != upstreamProto

	if crossProtocol {
		converted, err := protocol.ConvertRequest(inbound, upstreamProto, upstreamBody)
		if err != nil {
			writeOpenAIError(c, http.StatusBadRequest, "conversion_error",
				fmt.Sprintf("failed to convert request from %s to %s: %v", inbound, upstreamProto, err))
			return
		}
		upstreamBody = converted
		// The converted body may have a different stream field; re-parse.
		var h struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(upstreamBody, &h)
		head.Stream = h.Stream
	}

	key, err := p.Pick(route.Group)
	if err != nil {
		writeOpenAIError(c, http.StatusServiceUnavailable, "no_upstream_key_error",
			"no available upstream key for group "+route.Group)
		return
	}

	// Rewrite the model field in the body to the upstream's real model id.
	rewritten, ok := rewriteModel(upstreamBody, route.RealModel)
	if ok {
		upstreamBody = rewritten
	}

	// Determine the upstream endpoint path.  If the client called /v1/messages
	// but the model speaks "chat" upstream, we must hit /v1/chat/completions.
	actualUpstreamPath := upstreamPath
	if crossProtocol {
		actualUpstreamPath = upstreamPathFor(upstreamProto)
	}

	baseURL := config.BaseURLFor(route.Upstream)
	target := baseURL + actualUpstreamPath

	ctx, cancel := context.WithTimeout(c.Request.Context(), upstream.Timeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, c.Request.Method, target, bytes.NewReader(upstreamBody))
	if err != nil {
		markAndLog(c, p, key, route, inbound, http.StatusInternalServerError, start, head.Stream, err.Error())
		writeOpenAIError(c, http.StatusInternalServerError, "internal_error", "failed to build upstream request")
		return
	}
	copyForwardHeaders(req.Header, c.Request.Header)
	setContentLength(req, len(upstreamBody))
	injectUpstreamAuth(req.Header, key.Value)

	upstreamClient := upstream.NewClientForProxy(key.ProxyURL)
	resp, err := upstreamClient.Do(req)
	if err != nil {
		p.MarkFailure(key.ID)
		markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, head.Stream, err.Error())
		writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "failed to reach upstream: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if crossProtocol {
		// Buffer the upstream response and convert it back to the inbound protocol.
		proxyCrossProtocolResponse(c, resp, head.Stream, inbound, upstreamProto, p, key, route, start)
	} else {
		// Same-protocol: stream verbatim.
		proxySameProtocolResponse(c, resp, head.Stream, p, key, route, inbound, start)
	}
}

// proxySameProtocolResponse streams the upstream response verbatim to the client.
func proxySameProtocolResponse(c *gin.Context, resp *http.Response, stream bool,
	p *pool.Picker, key *store.Key, route config.ModelRoute, inbound config.Protocol, start time.Time) {

	for k, vs := range resp.Header {
		for _, v := range vs {
			if isHopHeader(k) {
				continue
			}
			c.Writer.Header().Add(k, v)
		}
	}
	c.Writer.WriteHeader(resp.StatusCode)
	_, copyErr := io.Copy(c.Writer, resp.Body)

	if resp.StatusCode < 400 && copyErr == nil {
		p.MarkSuccess(key.ID)
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, "")
	} else if shouldMarkUpstreamFailure(resp.StatusCode) {
		p.MarkFailure(key.ID)
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, "")
	} else {
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, "")
	}
}

// proxyCrossProtocolResponse buffers the upstream response, converts it through
// the IR, and writes the converted response to the client.
func proxyCrossProtocolResponse(c *gin.Context, resp *http.Response, stream bool,
	inbound, upstreamProto config.Protocol,
	p *pool.Picker, key *store.Key, route config.ModelRoute, start time.Time) {

	if resp.StatusCode >= 400 {
		// Don't convert error responses; pass them through.
		for k, vs := range resp.Header {
			for _, v := range vs {
				if isHopHeader(k) {
					continue
				}
				c.Writer.Header().Add(k, v)
			}
		}
		c.Writer.WriteHeader(resp.StatusCode)
		io.Copy(c.Writer, resp.Body)
		if shouldMarkUpstreamFailure(resp.StatusCode) {
			p.MarkFailure(key.ID)
		}
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, stream, "")
		return
	}

	if stream {
		// Streaming cross-protocol: buffer the full upstream SSE stream,
		// then re-emit in the target protocol format.
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.WriteHeader(http.StatusOK)

		err := protocol.StreamConverter(c.Writer, resp.Body, upstreamProto, inbound)
		if err != nil {
			p.MarkFailure(key.ID)
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, true, err.Error())
			return
		}
		p.MarkSuccess(key.ID)
		markAndLog(c, p, key, route, inbound, http.StatusOK, start, true, "")
	} else {
		// Non-streaming: buffer, convert, write.
		upstreamBody, err := io.ReadAll(resp.Body)
		if err != nil {
			p.MarkFailure(key.ID)
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, false, err.Error())
			writeOpenAIError(c, http.StatusBadGateway, "upstream_error", "failed to read upstream response")
			return
		}

		converted, err := protocol.ConvertResponse(upstreamProto, inbound, upstreamBody)
		if err != nil {
			p.MarkFailure(key.ID)
			markAndLog(c, p, key, route, inbound, http.StatusBadGateway, start, false, err.Error())
			writeOpenAIError(c, http.StatusBadGateway, "conversion_error",
				fmt.Sprintf("failed to convert response from %s to %s: %v", upstreamProto, inbound, err))
			return
		}

		// Set appropriate content type for the target protocol.
		switch inbound {
		case config.ProtocolMessages:
			c.Writer.Header().Set("Content-Type", "application/json")
		default:
			c.Writer.Header().Set("Content-Type", "application/json")
		}
		c.Writer.WriteHeader(resp.StatusCode)
		c.Writer.Write(converted)

		p.MarkSuccess(key.ID)
		markAndLog(c, p, key, route, inbound, resp.StatusCode, start, false, "")
	}
}

// upstreamPathFor returns the canonical upstream path for a protocol.
func upstreamPathFor(proto config.Protocol) string {
	switch proto {
	case config.ProtocolMessages:
		return "/v1/messages"
	case config.ProtocolResponses:
		return "/v1/responses"
	default:
		return "/v1/chat/completions"
	}
}

type requestHead struct {
	Body     []byte
	Model    string
	Stream   bool
	Parsed   bool
	HasModel bool
	Mapped   bool
}

// inspectAndMapRequestBody parses a JSON request body just enough to find
// top-level "model" and "stream". If a configured model mapping matches, it
// rewrites the JSON body and returns the mapped model. Invalid JSON or missing
// model is logged and forwarded unchanged.
func inspectAndMapRequestBody(path string, body []byte) requestHead {
	head := requestHead{Body: body}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		log.Printf("warn: model mapping skipped for %s: request body is not valid JSON: %v", path, err)
		return head
	}
	head.Parsed = true

	if stream, ok := m["stream"].(bool); ok {
		head.Stream = stream
	}

	model, ok := m["model"].(string)
	if !ok || strings.TrimSpace(model) == "" {
		log.Printf("warn: model mapping skipped for %s: request JSON has no string model field", path)
		return head
	}
	head.Model = model
	head.HasModel = true

	mapped, ok := config.LookupModelMapping(model)
	if !ok {
		return head
	}
	m["model"] = mapped
	out, err := json.Marshal(m)
	if err != nil {
		log.Printf("warn: model mapping %q -> %q skipped for %s: remarshal failed: %v", model, mapped, path, err)
		return head
	}
	head.Body = out
	head.Model = mapped
	head.Mapped = true
	log.Printf("model mapping applied for %s: %q -> %q", path, model, mapped)
	return head
}

func passthroughRoute(model string, inbound config.Protocol) config.ModelRoute {
	id := strings.TrimSpace(model)
	if id == "" {
		id = "passthrough"
	}
	return config.ModelRoute{
		ID:        id,
		Name:      id,
		Upstream:  config.UpstreamGo,
		Protocol:  inbound,
		RealModel: model,
		Group:     "go",
	}
}

// rewriteModel returns body with the top-level "model" field replaced. It
// re-marshals compact JSON; on any failure the original body is returned with
// ok=false so the caller keeps the original (model name may be a prefix match
// upstream, but that is acceptable degradation).
func rewriteModel(body []byte, realModel string) ([]byte, bool) {
	if realModel == "" {
		return body, false
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body, false
	}
	m["model"] = realModel
	out, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return out, true
}

// setContentLength pins the outbound request length after any body rewrite.
func setContentLength(req *http.Request, n int) {
	req.ContentLength = int64(n)
	req.Header.Set("Content-Length", strconv.Itoa(n))
}

// copyForwardHeaders forwards client headers to the upstream request while
// stripping hop-by-hop and client auth headers.
func copyForwardHeaders(dst, src http.Header) {
	skip := map[string]bool{
		"Authorization":  true,
		"X-Api-Key":      true,
		"Api-Key":        true,
		"Content-Length": true,
		"Host":           true,
	}
	for k, vs := range src {
		if skip[http.CanonicalHeaderKey(k)] || isHopHeader(k) {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// injectUpstreamAuth attaches the selected upstream key to the outbound
// request using both Authorization and X-Api-Key for compatibility.
func injectUpstreamAuth(h http.Header, keyValue string) {
	keyValue = strings.TrimSpace(keyValue)
	if keyValue == "" {
		return
	}
	h.Set("Authorization", "Bearer "+keyValue)
	h.Set("X-Api-Key", keyValue)
}

// shouldMarkUpstreamFailure reports whether a response status should count as a
// key failure and trigger cooldown bookkeeping.
func shouldMarkUpstreamFailure(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		return true
	default:
		return status >= 500
	}
}

// isHopHeader reports whether a header should be stripped on proxy hop.
func isHopHeader(k string) bool {
	switch strings.ToLower(k) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

// markAndLog writes a usage log row. It never blocks the response path on DB errors.
func markAndLog(c *gin.Context, p *pool.Picker, key *store.Key, route config.ModelRoute,
	proto config.Protocol, status int, start time.Time, stream bool, errMsg string) {
	var tokenID uint
	var tokenName string
	if tokAny, exists := c.Get("token"); exists {
		if tok, ok := tokAny.(*store.Token); ok {
			tokenID = tok.ID
			tokenName = tok.Name
		}
	}
	entry := store.UsageLog{
		TokenID:    tokenID,
		TokenName:  tokenName,
		KeyID:      key.ID,
		Model:      route.ID,
		Protocol:   string(proto),
		StatusCode: status,
		DurationMs: time.Since(start).Milliseconds(),
		Stream:     stream,
		Error:      errMsg,
	}
	_ = store.DB().Create(&entry).Error
}

// writeOpenAIError emits an OpenAI-style error envelope.
func writeOpenAIError(c *gin.Context, status int, errType, message string) {
	c.JSON(status, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}
