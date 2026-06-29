package admin

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/xb0or/opencode-GO/store"
	"gorm.io/gorm"
)

func newLoginHTTPClients(proxyRaw string) (*http.Client, *http.Client, *cookiejar.Jar, error) {
	jar, _ := cookiejar.New(nil)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if strings.TrimSpace(proxyRaw) != "" {
		proxyURL, err := url.Parse(strings.TrimSpace(proxyRaw))
		if err != nil {
			return nil, nil, nil, fmt.Errorf("invalid proxy URL: %w", err)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	follow := &http.Client{Timeout: 60 * time.Second, Jar: jar, Transport: transport}
	noRedirect := &http.Client{
		Timeout:   60 * time.Second,
		Jar:       jar,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return follow, noRedirect, jar, nil
}

func startGitHubOAuth(client *http.Client) (string, error) {
	page, err := requestPage(client, http.MethodGet, opencodeBaseURL+"/auth", nil, "")
	if err != nil {
		return "", err
	}
	location := page.Header.Get("Location")
	if location == "" {
		return "", fmt.Errorf("opencode auth did not redirect")
	}
	page, err = requestPage(client, http.MethodGet, absoluteURL(page.URL, location), nil, page.URL)
	if err != nil {
		return "", err
	}
	location = page.Header.Get("Location")
	if location == "" {
		return "", fmt.Errorf("opencode authorize did not redirect")
	}
	if _, err = requestPage(client, http.MethodGet, absoluteURL(page.URL, location), nil, page.URL); err != nil {
		return "", err
	}
	page, err = requestPage(client, http.MethodGet, opencodeAuthURL+"/github/authorize", nil, "")
	if err != nil {
		return "", err
	}
	location = page.Header.Get("Location")
	if location == "" {
		return "", fmt.Errorf("GitHub OAuth authorize URL was not returned")
	}
	return absoluteURL(page.URL, location), nil
}

func submitGitHubLogin(client *http.Client, page *httpPage, username, password string) (*httpPage, error) {
	form, err := findLoginForm(page.Body, "/session")
	if err != nil {
		return nil, err
	}
	form.Values.Set("login", username)
	form.Values.Set("password", password)
	form.Values.Set("javascript-support", "true")
	form.Values.Set("webauthn-support", "supported")
	form.Values.Set("webauthn-iuvpaa-support", "supported")
	if form.Values.Get("commit") == "" {
		form.Values.Set("commit", "Sign in")
	}
	return requestPage(client, http.MethodPost, absoluteURL(page.URL, form.Action), form.Values, page.URL)
}

func advanceGitHubChallenges(client *http.Client, page *httpPage, body githubImportRequest) (*httpPage, error) {
	location := page.Header.Get("Location")
	if strings.Contains(location, "/sessions/two-factor") {
		twoFactorPage, err := requestPage(client, http.MethodGet, absoluteURL(page.URL, location), nil, page.URL)
		if err != nil {
			return nil, err
		}
		otp, err := githubOTP(body)
		if err != nil {
			return nil, err
		}
		form, err := findLoginForm(twoFactorPage.Body, "/sessions/two-factor")
		if err != nil {
			return nil, err
		}
		form.Values.Set("app_otp", otp)
		page, err = requestPage(client, http.MethodPost, absoluteURL(twoFactorPage.URL, form.Action), form.Values, twoFactorPage.URL)
		if err != nil {
			return nil, err
		}
		location = page.Header.Get("Location")
	}
	if strings.Contains(location, "/sessions/trusted-device") {
		trustedPage, err := requestPage(client, http.MethodGet, absoluteURL(page.URL, location), nil, page.URL)
		if err != nil {
			return nil, err
		}
		if strings.Contains(trustedPage.Body, "/sessions/trusted-device/decline") {
			form, err := findLoginForm(trustedPage.Body, "/sessions/trusted-device/decline")
			if err != nil {
				return nil, err
			}
			return requestPage(client, http.MethodPost, absoluteURL(trustedPage.URL, form.Action), form.Values, trustedPage.URL)
		}
		return trustedPage, nil
	}
	return page, nil
}

func advanceGitHubOAuth(client *http.Client, page *httpPage) (string, error) {
	for i := 0; i < 20; i++ {
		if location := page.Header.Get("Location"); location != "" {
			next := absoluteURL(page.URL, location)
			if strings.Contains(next, "auth.opencode.ai/github/callback") {
				return next, nil
			}
			var err error
			page, err = requestPage(client, http.MethodGet, next, nil, page.URL)
			if err != nil {
				return "", err
			}
			continue
		}
		if strings.Contains(page.URL, "auth.opencode.ai/github/callback") {
			return page.URL, nil
		}
		if match := opencodeCallbackPattern.FindString(page.Body); match != "" {
			return strings.ReplaceAll(match, "&amp;", "&"), nil
		}
		form, err := findLoginForm(page.Body, "/login/oauth/authorize")
		if err != nil {
			return "", fmt.Errorf("cannot advance GitHub OAuth page: %s", page.URL)
		}
		form.Values.Set("authorize", "1")
		method := http.MethodGet
		if strings.EqualFold(form.Method, "post") {
			method = http.MethodPost
		}
		target := absoluteURL(page.URL, form.Action)
		if method == http.MethodGet {
			target += "?" + form.Values.Encode()
			form.Values = nil
		}
		page, err = requestPage(client, method, target, form.Values, page.URL)
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("too many GitHub OAuth redirects")
}

func completeOpencodeCallback(client *http.Client, callback string) (string, error) {
	page, err := requestPage(client, http.MethodGet, callback, nil, "")
	if err != nil {
		return "", err
	}
	location := page.Header.Get("Location")
	if location == "" {
		return "", fmt.Errorf("auth.opencode.ai callback did not redirect")
	}
	page, err = requestPage(client, http.MethodGet, absoluteURL(page.URL, location), nil, page.URL)
	if err != nil {
		return "", err
	}
	for i := 0; i < 5; i++ {
		if workspaceID := normalizeWorkspaceID(page.URL); strings.HasPrefix(workspaceID, "wrk_") {
			return workspaceID, nil
		}
		location = page.Header.Get("Location")
		if location == "" {
			break
		}
		if workspaceID := normalizeWorkspaceID(location); strings.HasPrefix(workspaceID, "wrk_") {
			return workspaceID, nil
		}
		page, err = requestPage(client, http.MethodGet, absoluteURL(page.URL, location), nil, page.URL)
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("workspace redirect not found after opencode callback")
}

func fetchOpenCodeGoKeys(client *http.Client, workspaceID string) ([]string, error) {
	keys, err := callOpenCodeKeyList(client, githubListKeysServerHash, workspaceID, "keys", 0)
	if err != nil {
		return nil, err
	}
	if len(keys) > 0 {
		return keys, nil
	}
	return callOpenCodeKeyList(client, githubListGoServerHash, workspaceID, "go", 1)
}

func callOpenCodeKeyList(client *http.Client, serverHash, workspaceID, page string, instanceIndex int) ([]string, error) {
	args := solidArgsJSON(workspaceID)
	endpoint, _ := url.Parse(opencodeBaseURL + "/_server")
	query := endpoint.Query()
	query.Set("id", serverHash)
	query.Set("args", string(args))
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequest(http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	instance := fmt.Sprintf("server-fn:%d", instanceIndex)
	req.Header.Set("User-Agent", adminImportUserAgent)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("X-Server-Id", serverHash)
	req.Header.Set("X-Server-Instance", instance)
	req.Header.Set("Referer", fmt.Sprintf("%s/workspace/%s/%s", opencodeBaseURL, workspaceID, page))
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch Go key list: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("Go key list returned HTTP %d", resp.StatusCode)
	}
	if strings.Contains(string(raw), `"/auth/authorize"`) {
		return nil, fmt.Errorf("opencode session expired while fetching Go keys")
	}
	return extractOpenCodeKeyValues(string(raw)), nil
}

func upsertImportedGoKeys(values []string, authCookie, workspaceID string, req githubImportRequest) ([]importedGoKey, int, int, error) {
	if len(values) == 0 {
		return nil, 0, 0, fmt.Errorf("no Go API key found")
	}
	weight := req.Weight
	if weight <= 0 {
		weight = 1
	}
	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = "GitHub 自动获取"
	}
	proxyURL := strings.TrimSpace(req.ProxyURL)
	out := make([]importedGoKey, 0, len(values))
	created, updated := 0, 0
	for idx, value := range values {
		var existing store.Key
		err := store.DB().Where("value = ?", value).First(&existing).Error
		if err == nil {
			updates := map[string]any{
				"cookie":           authCookie,
				"workspace_id":     workspaceID,
				"quota_snapshot":   "",
				"quota_updated_at": nil,
			}
			if proxyURL != "" {
				updates["proxy_url"] = proxyURL
			}
			if req.Weight > 0 {
				updates["weight"] = weight
			}
			if strings.TrimSpace(req.Label) != "" {
				updates["label"] = label
			}
			if err := store.DB().Model(&store.Key{}).Where("id = ?", existing.ID).Updates(updates).Error; err != nil {
				return nil, created, updated, err
			}
			store.DB().First(&existing, existing.ID)
			out = append(out, importedGoKey{ID: existing.ID, Value: maskSecret(existing.Value), Label: existing.Label, Created: false})
			updated++
			continue
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, created, updated, err
		}
		keyLabel := label
		if len(values) > 1 {
			keyLabel = fmt.Sprintf("%s #%d", label, idx+1)
		}
		k := store.Key{
			Value:       value,
			Group:       "go",
			Label:       keyLabel,
			Enabled:     true,
			Weight:      weight,
			ProxyURL:    proxyURL,
			Cookie:      authCookie,
			WorkspaceID: workspaceID,
		}
		if err := store.DB().Create(&k).Error; err != nil {
			return nil, created, updated, err
		}
		out = append(out, importedGoKey{ID: k.ID, Value: maskSecret(k.Value), Label: k.Label, Created: true})
		created++
	}
	return out, created, updated, nil
}
