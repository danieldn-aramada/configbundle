package controller

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
)

// HTTPOrbClient implements OrbClient by POSTing a zip bundle to orb's
// POST /api/v1/import/subgraph endpoint.
type HTTPOrbClient struct {
	endpoint string
	http     *http.Client
}

// NewHTTPOrbClient returns an OrbClient that calls the given orb base URL.
func NewHTTPOrbClient(endpoint string) *HTTPOrbClient {
	return &HTTPOrbClient{
		endpoint: endpoint,
		http:     &http.Client{},
	}
}

// ImportSubgraph zips data and schema into a bundle and POSTs it to orb.
// Returns an error if orb responds with a non-2xx status code.
func (c *HTTPOrbClient) ImportSubgraph(ctx context.Context, data, schema []byte) error {
	body, err := buildSubgraphZip(data, schema)
	if err != nil {
		return fmt.Errorf("build subgraph zip: %w", err)
	}

	url := c.endpoint + "/api/v1/import/subgraph"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/zip")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("orb import returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// buildSubgraphZip creates an in-memory ZIP archive with data.json.gz and schema.gz.
func buildSubgraphZip(data, schema []byte) (*bytes.Buffer, error) {
	buf := &bytes.Buffer{}
	w := zip.NewWriter(buf)

	for _, entry := range []struct {
		name    string
		content []byte
	}{
		{"data.json.gz", data},
		{"schema.gz", schema},
	} {
		f, err := w.Create(entry.name)
		if err != nil {
			return nil, err
		}
		if _, err := f.Write(entry.content); err != nil {
			return nil, err
		}
	}

	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf, nil
}
