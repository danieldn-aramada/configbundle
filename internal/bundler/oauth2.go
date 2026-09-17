package bundler

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2/clientcredentials"
)

// DefaultHTTPTimeout is the default timeout for Orbital HTTP clients.
const DefaultHTTPTimeout = 25 * time.Second

// NewOAuth2HTTPClient returns an http.Client that obtains tokens via the OAuth2
// client credentials grant and retries once on HTTP 401.
// The returned client is safe for concurrent use and caches tokens until expiry.
//
// Token URL: cfg.TokenURL if set; otherwise derived from OIDCIssuerURL via
// oidcIssuerToTokenURL (Entra format). Scope: cfg.TokenScope if set; otherwise
// api://{OIDCClientID}/.default (Entra client-credentials default).
func NewOAuth2HTTPClient(cfg *Config) *http.Client {
	tokenURL := cfg.TokenURL
	if tokenURL == "" {
		tokenURL = oidcIssuerToTokenURL(cfg.OIDCIssuerURL)
	}
	return newOAuth2HTTPClientWithURL(cfg, tokenURL)
}

// oidcIssuerToTokenURL derives the Entra token endpoint from an OIDC issuer URL.
// "https://login.microsoftonline.com/{tenant}/v2.0" →
// "https://login.microsoftonline.com/{tenant}/oauth2/v2.0/token"
// Not used when cfg.TokenURL is set.
func oidcIssuerToTokenURL(issuerURL string) string {
	base := strings.TrimSuffix(issuerURL, "/")
	base = strings.TrimSuffix(base, "/v2.0")
	return base + "/oauth2/v2.0/token"
}

// tokenScope returns the OAuth2 scope to request. Uses cfg.TokenScope when set;
// otherwise falls back to the Entra client-credentials form.
func tokenScope(cfg *Config) string {
	if cfg.TokenScope != "" {
		return cfg.TokenScope
	}
	return "api://" + cfg.OIDCClientID + "/.default"
}

// newOAuth2HTTPClientWithURL is the internal constructor used by NewOAuth2HTTPClient
// and by tests to substitute a fake token endpoint.
func newOAuth2HTTPClientWithURL(cfg *Config, tokenURL string) *http.Client {
	ccCfg := &clientcredentials.Config{
		ClientID:     cfg.OIDCClientID,
		ClientSecret: cfg.OIDCClientSecret,
		TokenURL:     tokenURL,
		Scopes:       []string{tokenScope(cfg)},
	}
	// ccCfg.Client caches tokens via ReuseTokenSource; expires before expiry.
	oauth2Client := ccCfg.Client(context.Background())
	return &http.Client{
		Timeout: DefaultHTTPTimeout,
		Transport: &retryOn401Transport{
			base:  oauth2Client.Transport,
			ccCfg: ccCfg,
		},
	}
}

// NewStaticBearerHTTPClient returns an http.Client that injects a fixed bearer
// token on every request. Used when ORBITAL_BEARER_TOKEN is set.
func NewStaticBearerHTTPClient(token string) *http.Client {
	return &http.Client{
		Timeout: DefaultHTTPTimeout,
		Transport: &staticBearerTransport{
			token: token,
			base:  http.DefaultTransport,
		},
	}
}

// retryOn401Transport wraps an oauth2-aware transport. On a 401 response it
// force-fetches a fresh token (bypassing the cache) and retries the request
// once. The retry MUST reset the request body via req.GetBody —
// http.Request.Clone is shallow w.r.t. Body, so the first attempt's consumed
// Reader would be reused and the stdlib would reject the retry mid-write with
// "ContentLength=N with Body length 0".
type retryOn401Transport struct {
	base  http.RoundTripper
	ccCfg *clientcredentials.Config
}

func (t *retryOn401Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		log.Printf("bundler: oauth2 first-attempt transport error on %s %s: %v",
			req.Method, req.URL.Path, err)
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}

	log.Printf("bundler: orbital returned 401 on %s %s — force-refreshing token and retrying once",
		req.Method, req.URL.Path)
	resp.Body.Close()

	// ccCfg.Token always fetches fresh — bypasses ReuseTokenSource cache.
	tok, err := t.ccCfg.Token(req.Context())
	if err != nil {
		log.Printf("bundler: force-refresh after 401 failed: %v", err)
		return nil, fmt.Errorf("force-refresh after 401: %w", err)
	}

	// Reset the body — req.Clone is shallow w.r.t. Body, so the first attempt's
	// consumed Reader would be reused. GetBody is set by http.NewRequest when
	// the body type is *bytes.Reader / *bytes.Buffer / *strings.Reader; this
	// covers all our call sites.
	req2 := req.Clone(req.Context())
	if req.GetBody != nil {
		freshBody, err := req.GetBody()
		if err != nil {
			log.Printf("bundler: retry body restoration failed: %v", err)
			return nil, fmt.Errorf("restore body for retry: %w", err)
		}
		req2.Body = freshBody
	} else if req.Body != nil {
		// Non-replayable body and we hit 401 — surface a clear error rather
		// than the stdlib's confusing mid-write ContentLength mismatch.
		log.Printf("bundler: retry needs replayable body — caller must use bytes.Reader / bytes.Buffer / strings.Reader so http.NewRequest sets GetBody")
		return nil, fmt.Errorf("retry not possible: request body is not replayable")
	}

	tok.SetAuthHeader(req2)
	resp2, err := http.DefaultTransport.RoundTrip(req2)
	if err != nil {
		log.Printf("bundler: retry after 401 transport error: %v", err)
		return nil, err
	}
	log.Printf("bundler: retry after 401 returned %d", resp2.StatusCode)
	return resp2, nil
}

// staticBearerTransport injects a fixed bearer token on every request.
type staticBearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t *staticBearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req2 := req.Clone(req.Context())
	req2.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(req2)
}
