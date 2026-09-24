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

	// OAuth2 client credentials, used when OrbitalBearerToken is empty.
	//
	// These have NO defaults on purpose. They previously defaulted to an Entra
	// tenant and client id, which survived orbital's move to Keycloak: setting
	// only ORBITAL_OIDC_CLIENT_SECRET then sent a Keycloak secret to
	// login.microsoftonline.com, which rejected it with AADSTS7000215
	// ("Invalid client secret") — an error naming a tenant nobody had chosen.
	// Orbital's own config dropped its OIDC defaults for the same reason and
	// says so explicitly: "a tenant or client id baked in as a default silently
	// points someone else's deployment at OUR identity provider."
	//
	// For local development copy hack/local/bundler.env.example to
	// hack/local/bundler.env — `make run-bundler` sources it when present.
	OIDCIssuerURL    string `envconfig:"ORBITAL_OIDC_ISSUER_URL"    default:""`
	OIDCClientID     string `envconfig:"ORBITAL_OIDC_CLIENT_ID"     default:""`
	OIDCClientSecret string `envconfig:"ORBITAL_OIDC_CLIENT_SECRET" default:""`

	// TokenURL overrides the token endpoint derived from OIDCIssuerURL.
	// Required for non-Entra providers (e.g. Keycloak:
	// https://<host>/realms/<realm>/protocol/openid-connect/token).
	TokenURL string `envconfig:"ORBITAL_TOKEN_URL" default:""`

	// TokenScope overrides the OAuth2 scope sent to the token endpoint.
	// Empty falls back to the Entra client-credentials form
	// api://{OIDCClientID}/.default, which Keycloak does NOT recognise — set it
	// (e.g. "openid") for any non-Entra provider.
	TokenScope string `envconfig:"ORBITAL_TOKEN_SCOPE" default:""`
}

func NewConfig() (*Config, error) {
	var cfg Config
	if err := envconfig.Process("", &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
