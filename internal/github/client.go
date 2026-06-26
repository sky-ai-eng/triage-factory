package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HTTPError is returned by GetRaw when the server responds with a non-2xx
// status. Callers can use errors.As to inspect the status code.
type HTTPError struct {
	StatusCode int
	Body       string
	msg        string
}

func (e *HTTPError) Error() string { return e.msg }

// NewHTTPError reconstructs an *HTTPError from its wire-transmitted parts.
// The agenthost IPC path serializes a GitHub API failure's status code,
// body, and rendered message across the per-run socket; the sandbox-side
// client rebuilds the typed error here so callers that discriminate on
// status (IsHTTP406, the download-logs 404 fallback) keep working over the
// RPC exactly as they do against an in-process *Client.
func NewHTTPError(statusCode int, body, msg string) *HTTPError {
	return &HTTPError{StatusCode: statusCode, Body: body, msg: msg}
}

// IsHTTP406 reports whether err is an HTTP 406 Not Acceptable response.
func IsHTTP406(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.StatusCode == 406
}

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
	return &Client{
		baseURL: APIBase(baseURL),
		pat:     pat,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Get performs an authenticated GET request and returns the response body.
func (c *Client) Get(path string) ([]byte, error) {
	return c.do("GET", path, nil)
}

// UserID resolves the numeric user-account id for a GitHub login (e.g. a bot
// account "<slug>[bot]") via GET /users/{login}. It is the bot *user* id — the
// account behind the App — not the App id, and it's the value the numeric-id
// noreply commit email is built from so App-bot commits link on github.com
// (TFAC-474).
//
// /users/{login} is a public endpoint that works unauthenticated, which is what
// the App-registration path relies on (it has only the org's GitHub base URL,
// no installation token yet). The login is url.PathEscape'd because a bot login
// carries "[bot]" brackets that would otherwise be mangled in the path. The
// call routes through c.baseURL like every other request, so it targets
// github.com or the org's GHES host without hardcoding api.github.com.
//
// Returns (0, error) on a non-2xx (notably 404 when the freshly-created bot
// account hasn't propagated yet) or a malformed/idless body, so the caller can
// fall back to the plain "<slug>[bot]@users.noreply.github.com" form.
func (c *Client) UserID(login string) (int64, error) {
	data, err := c.Get("/users/" + url.PathEscape(login))
	if err != nil {
		return 0, fmt.Errorf("get user %q: %w", login, err)
	}
	var u struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(data, &u); err != nil {
		return 0, fmt.Errorf("parse user %q: %w", login, err)
	}
	if u.ID == 0 {
		return 0, fmt.Errorf("user %q: response carried no id", login)
	}
	return u.ID, nil
}

// GetConditional performs an authenticated GET that sends an If-None-Match
// header when etag is non-empty. It surfaces the conditional-request outcome
// the plain Get hides:
//
//   - 304 Not Modified → (nil, "", true, nil). Conditional requests that
//     resolve to 304 do NOT increment the primary rate-limit counter
//     (x-ratelimit-used), so a quiet resource is free to re-poll.
//   - 200 OK → (body, <response ETag>, false, nil). The returned ETag should
//     be stored and replayed on the next call.
//   - other non-2xx → (nil, "", false, *HTTPError).
//
// Kept separate from Get so existing callers don't have to thread ETags they
// don't use.
func (c *Client) GetConditional(path, etag string) (body []byte, newEtag string, notModified bool, err error) {
	url := c.baseURL + path
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, "", false, err
	}
	c.setAuth(req)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", false, fmt.Errorf("request %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		// Drain so the connection can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, "", true, nil
	}

	data, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, "", false, fmt.Errorf("read response body: %w", readErr)
	}
	if resp.StatusCode >= 400 {
		return nil, "", false, &HTTPError{
			StatusCode: resp.StatusCode,
			Body:       string(data),
			msg:        fmt.Sprintf("GET %s returned %d: %s", path, resp.StatusCode, string(data)),
		}
	}
	return data, resp.Header.Get("ETag"), false, nil
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
	c.setAuth(req)
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
		// Wrap in *HTTPError so callers can errors.As to discriminate
		// status codes (e.g., the download-logs fallback path needs to
		// detect 404 specifically — GitHub returns it for runs that
		// haven't finished yet — without resorting to string matching).
		return 0, &HTTPError{
			StatusCode: resp.StatusCode,
			Body:       string(body),
			msg:        fmt.Sprintf("GET %s returned %d: %s", path, resp.StatusCode, string(body)),
		}
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
	c.setAuth(req)
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
		body := string(data)
		return nil, &HTTPError{
			StatusCode: resp.StatusCode,
			Body:       body,
			msg:        fmt.Sprintf("GET %s returned %d: %s", path, resp.StatusCode, body),
		}
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

// gqlError is one entry in a GraphQL response's errors[] array. Path is a mixed
// string/int array (field names + list indices); only the PostGraphQL
// partial-error path reads it, the per-method re-checks use Type + Message.
type gqlError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Path    []any  `json:"path"`
}

// gqlErrors is a GraphQL response's errors[] array. It lets a method embed
// `Errors gqlErrors` in its response struct and surface a partial error in one
// line, rather than re-declaring the struct + the len-check at every call site.
type gqlErrors []gqlError

// first returns a formatted error for the first entry, or nil when empty. PostGraphQL
// passes a 200 partial response (usable data + errors[]) through without erroring,
// so a method that finds an errors[] scoped to its own field calls this to surface
// it rather than misreading the empty data as a real "not found". context prefixes
// the message (e.g. "get review").
func (e gqlErrors) first(context string) error {
	if len(e) == 0 {
		return nil
	}
	return fmt.Errorf("%s: GraphQL error (%s): %s", context, e[0].Type, e[0].Message)
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
	c.setAuth(req)
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

	// GraphQL can return a 200 carrying BOTH a usable `data` block and an
	// `errors[]` list — a per-field partial failure (e.g. a FORBIDDEN
	// statusCheckRollup on a PR when the App lacks statuses:read). The old
	// behavior treated any errors[] as a total failure and discarded the data,
	// so one forbidden field on one node aborted the whole batch. Distinguish
	// the two: present-and-non-null `data` ⇒ degrade to the partial result;
	// absent/null `data` ⇒ a genuine failure (bad query, cost ceiling, auth).
	var gqlResp struct {
		Data   json.RawMessage `json:"data"`
		Errors gqlErrors       `json:"errors"`
	}
	if json.Unmarshal(data, &gqlResp) == nil && len(gqlResp.Errors) > 0 {
		e := gqlResp.Errors[0]
		// `data` present and not JSON null ⇒ the response is usable; callers
		// already tolerate missing/null nodes (refreshPRsBatch skips null nodes,
		// buildSnapshot treats a nil statusCheckRollup as "polled, no CI").
		usableData := len(gqlResp.Data) > 0 && string(gqlResp.Data) != "null"
		if usableData {
			// One log line per response (= per batch), not per node, so even an
			// all-forbidden batch logs once. Summarize via the first error.
			githubLog.Warn("GraphQL partial error; using partial data",
				"errors", len(gqlResp.Errors), "type", e.Type, "path", e.Path, "message", e.Message)
			return data, nil
		}
		// Surface the first error; note when more were dropped so a multi-error
		// total failure isn't silently reduced to a single message.
		if len(gqlResp.Errors) > 1 {
			return nil, fmt.Errorf("GraphQL error (%s) at %v: %s (and %d more)",
				e.Type, e.Path, e.Message, len(gqlResp.Errors)-1)
		}
		return nil, fmt.Errorf("GraphQL error (%s) at %v: %s", e.Type, e.Path, e.Message)
	}

	return data, nil
}

// setAuth applies the bearer Authorization header for an authenticated client,
// and is the single place every request builder (do, GetConditional, GetRaw,
// DownloadArtifact, PostGraphQL) sets it — so "empty token ⇒ unauthenticated"
// holds uniformly across the whole client, not just one method.
//
// An empty token means an unauthenticated client. The App-registration path
// builds one to read the public GET /users/{login} (UserID) before any
// installation token exists (TFAC-474). We omit the header entirely rather than
// send a malformed "Bearer " with no value, which GitHub rejects as bad
// credentials (401) — strictly worse than an anonymous request, which GitHub
// serves for public reads. Every real call site passes a non-empty token
// (PAT or minted installation token), so this only affects the deliberate
// unauthenticated case and never weakens an authenticated request.
func (c *Client) setAuth(req *http.Request) {
	if c.pat == "" {
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.pat)
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
	c.setAuth(req)
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
