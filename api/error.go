package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
)

// previewBody returns a compact, redacted, length-limited preview of an
// upstream response body for inclusion in error logs. It collapses whitespace
// and strips control characters so HTML error pages are readable.
func previewBody(body []byte) string {
	const maxPreview = 512
	s := strings.ToValidUTF8(string(body), "�")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxPreview {
		s = s[:maxPreview] + "…"
	}
	return s
}

// shouldMarkUpstreamFailure reports whether a response status should count as a
// key failure and trigger cooldown bookkeeping.
func shouldMarkUpstreamFailure(status int) bool {
	switch status {
	case http.StatusPaymentRequired, http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		return true
	default:
		return status >= 500
	}
}

// shouldRetryWithNextKey reports whether a failed upstream response should be
// retried with another available key before returning it to the client.
// Only key-level failures (auth, rate limit, 5xx) are retried with a different key.
func shouldRetryWithNextKey(status int) bool {
	return shouldMarkUpstreamFailure(status)
}

// isClientErrorNonRetryable reports whether the HTTP status represents a
// client-side error (4xx) that should NOT trigger a key retry or upstream
// failover. These errors indicate the request itself is invalid and
// switching keys or providers will not help — the same error will likely
// recur, or worse, a different provider might accept the request with
// different semantics, producing incorrect results.
//
// Examples: 400 (bad request), 404 (model not found), 409 (conflict),
// 413 (payload too large), 415 (unsupported media type), 422 (unprocessable).
func isClientErrorNonRetryable(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict,
		http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType,
		http.StatusUnprocessableEntity, http.StatusMethodNotAllowed:
		return true
	default:
		return false
	}
}

// upstreamErrorType maps an upstream HTTP status to an OpenAI-style error type
// that is safe to return to the client.
func upstreamErrorType(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "permission_denied"
	case status == http.StatusRequestTimeout, status == http.StatusGatewayTimeout:
		return "upstream_timeout"
	case status >= 500:
		return "upstream_error"
	default:
		return "upstream_error"
	}
}

// genericUpstreamMessage returns a client-safe error message that does not
// expose upstream provider/channel details (e.g. "Error from provider
// (DeepSeek)"). The raw upstream error is still recorded in the admin usage
// log via summarizeUpstreamError for debugging.
func genericUpstreamMessage(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "upstream rate limit reached, please retry later"
	case status == http.StatusUnauthorized:
		return "upstream authentication failed"
	case status == http.StatusForbidden:
		return "upstream access denied"
	case status == http.StatusRequestTimeout, status == http.StatusGatewayTimeout:
		return "upstream request timed out"
	case status >= 500:
		return "upstream service error"
	default:
		return "upstream request failed"
	}
}

const statusClientClosedRequest = 499

func classifyProxyContextError(err error) (int, string, string, bool) {
	if err == nil {
		return 0, "", "", false
	}
	if errors.Is(err, context.Canceled) {
		return statusClientClosedRequest, "client_closed_request", "client canceled request", true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, "upstream_timeout", "upstream request timed out", true
	}
	return 0, "", "", false
}

func copyErrString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

const maxUsageErrorLen = 2048

var sensitiveErrorPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._~+\-/=]+`),
	regexp.MustCompile(`(?i)((?:api[_-]?key|token|secret|credential)["'\s:=]+)([^"'\s,}]+)`),
}

// summarizeUpstreamError extracts a compact, redacted error message from an
// upstream error response body for admin usage logs.
func summarizeUpstreamError(status int, body []byte) string {
	msg := extractUpstreamErrorMessage(body)
	if msg == "" {
		msg = strings.TrimSpace(strings.ToValidUTF8(string(body), "�"))
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if msg == "" {
		msg = http.StatusText(status)
	}
	return trimUsageError(fmt.Sprintf("upstream returned HTTP %d: %s", status, redactUsageError(msg)))
}

func extractUpstreamErrorMessage(body []byte) string {
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	if msg := findErrorMessage(payload); msg != "" {
		return msg
	}
	compact, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(compact)
}

func findErrorMessage(v any) string {
	switch x := v.(type) {
	case map[string]any:
		for _, key := range []string{"error", "message", "detail", "error_description", "code", "type"} {
			if val, ok := x[key]; ok {
				if msg := findErrorMessage(val); msg != "" {
					return msg
				}
			}
		}
	case []any:
		var parts []string
		for _, item := range x {
			if msg := findErrorMessage(item); msg != "" {
				parts = append(parts, msg)
			}
		}
		return strings.Join(parts, "; ")
	case string:
		return strings.TrimSpace(x)
	case float64, bool, nil:
		return fmt.Sprint(x)
	}
	return ""
}

func redactUsageError(s string) string {
	for _, pattern := range sensitiveErrorPatterns {
		s = pattern.ReplaceAllString(s, "${1}[redacted]")
	}
	return s
}

func trimUsageError(s string) string {
	if len(s) <= maxUsageErrorLen {
		return s
	}
	return s[:maxUsageErrorLen-1] + "…"
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
