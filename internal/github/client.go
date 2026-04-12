package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// downloadTimeout is the cap for streaming artifact downloads (log archives
// and similar large blobs). Deliberately way longer than the 30s shared-client
// timeout on c.http — a 400 MB log archive on a slow link can take several
// minutes, and we'd rather wait than cancel mid-stream.
const downloadTimeout = 15 * time.Minute

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

// DownloadArtifact performs an authenticated streaming GET against a GitHub
// endpoint that serves large binary blobs via a 302 redirect to a signed URL
// — e.g. /repos/{owner}/{repo}/actions/runs/{run_id}/logs, which redirects to
// a short-lived pipelines.actions.githubusercontent.com URL.
//
// The response body is streamed directly to dst without buffering in memory,
// capped at maxBytes total. The cap is enforced two ways: Content-Length is
// checked up front when GitHub provides it, and io.LimitReader wraps the
// copy as a belt-and-suspenders guard in case the header is missing or wrong.
//
// Uses a shallow copy of c.http with Timeout overridden to downloadTimeout.
// The shared client's 30-second timeout is unusable for multi-hundred-MB
// downloads, but we can't simply construct a fresh http.Client because that
// would discard any Transport/proxy/TLS configuration that was attached to
// c.http — which matters in enterprise environments with corporate proxies
// or GHES instances with custom root CAs. Shallow-copying the struct and
// overriding only Timeout preserves every other field (Transport, Jar,
// CheckRedirect) while extending the download window. The copy is safe
// because http.Client's fields are value types or interfaces that we don't
// mutate.
//
// Redirects are followed automatically by the inherited transport; Go's
// stdlib strips the Authorization header on cross-origin redirects, which
// is the right behavior here — the signed S3 URL would reject our Bearer
// token anyway.
//
// Returns the number of bytes written to dst.
func (c *Client) DownloadArtifact(ctx context.Context, path string, dst io.Writer, maxBytes int64) (int64, error) {
	url := c.baseURL + path

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.pat)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	// Shallow copy so the long-download timeout doesn't bleed into other
	// API calls that share the same client. Inherits Transport/Jar/CheckRedirect.
	client := *c.http
	client.Timeout = downloadTimeout
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("download request %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		// Drain a modest amount of the error body for context — these are
		// usually small JSON messages.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, fmt.Errorf("GET %s returned %d: %s", path, resp.StatusCode, string(body))
	}

	// Pre-flight size cap. GitHub's signed-URL redirect returns an honest
	// Content-Length for workflow log archives.
	if resp.ContentLength > 0 && resp.ContentLength > maxBytes {
		return 0, fmt.Errorf("artifact too large: %d bytes exceeds cap of %d", resp.ContentLength, maxBytes)
	}

	// io.LimitReader as a second guard. +1 so we can detect the cap was hit
	// even when Content-Length was missing and the body actually ran over.
	limited := io.LimitReader(resp.Body, maxBytes+1)
	n, err := io.Copy(dst, limited)
	if err != nil {
		return n, fmt.Errorf("stream artifact body: %w", err)
	}
	if n > maxBytes {
		return n, fmt.Errorf("artifact too large: received more than the cap of %d bytes (no Content-Length header)", maxBytes)
	}
	return n, nil
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
