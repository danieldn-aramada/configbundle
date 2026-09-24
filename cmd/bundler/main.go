package main

import (
	"log"
	"net/http"

	"github.com/armada/configbundle/internal/bundler"
	"github.com/armada/configbundle/internal/version"
)

func main() {
	cfg, err := bundler.NewConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	httpClient := buildHTTPClient(cfg)

	orbital := &bundler.HTTPOrbitalClient{
		BaseURL:    cfg.OrbitalBaseURL,
		HTTPClient: httpClient,
	}

	mux := http.NewServeMux()
	mux.Handle("POST /bundle", &bundler.Handler{Orbital: orbital, Resolutions: orbital})

	log.Printf("bundler starting version=%s port=%s orbital=%s", version.Version, cfg.Port, cfg.OrbitalBaseURL)
	if err := http.ListenAndServe(":"+cfg.Port, mux); err != nil {
		log.Fatalf("bundler: %v", err)
	}
}

// buildHTTPClient returns an auth-aware http.Client based on config priority:
//  1. ORBITAL_BEARER_TOKEN — static bearer (deprecated fallback)
//  2. ORBITAL_OIDC_CLIENT_SECRET set — OAuth2 client credentials (any OIDC
//     provider; set ORBITAL_TOKEN_URL + ORBITAL_TOKEN_SCOPE for non-Entra)
//  3. Neither — plain client (for local dev without auth)
func buildHTTPClient(cfg *bundler.Config) *http.Client {
	if cfg.OrbitalBearerToken != "" {
		log.Println("bundler: using static bearer token for Orbital auth (ORBITAL_BEARER_TOKEN)")
		return bundler.NewStaticBearerHTTPClient(cfg.OrbitalBearerToken)
	}
	if cfg.OIDCClientSecret != "" {
		// Refuse rather than guess. The token endpoint used to default to an
		// Entra tenant, so a half-configured bundler silently sent the operator's
		// secret to login.microsoftonline.com and reported AADSTS7000215 —
		// an error about a tenant they had never configured. With no defaults,
		// an unresolvable endpoint must say so here instead of failing later
		// as a malformed request.
		if cfg.TokenURL == "" && cfg.OIDCIssuerURL == "" {
			log.Fatal("bundler: ORBITAL_OIDC_CLIENT_SECRET is set but no token endpoint is configured — " +
				"set ORBITAL_TOKEN_URL (Keycloak: https://<host>/realms/<realm>/protocol/openid-connect/token) " +
				"or ORBITAL_OIDC_ISSUER_URL. See hack/local/bundler.env.example.")
		}
		if cfg.OIDCClientID == "" {
			log.Fatal("bundler: ORBITAL_OIDC_CLIENT_SECRET is set but ORBITAL_OIDC_CLIENT_ID is empty — " +
				"orbital accepts tokens only from client ids listed in its ORBITAL_AUTH_PROVIDERS.")
		}
		log.Printf("bundler: using OAuth2 client credentials for Orbital auth (clientId: %s, tokenURL: %s)",
			cfg.OIDCClientID, tokenEndpoint(cfg))
		return bundler.NewOAuth2HTTPClient(cfg)
	}
	log.Println("bundler: no Orbital credentials configured — every request to orbital will 401. " +
		"Set ORBITAL_OIDC_CLIENT_ID/_SECRET/_TOKEN_URL/_TOKEN_SCOPE (copy hack/local/bundler.env.example " +
		"to hack/local/bundler.env), or ORBITAL_BEARER_TOKEN for a one-off token.")
	return &http.Client{Timeout: bundler.DefaultHTTPTimeout}
}

// tokenEndpoint reports the endpoint NewOAuth2HTTPClient will use, for logging.
func tokenEndpoint(cfg *bundler.Config) string {
	if cfg.TokenURL != "" {
		return cfg.TokenURL
	}
	return cfg.OIDCIssuerURL + " (derived, Entra format)"
}
