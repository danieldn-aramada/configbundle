package bundler

import (
	"github.com/kelseyhightower/envconfig"
)

// Config holds all bundler configuration. Defaults are set for local development.
type Config struct {
	Port string `envconfig:"BUNDLER_PORT" default:"8020"`

	// OrbitalBaseURL is the single root URL for orbital — both GraphQL
	// (`<base>/graphql`) and REST (`<base>/api/v1/...`) are derived from it.
	// Must include any base path orbital is mounted under
	// (e.g. AKS: `http://localhost:8001/orbital`, local: `http://localhost:8001`).
	// Trailing slashes are trimmed at use sites.
	OrbitalBaseURL     string `envconfig:"ORBITAL_BASE_URL"     default:"http://localhost:8001"`
	OrbitalBearerToken string `envconfig:"ORBITAL_BEARER_TOKEN" default:""`

	// OAuth2 client credentials. Token URL is derived from OIDCIssuerURL (Entra
	// format) unless TokenURL is set. Scope defaults to the Entra client-credentials
	// form unless TokenScope is set. Used when OrbitalBearerToken is empty.
	// See docs/reference/API.md § bundler auth.
	OIDCIssuerURL    string `envconfig:"ORBITAL_OIDC_ISSUER_URL"    default:"https://login.microsoftonline.com/8f231c2a-9551-4b40-be17-5b24afe5e890/v2.0"`
	OIDCClientID     string `envconfig:"ORBITAL_OIDC_CLIENT_ID"     default:"5fc832f6-843e-4207-93dd-b3c3a77c06f2"`
	OIDCClientSecret string `envconfig:"ORBITAL_OIDC_CLIENT_SECRET" default:""`

	// TokenURL overrides the token endpoint derived from OIDCIssuerURL.
	// Required for non-Entra providers (e.g. Keycloak:
	// https://<host>/realms/<realm>/protocol/openid-connect/token).
	TokenURL string `envconfig:"ORBITAL_TOKEN_URL" default:""`

	// TokenScope overrides the OAuth2 scope sent to the token endpoint.
	// Defaults to the Entra client-credentials scope api://{OIDCClientID}/.default.
	// Set for non-Entra providers that do not recognise that scope format.
	TokenScope string `envconfig:"ORBITAL_TOKEN_SCOPE" default:""`
}

func NewConfig() (*Config, error) {
	var cfg Config
	if err := envconfig.Process("", &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
