package upstream

import (
	"crypto/tls"
	"net/http"
	"net/url"
	"time"

	"github.com/opencode-sw/gateway/config"
)

// NewClient builds an *http.Client tuned for upstream LLM calls (streaming-
// friendly: no response-body timeout, TLS compat enabled, large idle pool).
func NewClient() *http.Client {
	return newClientWithProxy("")
}

// NewClientForProxy builds an *http.Client that routes traffic through the
// given proxyURL. If proxyURL is empty, it falls back to the environment
// proxy (http.ProxyFromEnvironment).
func NewClientForProxy(proxyURL string) *http.Client {
	return newClientWithProxy(proxyURL)
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

// Timeout returns the configured upstream timeout for context deadlines.
func Timeout() time.Duration {
	cfg := config.Get()
	return time.Duration(cfg.UpstreamTimeout) * time.Second
}
