// Package version exposes the build version of the opencode-go gateway and
// resolves the latest published GitHub release so the admin panel can report
// whether an update is available.
//
// Version is overridable at build time via ldflags:
//
//	go build -ldflags "-X github.com/xb0or/opencode-GO/version.Version=v1.2.3"
package version

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Version is the current build version. Override at build time via ldflags.
var Version = "v1.0.1"

// Repo is the GitHub "owner/name" identifier of the source repository.
const Repo = "xb0or/opencode-GO"

// GitHubURL returns the canonical HTTPS URL of the repository.
func GitHubURL() string {
	return "https://github.com/" + Repo
}

// LatestRelease describes the most recent published GitHub release.
type LatestRelease struct {
	Tag         string `json:"tag"`
	Name        string `json:"name"`
	HTMLURL     string `json:"html_url"`
	PublishedAt string `json:"published_at"`
}

// FetchLatestRelease fetches the latest GitHub release for Repo. Results are
// cached for cacheTTL to stay well within GitHub's unauthenticated rate limit
// (60 req/h/IP). Network failures return an error; callers should treat them
// as "no update info available".
func FetchLatestRelease(ctx context.Context) (LatestRelease, error) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if !cacheTime.IsZero() && time.Since(cacheTime) < cacheTTL && cacheErr == nil {
		return cacheRelease, nil
	}

	rel, err := fetchLatestRelease(ctx)
	cacheRelease = rel
	cacheErr = err
	cacheTime = time.Now()
	return rel, err
}

const cacheTTL = time.Hour

var (
	cacheMu      sync.Mutex
	cacheRelease LatestRelease
	cacheErr     error
	cacheTime    time.Time
)

func fetchLatestRelease(ctx context.Context) (LatestRelease, error) {
	url := "https://api.github.com/repos/" + Repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return LatestRelease{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return LatestRelease{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return LatestRelease{}, fmt.Errorf("github: unexpected status %d", resp.StatusCode)
	}

	var body struct {
		TagName     string `json:"tag_name"`
		Name        string `json:"name"`
		HTMLURL     string `json:"html_url"`
		PublishedAt string `json:"published_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return LatestRelease{}, err
	}
	return LatestRelease{
		Tag:         body.TagName,
		Name:        body.Name,
		HTMLURL:     body.HTMLURL,
		PublishedAt: body.PublishedAt,
	}, nil
}

// Compare returns -1, 0, or 1 depending on whether semver tag a is less than,
// equal to, or greater than b. Tags are expected in "vX.Y.Z" form; a leading
// "v" is optional. When either side fails to parse, comparison falls back to
// a plain string compare so callers still get a deterministic ordering.
func Compare(a, b string) int {
	an, aok := parseSemver(a)
	bn, bok := parseSemver(b)
	if !aok || !bok {
		return strings.Compare(a, b)
	}
	for i := 0; i < 3; i++ {
		if an[i] != bn[i] {
			if an[i] < bn[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// parseSemver splits a "vX.Y.Z" (or "X.Y.Z") tag into three numeric components.
// Extra pre-release/build metadata after "-" or "+" is ignored.
func parseSemver(tag string) ([3]int, bool) {
	var out [3]int
	s := strings.TrimPrefix(strings.TrimSpace(tag), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
