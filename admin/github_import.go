package admin

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	opencodeBaseURL          = "https://opencode.ai"
	opencodeAuthURL          = "https://auth.opencode.ai"
	githubListKeysServerHash = "c22cd964237ba79f2f9b95faa2a14b804f870d1bab49279463379cc6a0fd0c85"
	githubListGoServerHash   = "def2ab20a296ef06465b1c3cf86da4ea983c0696e7a5708b9468aaed85083d6b"
	adminImportUserAgent     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"
)

var (
	opencodeCallbackPattern = regexp.MustCompile(`https://auth\.opencode\.ai/github/callback\?[^"'<> ]+`)
	apiKeyFieldPattern      = regexp.MustCompile(`(?i)(?:"?(?:key|value)"?)\s*[:=]\s*"((?:\\.|[^"\\]){8,})"`)
	apiKeyTokenPattern      = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_.-]{8,}|opencode_[A-Za-z0-9_.-]{8,})\b`)
)

type githubImportRequest struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	OTP        string `json:"otp"`
	TOTPSecret string `json:"totp_secret"`
	Label      string `json:"label"`
	Weight     int    `json:"weight"`
	ProxyURL   string `json:"proxy_url"`
}

type importedGoKey struct {
	ID      uint   `json:"id"`
	Value   string `json:"value"`
	Label   string `json:"label,omitempty"`
	Created bool   `json:"created"`
}

type loginForm struct {
	Action string
	Method string
	Values url.Values
}

type httpPage struct {
	URL    string
	Header http.Header
	Body   string
	Status int
}

func importGithubKeys(c *gin.Context) {
	var body githubImportRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	body.Password = strings.TrimSpace(body.Password)
	if body.Username == "" || body.Password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "GitHub login and password are required"})
		return
	}

	workspaceID, authCookie, keys, err := loginGithubAndFetchGoKeys(body)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	imported, created, updated, err := upsertImportedGoKeys(keys, authCookie, workspaceID, body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"workspace_id":       workspaceID,
		"cookie_configured":  authCookie != "",
		"imported":           len(imported),
		"created":            created,
		"updated":            updated,
		"keys":               imported,
		"auth_provider":      "github",
		"google_manual_only": true,
	})
}

func loginGithubAndFetchGoKeys(body githubImportRequest) (string, string, []string, error) {
	follow, noRedirect, jar, err := newLoginHTTPClients(body.ProxyURL)
	if err != nil {
		return "", "", nil, err
	}
	githubURL, err := startGitHubOAuth(noRedirect)
	if err != nil {
		return "", "", nil, err
	}
	page, err := requestPage(follow, http.MethodGet, githubURL, nil, "")
	if err != nil {
		return "", "", nil, err
	}
	if strings.Contains(page.URL, "github.com/login") {
		page, err = submitGitHubLogin(noRedirect, page, body.Username, body.Password)
		if err != nil {
			return "", "", nil, err
		}
	}
	page, err = advanceGitHubChallenges(noRedirect, page, body)
	if err != nil {
		return "", "", nil, err
	}
	callback, err := advanceGitHubOAuth(noRedirect, page)
	if err != nil {
		return "", "", nil, err
	}
	workspaceID, err := completeOpencodeCallback(noRedirect, callback)
	if err != nil {
		return "", "", nil, err
	}
	authCookie := opencodeAuthCookie(jar)
	if authCookie == "" {
		return "", "", nil, fmt.Errorf("GitHub login succeeded but opencode auth cookie was not found")
	}
	keys, err := fetchOpenCodeGoKeys(noRedirect, workspaceID)
	if err != nil {
		return "", "", nil, err
	}
	if len(keys) == 0 {
		return "", "", nil, fmt.Errorf("GitHub login succeeded but no Go API key was found")
	}
	return workspaceID, authCookie, keys, nil
}
