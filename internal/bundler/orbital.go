package bundler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// OrbitalQuerier fetches datacenter configuration from Orbital's GraphQL API
// by orbId — DataCenter.orbId is hash-indexed in DGraph and supports exact
// `eq` filtering. Returns a single-element slice when found, empty when not.
type OrbitalQuerier interface {
	QueryDataCenter(ctx context.Context, orbID string) ([]DataCenterResult, error)
}

// ResolutionQuerier fetches active takeover and omission resolutions from Orbital.
// Both queries return only currently-active resolutions — a resolution row in
// orbital lives 1:1 with the underlying DivergenceEntry, so once the loop
// closes (orb stops reporting the divergence) the resolution disappears from
// the query results too. The bundler does NOT mark resolutions consumed —
// orbital's source of truth is what it observes, not what consumers assert.
type ResolutionQuerier interface {
	QueryPendingForce(ctx context.Context) ([]PendingForceResolution, error)
	QueryOmissions(ctx context.Context) ([]Omission, error)
}

// PendingForceResolution is one un-consumed accept- or reject-resolution from Orbital.
// The bundler doesn't distinguish action — orbital has already mutated the intent
// value (for accept) before returning the row, so both shapes look identical here.
type PendingForceResolution struct {
	OrbID string
	Field string
}

// Omission is one ignore-resolution from Orbital. It identifies a (orbId, field)
// pair that the bundler must remove from the cb-manifest apply config, so the
// controller releases its claim and the local:<id> manager remains sole owner.
type Omission struct {
	OrbID string `json:"orbId"`
	Field string `json:"field"`
}

// DataCenterResult is the GraphQL response shape for a DataCenter node.
type DataCenterResult struct {
	Name    string         `json:"name"`
	OrbID   string         `json:"orbId"`
	Servers []ServerResult `json:"servers"`
}

// ServerResult is the GraphQL response shape for a Server node.
type ServerResult struct {
	Hostname      string               `json:"hostname"`
	ServiceTag    string               `json:"serviceTag"`
	OrbID         string               `json:"orbId"`
	OobIP         *IPAddressResult     `json:"oobIP"`
	IdracSettings *IdracSettingsResult `json:"idracSettings"`
}

// IPAddressResult is the GraphQL response shape for an IPAddress node.
type IPAddressResult struct {
	Address string `json:"address"`
}

// IdracSettingsResult is the GraphQL response shape for an IdracSettings node.
// Field names match the Orbital DGraph schema exactly.
type IdracSettingsResult struct {
	OrbID                       string `json:"orbId"`
	FirmwareVersion             string `json:"firmwareVersion"`
	SSHEnabled                  bool   `json:"sshEnabled"`
	IPMIEnabled                 bool   `json:"ipmiEnabled"`
	LockdownModeEnabled         bool   `json:"lockdownModeEnabled"`
	OsToIdracPassThroughEnabled bool   `json:"osToIdracPassThroughEnabled"`
	UsbManagementPortEnabled    bool   `json:"usbManagementPortEnabled"`
	DHCPEnabled                 bool   `json:"dhcpEnabled"`
	RacadmEnabled               bool   `json:"racadmEnabled"`
}

// divergenceEntry is the wire shape for one item from GET /api/v1/divergences.
// Only the fields the bundler uses are decoded; the rest are ignored.
type divergenceEntry struct {
	EntryOrbID string `json:"entryOrbId"`
	Field      string `json:"field"`
	Resolution struct {
		Action string `json:"action"`
	} `json:"resolution"`
}

// configBundleQuery filters by orbId — DataCenter.orbId is
// @search(by: [hash]) which generates StringHashFilter (supports `eq`).
// Verified working against the live DGraph schema 2026-06-12.
const configBundleQuery = `
query ConfigBundleByOrbID($orbId: String!) {
  queryDataCenter(filter: { orbId: { eq: $orbId } }) {
    name
    orbId
    servers {
      hostname
      serviceTag
      orbId
      oobIP {
        address
      }
      idracSettings {
        orbId
        firmwareVersion
        sshEnabled
        ipmiEnabled
        lockdownModeEnabled
        osToIdracPassThroughEnabled
        usbManagementPortEnabled
        dhcpEnabled
        racadmEnabled
      }
    }
  }
}`

type graphqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

type graphqlResponse struct {
	Data struct {
		QueryDataCenter []DataCenterResult `json:"queryDataCenter"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// HTTPOrbitalClient queries Orbital's GraphQL and REST APIs over HTTP.
// Auth is handled by the injected HTTPClient transport (OAuth2 or static bearer).
type HTTPOrbitalClient struct {
	URL        string // GraphQL endpoint
	APIURL     string // REST API base (e.g. "http://localhost:8001")
	HTTPClient *http.Client
}

func (c *HTTPOrbitalClient) QueryDataCenter(ctx context.Context, orbID string) ([]DataCenterResult, error) {
	body, err := json.Marshal(graphqlRequest{
		Query:     configBundleQuery,
		Variables: map[string]any{"orbId": orbID},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal query: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("graphql request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("graphql returned status %d", resp.StatusCode)
	}

	var gqlResp graphqlResponse
	if err := json.NewDecoder(resp.Body).Decode(&gqlResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if len(gqlResp.Errors) > 0 {
		return nil, fmt.Errorf("graphql error: %s", gqlResp.Errors[0].Message)
	}

	// orbId is hash-indexed and unique — filter returns 0 or 1 result.
	return gqlResp.Data.QueryDataCenter, nil
}

func (c *HTTPOrbitalClient) QueryPendingForce(ctx context.Context) ([]PendingForceResolution, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.APIURL+"/api/v1/divergences", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	q := req.URL.Query()
	q.Add("action", "accept")
	q.Add("action", "reject")
	req.URL.RawQuery = q.Encode()

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query pending-force: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pending-force returned status %d", resp.StatusCode)
	}

	var entries []divergenceEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("decode pending-force: %w", err)
	}
	result := make([]PendingForceResolution, 0, len(entries))
	for _, e := range entries {
		result = append(result, PendingForceResolution{OrbID: e.EntryOrbID, Field: e.Field})
	}
	return result, nil
}

// QueryOmissions returns the set of (orbId, field) pairs that orbital has marked
// for ignore resolution. The bundler must remove these from the cb-manifest apply
// config on every bundle build (they persist until the resolution row is deleted).
func (c *HTTPOrbitalClient) QueryOmissions(ctx context.Context) ([]Omission, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.APIURL+"/api/v1/divergences", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	q := req.URL.Query()
	q.Set("action", "ignore")
	req.URL.RawQuery = q.Encode()

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query omissions: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("omissions returned status %d", resp.StatusCode)
	}

	var entries []divergenceEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("decode omissions: %w", err)
	}
	result := make([]Omission, 0, len(entries))
	for _, e := range entries {
		result = append(result, Omission{OrbID: e.EntryOrbID, Field: e.Field})
	}
	return result, nil
}
