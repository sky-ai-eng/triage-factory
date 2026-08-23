package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
)

// HTTPError is returned for any transport-level non-2xx GitHub response.
// Callers can use errors.As to inspect the status code.
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

// newStatusError builds the *HTTPError for a transport-level non-2xx. The
// "%s %s returned %d: %s" message format lives here, in one place, so the REST
// builders (request, GetConditional, DownloadArtifact) can't drift on it — the
// GET-only methods pass method "GET". (PostGraphQL keeps its own distinct
// "GraphQL returned %d: %s" shape — it has no REST path to report.)
func newStatusError(method, path string, statusCode int, body string) *HTTPError {
	return &HTTPError{
		StatusCode: statusCode,
		Body:       body,
		msg:        fmt.Sprintf("%s %s returned %d: %s", method, path, statusCode, body),
	}
}

// acceptJSON is the default Accept header for the GitHub v3 JSON API. Every
// request builder sends it unless the caller overrides it (GetRaw's diff/patch
// media types) or the endpoint doesn't take one (PostGraphQL).
const acceptJSON = "application/vnd.github.v3+json"

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

	// viaProxy marks a client whose baseURL is a per-run credential proxy
	// (NewProxyClient), not a real GitHub API base. The REST path
	// (/repos/{owner}/{repo}/…) reaches such a proxy verbatim, but the GraphQL
	// endpoint is a sibling of the REST base (api.github.com/graphql, or a GHES
	// /api/graphql next to /api/v3) that the REST proxy does not front — so
	// PostGraphQL fails closed here rather than silently misrouting. See
	// NewProxyClient.
	viaProxy bool

	// Rate-limit state (see ratelimit.go), updated from the headers on every
	// response the request core sees and read by RateLimit()/awaitBudget().
	rlMu        sync.Mutex
	rlRemaining int
	rlReset     time.Time
	rlUsed      int
	rlOK        bool // true once a response has carried rate-limit headers
	// rlObserver, when set via SetRateLimitObserver, is invoked on every
	// update to the fields above — the resolver's hook for mirroring this
	// client's (short-lived, per-resolve) state into its process-wide,
	// per-org RateLimitRegistry (see ratelimit_registry.go). nil-safe: a
	// *Client used directly (tests, other callers) simply has no observer.
	rlObserver func(RateLimitState)
}

// NewClient creates a GitHub API client. baseURL is the user-facing URL
// (e.g. "https://github.com" or "https://github.example.com").
func NewClient(baseURL, pat string) *Client {
	return &Client{
		baseURL: ghbase.APIBase(baseURL),
		pat:     pat,
		// At the transport, so REST, GraphQL, artifact downloads (which
		// clone this client, keeping its Transport), and the direct-Do
		// calls that skip the retry core are all covered — none of them
		// can reach the network any other way.
		http: telemetry.TracedHTTPClient(30*time.Second, "github"),
	}
}

// NewProxyClient builds a client that talks to a per-run credential proxy
// (internal/apiproxy) rather than to GitHub directly: proxyURL is the proxy's
// bare address and placeholder is the per-run token the proxy authenticates and
// swaps for the real credential on the upstream hop, so this client — and the
// process holding it — never touches a durable GitHub token (Property B).
//
// proxyURL is used as the REST base VERBATIM (no APIBase() derivation, unlike
// NewClient): the request path /repos/{owner}/{repo}/… must reach the proxy
// unmangled for it to prepend the real host's REST base (api.github.com, or a
// GHES /api/v3). GraphQL is deliberately unsupported through this client —
// PostGraphQL fails closed — because the GraphQL endpoint sits beside the REST
// base, not under it, and the REST proxy does not front it (see viaProxy).
func NewProxyClient(proxyURL, placeholder string) *Client {
	return &Client{
		baseURL: proxyURL,
		pat:     placeholder,
		// Covers the executor's hop to its own sidecar, not the sidecar's
		// hop to GitHub — the sidecar is span-free by standing decision,
		// so this is the only view of the call there is.
		http:     telemetry.TracedHTTPClient(30*time.Second, "github"),
		viaProxy: true,
	}
}

// NewClientWithHTTPClient is NewClient backed by a caller-supplied *http.Client
// rather than the default 30s one. It exists for callers that must control the
// Transport — a custom proxy/TLS stack in an enterprise deployment, or, in
// tests, a RoundTripper that injects faults or observes request cancellation.
// A nil hc falls back to NewClient's default. ctx threading is orthogonal: the
// supplied client still has every request built with http.NewRequestWithContext.
//
// Tracing follows the same ownership: a supplied client replaces the
// instrumented one NewClient built, so a caller that wants spans wraps its
// own Transport with telemetry.TracedTransport.
func NewClientWithHTTPClient(baseURL, pat string, hc *http.Client) *Client {
	c := NewClient(baseURL, pat)
	if hc != nil {
		c.http = hc
	}
	return c
}

// newRequest builds an authenticated *http.Request under ctx and is the shared
// construction prefix for the whole client: request() (the do-family core),
// GetConditional, DownloadArtifact, and PostGraphQL all build through it, so
// auth + header handling lives in exactly one place. It marshals a JSON body
// when present, applies setAuth (empty token ⇒ no Authorization header — see
// setAuth), sets Accept when non-empty (GraphQL passes ""), and sets
// Content-Type only when there's a body. fullURL is the absolute URL — callers
// pass c.baseURL+path, or the GraphQL endpoint.
func (c *Client) newRequest(ctx context.Context, method, fullURL string, body any, accept string) (*http.Request, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, bodyReader)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// request is the single ctx-aware request core behind the do() family
// (Get/Post/Put/Patch/Delete) and GetRaw. It builds through newRequest, honors
// ctx for cancellation (request-scoped cancellation, handler deadlines,
// poller/shutdown abort — additive to the 30s client timeout), reads the full
// body, and returns a typed *HTTPError on any non-2xx so every caller can
// status-discriminate via errors.As. The error string is the verbatim
// "%s %s returned %d: %s" the pre-unification do() formatted, so string-matching
// callers are unaffected and errors.As callers only gain accuracy.
//
// GET requests go through doIdempotent (rate-limit pre-flight + retry);
// every other method is a mutation and goes through doMutation (single
// attempt, no retry — see ratelimit.go). Either can return *ErrRateLimited
// in place of the transport error.
func (c *Client) request(ctx context.Context, method, path string, body any, accept string) ([]byte, error) {
	build := func() (*http.Request, error) {
		return c.newRequest(ctx, method, c.baseURL+path, body, accept)
	}

	var resp *http.Response
	var err error
	if method == http.MethodGet {
		resp, err = c.doIdempotent(ctx, build)
	} else {
		resp, err = c.doMutation(ctx, build)
	}
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body for %s: %w", path, err)
	}
	if resp.StatusCode >= 400 {
		return nil, newStatusError(method, path, resp.StatusCode, string(data))
	}
	return data, nil
}

// Get performs a context-aware authenticated GET request and returns the
// response body. ctx cancels the request; a non-2xx returns a *HTTPError.
func (c *Client) Get(ctx context.Context, path string) ([]byte, error) {
	return c.request(ctx, "GET", path, nil, acceptJSON)
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
// ctx cancels the request. Returns (0, error) on a non-2xx (notably 404 when
// the freshly-created bot account hasn't propagated yet) or a malformed/idless
// body, so the caller can fall back to the plain
// "<slug>[bot]@users.noreply.github.com" form. On a non-2xx the error wraps a
// *HTTPError, so a caller can status-discriminate via errors.As (e.g. 404 vs
// 429) rather than only checking err != nil.
func (c *Client) UserID(ctx context.Context, login string) (int64, error) {
	data, err := c.Get(ctx, "/users/"+url.PathEscape(login))
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
// don't use; it shares the newRequest build prefix but keeps the 304
// short-circuit and ETag return that the unified core doesn't model.
func (c *Client) GetConditional(ctx context.Context, path, etag string) (body []byte, newEtag string, notModified bool, err error) {
	build := func() (*http.Request, error) {
		req, err := c.newRequest(ctx, "GET", c.baseURL+path, nil, acceptJSON)
		if err != nil {
			return nil, err
		}
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		return req, nil
	}

	resp, err := c.doIdempotent(ctx, build)
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
		return nil, "", false, fmt.Errorf("read response body for %s: %w", path, readErr)
	}
	if resp.StatusCode >= 400 {
		return nil, "", false, newStatusError("GET", path, resp.StatusCode, string(data))
	}
	return data, resp.Header.Get("ETag"), false, nil
}

// Post performs a context-aware authenticated POST request with a JSON body.
func (c *Client) Post(ctx context.Context, path string, body any) ([]byte, error) {
	return c.request(ctx, "POST", path, body, acceptJSON)
}

// Put performs a context-aware authenticated PUT request with a JSON body.
func (c *Client) Put(ctx context.Context, path string, body any) ([]byte, error) {
	return c.request(ctx, "PUT", path, body, acceptJSON)
}

// Patch performs a context-aware authenticated PATCH request with a JSON body.
func (c *Client) Patch(ctx context.Context, path string, body any) ([]byte, error) {
	return c.request(ctx, "PATCH", path, body, acceptJSON)
}

// Delete performs a context-aware authenticated DELETE request.
func (c *Client) Delete(ctx context.Context, path string) ([]byte, error) {
	return c.request(ctx, "DELETE", path, nil, acceptJSON)
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
// ctx cancels the (potentially long) download; it's additive to the 15-minute
// clone timeout. Returns the number of bytes written to dst.
func (c *Client) DownloadArtifact(ctx context.Context, path string, dst io.Writer, maxBytes int64) (int64, error) {
	build := func() (*http.Request, error) {
		return c.newRequest(ctx, "GET", c.baseURL+path, nil, acceptJSON)
	}

	// Shallow copy so the long-download timeout doesn't bleed into other
	// API calls that share the same client. Inherits Transport/Jar/CheckRedirect.
	client := *c.http
	client.Timeout = downloadTimeout
	resp, err := c.doWithRetry(ctx, &client, true, build)
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
		return 0, newStatusError("GET", path, resp.StatusCode, string(body))
	}

	// Pre-flight size cap. GitHub's signed-URL redirect returns an honest
	// Content-Length for workflow log archives.
	if resp.ContentLength > 0 && resp.ContentLength > maxBytes {
		return 0, fmt.Errorf("artifact too large: %d bytes exceeds cap of %d", resp.ContentLength, maxBytes)
	}

	// io.LimitReader as a second guard. +1 so we can detect the cap was hit
	// even when Content-Length was missing and the body actually ran over.
	// Guard the arithmetic: at math.MaxInt64 the +1 wraps negative and
	// io.LimitReader would read nothing — a cap that large can't be exceeded
	// anyway, so skip the +1 there.
	readCap := maxBytes
	if readCap < math.MaxInt64 {
		readCap++
	}
	limited := io.LimitReader(resp.Body, readCap)
	n, err := io.Copy(dst, limited)
	if err != nil {
		return n, fmt.Errorf("stream artifact body: %w", err)
	}
	if n > maxBytes {
		return n, fmt.Errorf("artifact too large: received more than the cap of %d bytes (no Content-Length header)", maxBytes)
	}
	return n, nil
}

// GetRaw performs a context-aware authenticated GET with a custom Accept
// header and returns raw bytes. It is a thin layer over the unified core —
// the only thing distinct about it is the caller-supplied Accept (diff/patch
// media types); a non-2xx returns a *HTTPError like every other path.
func (c *Client) GetRaw(ctx context.Context, path, accept string) ([]byte, error) {
	return c.request(ctx, "GET", path, nil, accept)
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

// PostGraphQL sends a GraphQL query to GitHub's GraphQL API. ctx cancels the
// request. Only a transport-level (>= 400) non-2xx returns a *HTTPError; the
// load-bearing 200-carrying-errors[] partial-vs-total-failure path below is
// preserved exactly — a usable `data` block degrades to the partial result,
// an absent/null `data` is a genuine failure.
//
// Every caller in this codebase uses PostGraphQL for reads (queries), never
// mutations, so it goes through doIdempotent — a 429/secondary-403 retries
// like a GET rather than surfacing immediately as it would for a real
// mutation.
func (c *Client) PostGraphQL(ctx context.Context, body any) ([]byte, error) {
	if c.viaProxy {
		// A credential-proxy client's baseURL is the REST proxy, which does not
		// front the sibling GraphQL endpoint; deriving graphqlURL from it would
		// misroute (a GHES /api/v3/graphql 404, or a bypass straight to
		// api.github.com carrying only the placeholder). Fail closed until
		// GraphQL-over-proxy is wired end-to-end (routing + an installation-wide
		// bundle token for repo-less queries).
		return nil, errors.New("github: GraphQL is not supported through the per-run credential proxy")
	}
	build := func() (*http.Request, error) {
		return c.newRequest(ctx, "POST", graphqlURL(c.baseURL), body, "")
	}

	resp, err := c.doIdempotent(ctx, build)
	if err != nil {
		return nil, fmt.Errorf("graphql request: %w", err)
	}
	defer resp.Body.Close()

	// Surface a body-read failure rather than proceeding with an empty/partial
	// payload — matching request() and GetConditional. A truncated read on a
	// 200 would otherwise fall through, fail the partial-error unmarshal, and
	// return empty data with no error (silent partial behavior).
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read graphql response body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, &HTTPError{
			StatusCode: resp.StatusCode,
			Body:       string(data),
			msg:        fmt.Sprintf("GraphQL returned %d: %s", resp.StatusCode, string(data)),
		}
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
// and is the single place every request builder (newRequest, and through it
// request, GetConditional, GetRaw, DownloadArtifact, PostGraphQL) sets it — so
// "empty token ⇒ unauthenticated" holds uniformly across the whole client, not
// just one method.
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
