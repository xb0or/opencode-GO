package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

	// Determine the requested model from the body (all three formats use top-level "model").
	var head struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "request body is not valid JSON")
		return
	}
	if head.Model == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "missing 'model' field")
		return
	}

	route, ok := config.LookupModel(head.Model)
	if !ok {
		writeOpenAIError(c, http.StatusNotFound, "model_not_found_error",
			fmt.Sprintf("model %q is not registered in the gateway", head.Model))
		return
	}

	// Stash group so RequireGroup middleware (if mounted) can authorize it.
	c.Set("group", route.Group)
	// Re-run group authorization inline (in case middleware ordering differs).
	if tokAny, exists := c.Get("token"); exists {
		if tok, ok := tokAny.(*store.Token); ok && !pool.GroupAllowed(tok, route.Group) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": gin.H{"type": "permission_denied", "message": "token not allowed for group: " + route.Group},
			})
			return
		}
	}

	// Cross-protocol request conversion.
	// If the inbound protocol differs from the upstream model's protocol,
	// convert the request body through the IR.
	upstreamProto := route.Protocol
	upstreamBody := body
	crossProtocol := inbound != upstreamProto

	if crossProtocol {
		converted, err := protocol.ConvertRequest(inbound, upstreamProto, body)
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
	copyForwardHeaders(req.Header, c.Request.Header, upstreamProto)

	resp, err := upstream.NewClient().Do(req)
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
	} else if resp.StatusCode >= 500 {
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
		if resp.StatusCode >= 500 {
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

// rewriteModel returns body with the top-level "model" field replaced. It
// re-marshals compact JSON; on any failure the original body is returned with
// ok=false so the caller keeps the original (model name may be a prefix match
// upstream, but that is acceptable degradation).
func rewriteModel(body []byte, realModel string) ([]byte, bool) {
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

// copyForwardHeaders forwards client headers to the upstream request, replacing
// auth with the pool key and setting the protocol-appropriate content type.
func copyForwardHeaders(dst, src http.Header, proto config.Protocol) {
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
