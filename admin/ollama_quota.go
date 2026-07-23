package admin

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	xhtml "golang.org/x/net/html"

	"github.com/xb0or/opencode-GO/upstream"
)

const maxOllamaSettingsBody = 2 << 20

// OllamaQuotaResponse contains the usage information rendered by
// ollama.com/settings. Ollama currently renders this data into HTML instead
// of exposing a stable JSON usage endpoint.
type OllamaQuotaResponse struct {
	Plan    string              `json:"plan,omitempty"`
	Session *OllamaUsageSection `json:"session,omitempty"`
	Weekly  *OllamaUsageSection `json:"weekly,omitempty"`
}

// OllamaUsageSection is intentionally tolerant: the settings page has no
// documented JSON schema, so fields that cannot be identified remain empty
// while Detail preserves a compact, human-readable section summary.
type OllamaUsageSection struct {
	Label         string  `json:"label"`
	Used          string  `json:"used,omitempty"`
	Limit         string  `json:"limit,omitempty"`
	UsagePercent  *float64 `json:"usagePercent,omitempty"`
	Requests      string  `json:"requests,omitempty"`
	Model         string  `json:"model,omitempty"`
	ResetAt       string  `json:"resetAt,omitempty"`
	Balance       string  `json:"balance,omitempty"`
	Detail        string  `json:"detail,omitempty"`
}

var (
	ollamaPercentPattern = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*%`)
	ollamaUsedLimitPattern = regexp.MustCompile(`(?i)([0-9][0-9,\.]*\s*(?:[KMGTP]i?B?|[KMG])?)\s*(?:/|\bof\b)\s*([0-9][0-9,\.]*\s*(?:[KMGTP]i?B?|[KMG])?)`)
	ollamaRequestsPattern = regexp.MustCompile(`(?i)([0-9][0-9,]*)\s*(?:requests?|reqs?)`)
	ollamaModelPattern = regexp.MustCompile(`(?i)\bmodels?\s*[:\-]\s*([^|]+)`)
	ollamaResetPattern = regexp.MustCompile(`(?i)\b(?:reset(?:s)?|renews?|renewal)\s*(?:in|at|on|:)?\s*([^.;|]+)`)
	ollamaBalancePattern = regexp.MustCompile(`(?i)\b(?:balance|credit|remaining)\s*[:\-]?\s*([$€£]?[0-9][0-9,\.]*\s*[KMG]?)`)
)

var ollamaQuotaSectionLabels = []string{
	"session usage",
	"weekly usage",
	"cloud plan",
	// Keep Extra usage as a boundary so its page content cannot bleed into
	// the Weekly usage section; it is intentionally not parsed or returned.
	"extra usage",
}

// normalizeOllamaCookie preserves all browser cookie pairs. Unlike the
// OpenCode quota endpoint, Ollama's settings page authentication cookie name
// is not confirmed by the redacted capture, so reducing it to auth= would
// discard the actual session credential.
func normalizeOllamaCookie(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	for _, prefix := range []string{"Cookie:", "cookie:", "Set-Cookie:", "set-cookie:"} {
		s = strings.TrimSpace(strings.TrimPrefix(s, prefix))
	}

	parts := strings.Split(s, ";")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(name) == "" {
			continue
		}
		name = strings.TrimSpace(name)
		if isCookieAttribute(name) {
			continue
		}
		out = append(out, name+"="+strings.TrimSpace(value))
	}
	if len(out) > 0 {
		return strings.Join(out, "; ")
	}
	return s
}

func normalizeKeyCookie(group, raw string) string {
	if strings.EqualFold(strings.TrimSpace(group), "ollama") {
		return normalizeOllamaCookie(raw)
	}
	return normalizeAuthCookie(raw)
}

func isCookieAttribute(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "path", "domain", "expires", "max-age", "secure", "httponly", "samesite":
		return true
	default:
		return false
	}
}

func fetchOllamaQuota(cookie, proxyURL string) (*OllamaQuotaResponse, error) {
	cookie = normalizeOllamaCookie(cookie)
	if cookie == "" {
		return nil, fmt.Errorf("ollama login cookie is empty")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://ollama.com/settings", nil)
	if err != nil {
		return nil, fmt.Errorf("build Ollama settings request: %w", err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Referer", "https://ollama.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36")

	client := upstream.NewClientForProxy(proxyURL)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request Ollama settings: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("Ollama session rejected with HTTP %d (cookie may be expired)", resp.StatusCode)
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		location := strings.ToLower(resp.Header.Get("Location"))
		if strings.Contains(location, "/login") || strings.Contains(location, "/sign-in") || strings.Contains(location, "/auth") {
			return nil, fmt.Errorf("Ollama session redirected to login (cookie may be expired)")
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Ollama settings returned HTTP %d", resp.StatusCode)
	}
	if resp.Request != nil {
		path := strings.ToLower(resp.Request.URL.Path)
		if strings.Contains(path, "/login") || strings.Contains(path, "/sign-in") || strings.Contains(path, "/auth") {
			return nil, fmt.Errorf("Ollama session redirected to login (cookie may be expired)")
		}
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxOllamaSettingsBody))
	if err != nil {
		return nil, fmt.Errorf("read Ollama settings: %w", err)
	}
	result, err := parseOllamaQuotaPage(raw)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func parseOllamaQuotaPage(raw []byte) (*OllamaQuotaResponse, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("Ollama settings page is empty")
	}
	text, err := ollamaVisibleText(raw)
	if err != nil {
		return nil, fmt.Errorf("parse Ollama settings HTML: %w", err)
	}
	text = normalizeOllamaText(text)
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "session usage") &&
		!strings.Contains(lower, "weekly usage") &&
		(strings.Contains(lower, "sign in") || strings.Contains(lower, "log in")) {
		return nil, fmt.Errorf("Ollama settings returned a login page (cookie may be expired)")
	}

	result := &OllamaQuotaResponse{}
	if section := ollamaSection(text, "cloud plan"); section != "" {
		result.Plan = cleanOllamaPlan(section)
	}
	if section := ollamaSection(text, "session usage"); section != "" {
		result.Session = parseOllamaUsageSection("Session usage", section)
	}
	if section := ollamaSection(text, "weekly usage"); section != "" {
		result.Weekly = parseOllamaUsageSection("Weekly usage", section)
	}
	if result.Session == nil && result.Weekly == nil {
		return nil, fmt.Errorf("Ollama settings page contains no recognized usage sections")
	}
	return result, nil
}

func ollamaVisibleText(raw []byte) (string, error) {
	root, err := xhtml.Parse(strings.NewReader(string(raw)))
	if err != nil {
		return "", err
	}
	var parts []string
	var walk func(*xhtml.Node)
	walk = func(node *xhtml.Node) {
		if node.Type == xhtml.ElementNode {
			switch strings.ToLower(node.Data) {
			case "script", "style", "noscript", "template":
				return
			}
		}
		if node.Type == xhtml.TextNode {
			if value := strings.TrimSpace(html.UnescapeString(node.Data)); value != "" {
				parts = append(parts, value)
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return strings.Join(parts, " "), nil
}

func normalizeOllamaText(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func ollamaSection(text, label string) string {
	lower := strings.ToLower(text)
	start := strings.Index(lower, strings.ToLower(label))
	if start < 0 {
		return ""
	}
	end := len(text)
	for _, nextLabel := range ollamaQuotaSectionLabels {
		if strings.EqualFold(nextLabel, label) {
			continue
		}
		if index := strings.Index(lower[start+len(label):], strings.ToLower(nextLabel)); index >= 0 {
			candidate := start + len(label) + index
			if candidate < end {
				end = candidate
			}
		}
	}
	return normalizeOllamaText(text[start+len(label) : end])
}

func cleanOllamaPlan(section string) string {
	section = normalizeOllamaText(section)
	if section == "" {
		return ""
	}
	return trimOllamaDetail(section)
}

func parseOllamaUsageSection(label, section string) *OllamaUsageSection {
	section = normalizeOllamaText(section)
	if section == "" {
		return nil
	}
	result := &OllamaUsageSection{Label: label, Detail: trimOllamaDetail(section)}
	if match := ollamaPercentPattern.FindStringSubmatch(section); len(match) == 2 {
		if value, err := strconv.ParseFloat(match[1], 64); err == nil {
			result.UsagePercent = &value
		}
	}
	if match := ollamaUsedLimitPattern.FindStringSubmatch(section); len(match) == 3 {
		result.Used = strings.TrimSpace(match[1])
		result.Limit = strings.TrimSpace(match[2])
		if result.UsagePercent == nil {
			used, usedOK := parseOllamaNumeric(result.Used)
			limit, limitOK := parseOllamaNumeric(result.Limit)
			if usedOK && limitOK && limit > 0 {
				value := used / limit * 100
				result.UsagePercent = &value
			}
		}
	}
	if match := ollamaRequestsPattern.FindStringSubmatch(section); len(match) == 2 {
		result.Requests = strings.TrimSpace(match[1])
	}
	if match := ollamaModelPattern.FindStringSubmatch(section); len(match) == 2 {
		result.Model = trimOllamaDetail(match[1])
	}
	if match := ollamaResetPattern.FindStringSubmatch(section); len(match) == 2 {
		result.ResetAt = trimOllamaDetail(match[1])
	}
	if match := ollamaBalancePattern.FindStringSubmatch(section); len(match) == 2 {
		result.Balance = strings.TrimSpace(match[1])
	}
	return result
}

func parseOllamaNumeric(value string) (float64, bool) {
	value = strings.ReplaceAll(strings.TrimSpace(value), ",", "")
	for _, suffix := range []string{"TiB", "GiB", "MiB", "KiB", "TB", "GB", "MB", "KB", "T", "G", "M", "K"} {
		if strings.HasSuffix(strings.ToUpper(value), strings.ToUpper(suffix)) {
			factor := 1.0
			switch strings.ToUpper(suffix) {
			case "K", "KB", "KIB":
				factor = 1e3
			case "M", "MB", "MIB":
				factor = 1e6
			case "G", "GB", "GIB":
				factor = 1e9
			case "T", "TB", "TIB":
				factor = 1e12
			}
			n, err := strconv.ParseFloat(strings.TrimSpace(value[:len(value)-len(suffix)]), 64)
			return n * factor, err == nil
		}
	}
	n, err := strconv.ParseFloat(value, 64)
	return n, err == nil
}

func trimOllamaDetail(value string) string {
	value = normalizeOllamaText(value)
	if len(value) <= 512 {
		return value
	}
	return value[:509] + "..."
}
