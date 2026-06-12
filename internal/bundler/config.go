package bundler

import (
	"github.com/kelseyhightower/envconfig"
)

// Config holds all bundler configuration. Defaults are set for local development.
type Config struct {
	Port               string `envconfig:"BUNDLER_PORT"              default:"8020"`
	OrbitalGraphQLURL  string `envconfig:"ORBITAL_GRAPHQL_URL"       default:"http://localhost:8001/graphql"`
	OrbitalAPIURL      string `envconfig:"ORBITAL_API_URL"           default:"http://localhost:8001"`
	OrbitalBearerToken string `envconfig:"ORBITAL_BEARER_TOKEN"     default:""`
}

func NewConfig() (*Config, error) {
	var cfg Config
	if err := envconfig.Process("", &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
