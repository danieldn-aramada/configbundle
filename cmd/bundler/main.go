package main

import (
	"log"
	"net/http"
	"time"

	"github.com/armada/configbundle/internal/bundler"
)

func main() {
	cfg, err := bundler.NewConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	orbital := &bundler.HTTPOrbitalClient{
		URL:         cfg.OrbitalGraphQLURL,
		APIURL:      cfg.OrbitalAPIURL,
		BearerToken: cfg.OrbitalBearerToken,
		HTTPClient:  &http.Client{Timeout: 25 * time.Second},
	}

	mux := http.NewServeMux()
	mux.Handle("POST /bundle", &bundler.Handler{Orbital: orbital, Resolutions: orbital})

	log.Printf("bundler starting on :%s (orbital: %s)", cfg.Port, cfg.OrbitalGraphQLURL)
	if err := http.ListenAndServe(":"+cfg.Port, mux); err != nil {
		log.Fatalf("bundler: %v", err)
	}
}
