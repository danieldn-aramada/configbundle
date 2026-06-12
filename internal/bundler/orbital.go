package bundler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// OrbitalQuerier fetches datacenter configuration from Orbital's GraphQL API.
type OrbitalQuerier interface {
	QueryDataCenter(ctx context.Context, name string) ([]DataCenterResult, error)
}

// ResolutionQuerier fetches pending force-resolutions from Orbital's REST API.
type ResolutionQuerier interface {
	QueryPendingForce(ctx context.Context) ([]PendingForceResolution, error)
}

// PendingForceResolution is one un-consumed force resolution from Orbital.
type PendingForceResolution struct {
	ID    string `json:"id"`
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

const configBundleQuery = `
query ConfigBundleFields($dc: String!) {
  queryDataCenter(filter: { name: { eq: $dc } }) {
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
type HTTPOrbitalClient struct {
	URL         string // GraphQL endpoint
	APIURL      string // REST API base (e.g. "http://localhost:8001")
	BearerToken string
	HTTPClient  *http.Client
}

func (c *HTTPOrbitalClient) QueryDataCenter(ctx context.Context, name string) ([]DataCenterResult, error) {
	body, err := json.Marshal(graphqlRequest{
		Query:     configBundleQuery,
		Variables: map[string]any{"dc": name},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal query: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.BearerToken)
	}

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

	return gqlResp.Data.QueryDataCenter, nil
}

func (c *HTTPOrbitalClient) QueryPendingForce(ctx context.Context) ([]PendingForceResolution, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.APIURL+"/api/v1/divergence/resolutions/pending-force", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if c.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.BearerToken)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query pending-force: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pending-force returned status %d", resp.StatusCode)
	}

	var resolutions []PendingForceResolution
	if err := json.NewDecoder(resp.Body).Decode(&resolutions); err != nil {
		return nil, fmt.Errorf("decode pending-force: %w", err)
	}
	return resolutions, nil
}
