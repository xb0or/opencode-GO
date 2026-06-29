package admin

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"

	xhtml "golang.org/x/net/html"
)

func requestPage(client *http.Client, method, target string, values url.Values, referer string) (*httpPage, error) {
	var body io.Reader
	if values != nil {
		body = strings.NewReader(values.Encode())
	}
	req, err := http.NewRequest(method, target, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", adminImportUserAgent)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("Accept", "*/*")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	return &httpPage{URL: resp.Request.URL.String(), Header: resp.Header, Body: string(raw), Status: resp.StatusCode}, nil
}

func parseLoginForms(raw string) ([]loginForm, error) {
	root, err := xhtml.Parse(strings.NewReader(raw))
	if err != nil {
		return nil, err
	}
	var forms []loginForm
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode && n.Data == "form" {
			form := loginForm{Action: attr(n, "action"), Method: attr(n, "method"), Values: url.Values{}}
			collectFormValues(n, form.Values)
			forms = append(forms, form)
			return
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return forms, nil
}

func findLoginForm(raw, actionSubstring string) (loginForm, error) {
	forms, err := parseLoginForms(raw)
	if err != nil {
		return loginForm{}, err
	}
	for _, form := range forms {
		if strings.Contains(form.Action, actionSubstring) {
			return form, nil
		}
	}
	return loginForm{}, fmt.Errorf("form not found: %s", actionSubstring)
}

func collectFormValues(n *xhtml.Node, values url.Values) {
	if n.Type == xhtml.ElementNode && (n.Data == "input" || n.Data == "button") {
		name := attr(n, "name")
		if name != "" {
			typ := strings.ToLower(attr(n, "type"))
			if (typ == "checkbox" || typ == "radio") && attr(n, "checked") == "" {
				return
			}
			values.Set(name, attr(n, "value"))
		}
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		collectFormValues(child, values)
	}
}

func attr(n *xhtml.Node, name string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, name) {
			return a.Val
		}
	}
	return ""
}

func githubOTP(body githubImportRequest) (string, error) {
	if strings.TrimSpace(body.OTP) != "" {
		return strings.TrimSpace(body.OTP), nil
	}
	if strings.TrimSpace(body.TOTPSecret) != "" {
		return totpCode(body.TOTPSecret)
	}
	return "", fmt.Errorf("GitHub requires a 2FA code; fill OTP or TOTP secret and retry")
}

func totpCode(secret string) (string, error) {
	secret = extractTOTPSecret(secret)
	if secret == "" {
		return "", fmt.Errorf("TOTP secret is empty")
	}
	secret += strings.Repeat("=", (8-len(secret)%8)%8)
	key, err := base32.StdEncoding.DecodeString(secret)
	if err != nil {
		return "", fmt.Errorf("invalid TOTP secret")
	}
	counter := uint64(time.Now().Unix() / 30)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	code := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", code%1000000), nil
}

func extractTOTPSecret(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "otpauth://") {
		if parsed, err := url.Parse(value); err == nil {
			value = parsed.Query().Get("secret")
		}
	}
	value = strings.ToUpper(strings.ReplaceAll(value, " ", ""))
	return strings.Map(func(r rune) rune {
		if (r >= 'A' && r <= 'Z') || (r >= '2' && r <= '7') {
			return r
		}
		return -1
	}, value)
}

func opencodeAuthCookie(jar *cookiejar.Jar) string {
	for _, rawURL := range []string{opencodeBaseURL, opencodeAuthURL} {
		parsed, _ := url.Parse(rawURL)
		for _, cookie := range jar.Cookies(parsed) {
			if cookie.Name == "auth" && cookie.Value != "" {
				return normalizeAuthCookie("auth=" + cookie.Value)
			}
		}
	}
	return ""
}

func solidArgsJSON(workspaceID string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"t": map[string]any{
			"t": 9,
			"i": 0,
			"l": 1,
			"a": []map[string]any{{"t": 1, "s": workspaceID}},
			"o": 0,
		},
		"f": 31,
		"m": []any{},
	})
	return b
}

func extractOpenCodeKeyValues(raw string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(value string) {
		value = decodeJSString(value)
		if !looksLikeOpenCodeKey(value) || seen[value] {
			return
		}
		seen[value] = true
		out = append(out, value)
	}
	for _, match := range apiKeyFieldPattern.FindAllStringSubmatch(raw, -1) {
		if len(match) == 2 {
			add(match[1])
		}
	}
	for _, match := range apiKeyTokenPattern.FindAllString(raw, -1) {
		add(match)
	}
	return out
}

func decodeJSString(value string) string {
	value = html.UnescapeString(strings.TrimSpace(value))
	if decoded, err := strconv.Unquote(`"` + value + `"`); err == nil {
		return decoded
	}
	return value
}

func looksLikeOpenCodeKey(value string) bool {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "auth=") || strings.HasPrefix(lower, "wrk_") || strings.Contains(lower, "server-fn") {
		return false
	}
	return strings.HasPrefix(value, "sk-") || strings.HasPrefix(value, "opencode_")
}

func absoluteURL(base, target string) string {
	u, err := url.Parse(target)
	if err == nil && u.IsAbs() {
		return target
	}
	b, err := url.Parse(base)
	if err != nil {
		return target
	}
	return b.ResolveReference(u).String()
}
