package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a GitHub API client that handles auth and base URL routing.
type Client struct {
	baseURL string // API base: "https://api.github.com" or "{ghe}/api/v3"
	pat     string
	http    *http.Client
}

// NewClient creates a GitHub API client. baseURL is the user-facing URL
// (e.g. "https://github.com" or "https://github.example.com").
func NewClient(baseURL, pat string) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	apiBase := baseURL + "/api/v3"
	if baseURL == "https://github.com" {
		apiBase = "https://api.github.com"
	}
	return &Client{
		baseURL: apiBase,
		pat:     pat,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Get performs an authenticated GET request and returns the response body.
func (c *Client) Get(path string) ([]byte, error) {
	return c.do("GET", path, nil)
}

// Post performs an authenticated POST request with a JSON body.
func (c *Client) Post(path string, body any) ([]byte, error) {
	return c.do("POST", path, body)
}

// Put performs an authenticated PUT request with a JSON body.
func (c *Client) Put(path string, body any) ([]byte, error) {
	return c.do("PUT", path, body)
}

// Patch performs an authenticated PATCH request with a JSON body.
func (c *Client) Patch(path string, body any) ([]byte, error) {
	return c.do("PATCH", path, body)
}

// Delete performs an authenticated DELETE request.
func (c *Client) Delete(path string) ([]byte, error) {
	return c.do("DELETE", path, nil)
}

// GetRaw performs an authenticated GET with a custom Accept header and returns raw bytes.
func (c *Client) GetRaw(path, accept string) ([]byte, error) {
	url := c.baseURL + path
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.pat)
	req.Header.Set("Accept", accept)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("GET %s returned %d: %s", path, resp.StatusCode, string(data))
	}
	return data, nil
}

// graphqlURL derives the GraphQL endpoint from the REST API base URL.
func graphqlURL(baseURL string) string {
	// GitHub.com REST: https://api.github.com     → GraphQL: https://api.github.com/graphql
	// GHES REST:       https://<host>/api/v3      → GraphQL: https://<host>/api/graphql
	if strings.Contains(baseURL, "api.github.com") {
		return "https://api.github.com/graphql"
	}
	return strings.TrimSuffix(baseURL, "/v3") + "/graphql"
}

// PostGraphQL sends a GraphQL query to GitHub's GraphQL API.
func (c *Client) PostGraphQL(body any) ([]byte, error) {
	url := graphqlURL(c.baseURL)

	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.pat)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("GraphQL returned %d: %s", resp.StatusCode, string(data))
	}

	// Check for GraphQL-level errors
	var gqlResp struct {
		Errors []struct{ Message string } `json:"errors"`
	}
	if json.Unmarshal(data, &gqlResp) == nil && len(gqlResp.Errors) > 0 {
		return nil, fmt.Errorf("GraphQL error: %s", gqlResp.Errors[0].Message)
	}

	return data, nil
}

func (c *Client) do(method, path string, body any) ([]byte, error) {
	url := c.baseURL + path

	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.pat)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s %s returned %d: %s", method, path, resp.StatusCode, string(data))
	}
	return data, nil
}
