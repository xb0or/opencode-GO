package upstream

import (
	"crypto/tls"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/xb0or/opencode-GO/config"
)

// ---------------------------------------------------------------------------
// HTTP Client / Transport caching
// ---------------------------------------------------------------------------
//
// Creating a new http.Client (with a new http.Transport) per key attempt
// defeats connection keep-alive, causes excessive TLS handshakes, and
// leaks idle connections that only get reclaimed by GC.  We cache clients
// by normalised proxy URL so transports are reused across requests.
//
// A global "no-proxy" client is shared by all keys that have no ProxyURL.
// Keys with a ProxyURL get one client per unique proxy address.

var (
	clientCacheMu sync.RWMutex
	clientCache   = make(map[string]*http.Client)
)

// NewClient builds an *http.Client tuned for upstream LLM calls (streaming-
// friendly: no response-body timeout, TLS compat enabled, large idle pool).
// The returned client is shared across all callers that use no proxy.
func NewClient() *http.Client {
	return NewClientForProxy("")
}

// NewClientForProxy returns a cached *http.Client that routes traffic
// through the given proxyURL.  If proxyURL is empty, it falls back to the
// environment proxy (http.ProxyFromEnvironment).  Clients are cached by
// normalised proxy URL so connection pools are reused across requests.
func NewClientForProxy(proxyURL string) *http.Client {
	key := normaliseProxyKey(proxyURL)

	// Fast path: read lock.
	clientCacheMu.RLock()
	if c, ok := clientCache[key]; ok {
		clientCacheMu.RUnlock()
		return c
	}
	clientCacheMu.RUnlock()

	// Slow path: write lock + double-check.
	clientCacheMu.Lock()
	defer clientCacheMu.Unlock()
	if c, ok := clientCache[key]; ok {
		return c
	}
	c := newClientWithProxy(proxyURL)
	clientCache[key] = c
	return c
}

// CloseIdleConnections closes idle connections on all cached transports.
// Called during graceful shutdown or config reload.
func CloseIdleConnections() {
	clientCacheMu.Lock()
	defer clientCacheMu.Unlock()
	for _, c := range clientCache {
		if t, ok := c.Transport.(*http.Transport); ok {
			t.CloseIdleConnections()
		}
	}
}

func normaliseProxyKey(proxyURL string) string {
	if proxyURL == "" {
		return "__no_proxy__"
	}
	// Normalise: parse and re-encode to get a canonical form.
	u, err := url.Parse(proxyURL)
	if err != nil {
		return proxyURL // fall back to raw string
	}
	return u.String()
}

func newClientWithProxy(proxyURL string) *http.Client {
	var proxyFunc func(*http.Request) (*url.URL, error)
	if proxyURL != "" {
		proxyFunc = func(r *http.Request) (*url.URL, error) {
			return url.Parse(proxyURL)
		}
	} else {
		proxyFunc = http.ProxyFromEnvironment
	}
	return &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			Proxy: proxyFunc,
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
			},
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     90 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Timeout returns the configured upstream timeout for non-streaming request
// context deadlines. A non-positive value disables the gateway deadline.
func Timeout() time.Duration {
	cfg := config.Get()
	return time.Duration(cfg.UpstreamTimeout) * time.Second
}