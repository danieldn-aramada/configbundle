package bundler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// newFakeTokenServer returns a test server that issues minimal OAuth2 tokens.
func newFakeTokenServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"test-access-token","token_type":"Bearer","expires_in":3600}`))
	}))
}

// newTestOAuth2Client builds an OAuth2 client pointed at a fake token server.
func newTestOAuth2Client(t *testing.T, tokenSrv *httptest.Server) *http.Client {
	t.Helper()
	cfg := &Config{
		OIDCIssuerURL:    tokenSrv.URL, // token URL will be derived as tokenSrv.URL/oauth2/v2.0/token
		OIDCClientID:     "test-client",
		OIDCClientSecret: "test-secret",
	}
	return newOAuth2HTTPClientWithURL(cfg, tokenSrv.URL+"/token")
}

func TestOIDCIssuerToTokenURL(t *testing.T) {
	cases := []struct {
		issuer string
		want   string
	}{
		{
			"https://login.microsoftonline.com/tenant-id/v2.0",
			"https://login.microsoftonline.com/tenant-id/oauth2/v2.0/token",
		},
		{
			"https://login.microsoftonline.com/tenant-id/v2.0/",
			"https://login.microsoftonline.com/tenant-id/oauth2/v2.0/token",
		},
		{
			"https://login.microsoftonline.com/tenant-id",
			"https://login.microsoftonline.com/tenant-id/oauth2/v2.0/token",
		},
	}
	for _, tc := range cases {
		got := oidcIssuerToTokenURL(tc.issuer)
		if got != tc.want {
			t.Errorf("oidcIssuerToTokenURL(%q) = %q, want %q", tc.issuer, got, tc.want)
		}
	}
}

func TestTokenURL_OverrideSkipsDerivation(t *testing.T) {
	var tokenCalls atomic.Int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","expires_in":3600}`))
	}))
	defer tokenSrv.Close()

	keycloakURL := tokenSrv.URL + "/realms/armada/protocol/openid-connect/token"
	cfg := &Config{
		OIDCIssuerURL:    "https://login.microsoftonline.com/tenant/v2.0", // would produce wrong URL if used
		OIDCClientID:     "cb-bundler",
		OIDCClientSecret: "secret",
		TokenURL:         keycloakURL,
	}
	// NewOAuth2HTTPClient must use TokenURL, not the Entra derivation.
	client := NewOAuth2HTTPClient(cfg)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer api.Close()

	resp, err := client.Get(api.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	if tokenCalls.Load() == 0 {
		t.Error("expected token endpoint to be called; it was not — wrong URL used")
	}
}

func TestTokenScope_Default(t *testing.T) {
	cfg := &Config{OIDCClientID: "my-client"}
	got := tokenScope(cfg)
	want := "api://my-client/.default"
	if got != want {
		t.Errorf("tokenScope default: got %q, want %q", got, want)
	}
}

func TestTokenScope_Override(t *testing.T) {
	cfg := &Config{OIDCClientID: "my-client", TokenScope: "openid profile"}
	got := tokenScope(cfg)
	want := "openid profile"
	if got != want {
		t.Errorf("tokenScope override: got %q, want %q", got, want)
	}
}

func TestTokenScope_SentToEndpoint(t *testing.T) {
	var gotScope string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		for _, part := range strings.Split(string(body), "&") {
			if strings.HasPrefix(part, "scope=") {
				gotScope = strings.TrimPrefix(part, "scope=")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","expires_in":3600}`))
	}))
	defer tokenSrv.Close()

	cfg := &Config{
		OIDCClientID:     "cb-bundler",
		OIDCClientSecret: "secret",
		TokenURL:         tokenSrv.URL,
		TokenScope:       "openid",
	}
	client := NewOAuth2HTTPClient(cfg)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer api.Close()

	resp, err := client.Get(api.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	if gotScope != "openid" {
		t.Errorf("scope sent to token endpoint: got %q, want %q", gotScope, "openid")
	}
}

func TestStaticBearerTransport_SetsHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewStaticBearerHTTPClient("my-token")
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	if gotAuth != "Bearer my-token" {
		t.Errorf("Authorization header: got %q, want %q", gotAuth, "Bearer my-token")
	}
}

func TestRetryOn401Transport_NoRetryOn200(t *testing.T) {
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer api.Close()

	tokenSrv := newFakeTokenServer(t)
	defer tokenSrv.Close()

	client := newTestOAuth2Client(t, tokenSrv)
	resp, err := client.Get(api.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	if calls.Load() != 1 {
		t.Errorf("expected 1 API call, got %d", calls.Load())
	}
}

func TestRetryOn401Transport_RetryOn401(t *testing.T) {
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer api.Close()

	tokenSrv := newFakeTokenServer(t)
	defer tokenSrv.Close()

	client := newTestOAuth2Client(t, tokenSrv)
	resp, err := client.Get(api.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 after retry, got %d", resp.StatusCode)
	}
	if calls.Load() != 2 {
		t.Errorf("expected 2 API calls (1 original + 1 retry), got %d", calls.Load())
	}
}

func TestRetryOn401Transport_NoSecondRetryAfterRetry(t *testing.T) {
	// If the retry also returns 401, return it as-is (no infinite loop).
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer api.Close()

	tokenSrv := newFakeTokenServer(t)
	defer tokenSrv.Close()

	client := newTestOAuth2Client(t, tokenSrv)
	resp, err := client.Get(api.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 returned after retry, got %d", resp.StatusCode)
	}
	// original + 1 retry = 2 total calls
	if calls.Load() != 2 {
		t.Errorf("expected 2 API calls, got %d", calls.Load())
	}
}

func TestRetryOn401Transport_InjectsToken(t *testing.T) {
	var gotAuth string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer api.Close()

	tokenSrv := newFakeTokenServer(t)
	defer tokenSrv.Close()

	client := newTestOAuth2Client(t, tokenSrv)
	resp, err := client.Get(api.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	if gotAuth != "Bearer test-access-token" {
		t.Errorf("Authorization header: got %q, want %q", gotAuth, "Bearer test-access-token")
	}
}
