// Package linear is TF's client for Linear's GraphQL API. Every request is a
// POST of {"query", "variables"} to one endpoint, and every query and mutation
// document is a constant in this package: what varies per call travels as a
// variable, never as text spliced into a document.
//
// It shares nothing with internal/jira, so either provider can be removed
// without touching the other. What the two have in common — failure
// classification, backoff, bounded error bodies, request metrics — comes from
// internal/upstream.
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// DefaultEndpoint is Linear's GraphQL API.
const DefaultEndpoint = "https://api.linear.app/graphql"

// AuthMethod is the shape of a stored Linear credential. The org's service
// credential records it under the linear_auth_method secret; a user's
// credential carries it in its envelope (UserCredential).
type AuthMethod string

const (
	// AuthMethodAPIKey is a Linear personal API key, sent as the bare
	// Authorization header value.
	AuthMethodAPIKey AuthMethod = "api_key"
	// AuthMethodAppInstall is the org's workspace installing TF as an OAuth
	// app (actor=app): TF acts as the resulting app user through a refreshed
	// access token. Org credential only.
	AuthMethodAppInstall AuthMethod = "app_install"
	// AuthMethodConnectOAuth is a user's one-click Connect (actor=user): an
	// OAuth access token acting as that user. User credential only.
	AuthMethodConnectOAuth AuthMethod = "connect_oauth"
)

// Config says where a Client sends its requests and with what Authorization
// header. Build one with APIKey, Bearer or ProxyPlaceholder; the header is
// unexported so it can only be set that way.
type Config struct {
	// Endpoint is the GraphQL URL every request is POSTed to: DefaultEndpoint,
	// or a per-run credential proxy's (ProxyPlaceholder).
	Endpoint      string
	authorization string
}

// APIKey builds a Config for a Linear personal API key. Linear takes a
// personal key as the whole Authorization header, with no scheme.
func APIKey(key string) Config {
	return Config{Endpoint: DefaultEndpoint, authorization: key}
}

// Bearer builds a Config for an OAuth access token: the org's app install, or
// a user's Connect.
func Bearer(accessToken string) Config {
	return Config{Endpoint: DefaultEndpoint, authorization: "Bearer " + accessToken}
}

// ProxyPlaceholder builds a Config for a client that talks to a per-run
// credential proxy rather than to Linear: requests go to baseURL's /graphql,
// the path Linear serves, and carry placeholder as a Bearer token. The proxy
// swaps it for the org's real credential on the upstream hop, so the process
// holding this client never holds a Linear credential.
func ProxyPlaceholder(baseURL, placeholder string) Config {
	return Config{
		Endpoint:      strings.TrimRight(baseURL, "/") + "/graphql",
		authorization: "Bearer " + placeholder,
	}
}

// maxAttempts is the most attempts a request gets, whatever ended each one.
const maxAttempts = 3

// The extensions.code values the client classifies itself, and the two ways
// Linear says an id resolves to nothing (a code, or a message prefix).
const (
	codeRateLimited         = "RATELIMITED"
	codeAuthenticationError = "AUTHENTICATION_ERROR"
	codeForbidden           = "FORBIDDEN"
	codeEntityNotFound      = "ENTITY_NOT_FOUND"
	notFoundPrefix          = "Entity not found"
)

// rateLimitWaitCap is the longest a rate-limited request waits for its
// window to reset before trying again. backoffBase and backoffCap shape the
// wait after a transient failure. They are vars only so tests can shorten
// them.
var (
	rateLimitWaitCap = 60 * time.Second
	backoffBase      = 500 * time.Millisecond
	backoffCap       = 30 * time.Second
)

// userAgent names TF and its release on every request. It reads "dev" until
// SetVersion is called, which is what an unreleased build reports too.
var userAgent = "triagefactory/dev"

// SetVersion sets the release the User-Agent names: main.Version, which the
// release build stamps with the tag. main calls it once at boot, before any
// request is made.
func SetVersion(version string) {
	userAgent = "triagefactory/" + version
}

// Client sends GraphQL requests to Linear with one Config's credential.
type Client struct {
	cfg  Config
	http *http.Client
	// orgID is the org every request is counted under (upstream.Record). Set
	// by WithOrg; empty for a client that makes its calls for no org.
	orgID string
}

// NewClient builds a Client. Every Linear call in the process is made through
// a Client built here, so the traced transport covers all of them.
func NewClient(cfg Config) *Client {
	return &Client{
		cfg:  cfg,
		http: telemetry.TracedHTTPClient(15*time.Second, "linear"),
	}
}

// WithOrg sets the org c makes its calls for, so its requests are counted
// under that org, and returns c. Call it before c's first request.
func (c *Client) WithOrg(orgID string) *Client {
	c.orgID = orgID
	return c
}

// gqlRequest is the body of every request.
type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

// gqlResponse is the envelope of every response.
type gqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []gqlError      `json:"errors"`
}

type gqlError struct {
	Message    string `json:"message"`
	Path       []any  `json:"path"`
	Extensions struct {
		Code string `json:"code"`
	} `json:"extensions"`
}

// query sends a read. A read is idempotent, so a transient failure is retried.
func (c *Client) query(ctx context.Context, doc string, vars map[string]any, out any) error {
	return c.do(ctx, doc, vars, true, out)
}

// mutate sends a write. A transient failure may have happened after Linear
// applied it, so only a rate limit, which Linear refused before acting, is
// retried.
func (c *Client) mutate(ctx context.Context, doc string, vars map[string]any, out any) error {
	return c.do(ctx, doc, vars, false, out)
}

// do sends one GraphQL document and decodes its data into out. A response
// carrying any errors entry is an error, even when it carries data too, so a
// partial answer is never read as a whole one.
//
// Each attempt is counted under the client's org. A rate-limited attempt
// waits for the later of Linear's request and complexity windows to reset,
// at most rateLimitWaitCap; a transient failure of an idempotent request
// waits out an exponential backoff. No request gets more than maxAttempts
// attempts, and every wait ends when ctx does.
//
// Under a fail-fast scope (upstream.WithFailFast), a request that ends in a
// transient failure marks Linear unreachable for the rest of the scope, so
// later requests get one attempt, and two timeouts with no answer between
// them stop requests being sent at all.
func (c *Client) do(ctx context.Context, doc string, vars map[string]any, idempotent bool, out any) error {
	if c.cfg.authorization == "" {
		return fmt.Errorf("linear: Config has no credential - build it with APIKey, Bearer or ProxyPlaceholder")
	}
	body, err := json.Marshal(gqlRequest{Query: doc, Variables: vars})
	if err != nil {
		return fmt.Errorf("linear: encode request: %w", err)
	}
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Endpoint, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("linear: build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", c.cfg.authorization)
		req.Header.Set("User-Agent", userAgent)
		host := req.URL.Host
		if upstream.Silent(ctx, host) {
			return &upstream.TransportError{Err: upstream.ErrHostSilent}
		}

		sent := time.Now()
		resp, err := c.http.Do(req)
		if err != nil {
			class, counted := upstream.ClassifyTransport(ctx, err)
			if !counted {
				return err
			}
			upstream.Record(ctx, upstream.Linear, c.orgID, class)
			if !upstream.RetryableTransport(err, idempotent) || attempt >= maxAttempts || upstream.Unreachable(ctx, host) {
				upstream.MarkTransportFailure(ctx, host, sent, err)
				return &upstream.TransportError{Err: err}
			}
			if err := c.wait(ctx, class, backoff(attempt)); err != nil {
				return err
			}
			continue
		}
		upstream.MarkAnswered(ctx, host)

		var data []byte
		if resp.StatusCode >= 300 {
			data, err = upstream.ReadErrorBody(resp.Body)
		} else {
			data, err = io.ReadAll(resp.Body)
		}
		_ = resp.Body.Close()
		if err != nil {
			// The response broke off mid-body: a transport failure like one
			// from Do, retried on the same terms. Linear did answer, so it
			// counts toward unreachable but never toward silent.
			class, counted := upstream.ClassifyTransport(ctx, err)
			if !counted {
				return err
			}
			upstream.Record(ctx, upstream.Linear, c.orgID, class)
			if !upstream.RetryableTransport(err, idempotent) || attempt >= maxAttempts || upstream.Unreachable(ctx, host) {
				upstream.MarkUnreachable(ctx, host)
				return &upstream.TransportError{Err: err}
			}
			if err := c.wait(ctx, class, backoff(attempt)); err != nil {
				return err
			}
			continue
		}

		var env gqlResponse
		decodeErr := json.Unmarshal(data, &env)
		class := classify(resp.StatusCode, resp.Header, data, env.Errors)
		upstream.Record(ctx, upstream.Linear, c.orgID, class)

		retry := upstream.RetryableResponse(resp.StatusCode, class, idempotent) &&
			attempt < maxAttempts && !upstream.Unreachable(ctx, host)
		if !retry {
			if class == upstream.Transient {
				upstream.MarkUnreachable(ctx, host)
			}
			if err := responseError(resp.StatusCode, resp.Header, data, env.Errors, class); err != nil {
				return err
			}
			if decodeErr != nil {
				return fmt.Errorf("linear: decode response: %w", decodeErr)
			}
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(env.Data, out); err != nil {
				return fmt.Errorf("linear: decode response data: %w", err)
			}
			return nil
		}

		wait := backoff(attempt)
		if class == upstream.RateLimited {
			wait = rateLimitWait(resp.Header, attempt)
		}
		if err := c.wait(ctx, class, wait); err != nil {
			return err
		}
	}
}

// wait counts the decision to retry after an attempt that ended in class,
// then waits d or until ctx is done.
func (c *Client) wait(ctx context.Context, class upstream.Class, d time.Duration) error {
	upstream.RecordRetry(ctx, upstream.Linear, c.orgID, class)
	return upstream.Sleep(ctx, d)
}

// classify is Linear's own reading of a response, ahead of the default. Linear
// signals a rate limit with a 400 whose GraphQL error code is RATELIMITED, not
// with a 429, and a refused credential with an AUTHENTICATION_ERROR or
// FORBIDDEN code as well as with a 401. A response that is otherwise a success
// but carries GraphQL errors is the request being refused.
func classify(status int, h http.Header, body []byte, errs []gqlError) upstream.Class {
	for _, e := range errs {
		if e.Extensions.Code == codeRateLimited {
			return upstream.RateLimited
		}
	}
	if status == http.StatusUnauthorized {
		return upstream.Auth
	}
	for _, e := range errs {
		if isAuthCode(e.Extensions.Code) {
			return upstream.Auth
		}
	}
	class := upstream.ClassifyResponse(status, h, body)
	if class == upstream.OK && len(errs) > 0 {
		return upstream.Rejected
	}
	return class
}

func isAuthCode(code string) bool {
	return code == codeAuthenticationError || code == codeForbidden
}

// responseError is the error a final response stands for, or nil for a
// success: the first GraphQL error when there is one, else the HTTP status. A
// rate limit is a *RateLimitError in either shape, so a 429 from in front of
// Linear matches ErrRateLimited as Linear's own RATELIMITED does.
func responseError(status int, h http.Header, body []byte, errs []gqlError, class upstream.Class) error {
	var err error
	switch {
	case len(errs) > 0:
		e := errs[0]
		err = &GraphQLError{
			Code:    e.Extensions.Code,
			Message: e.Message,
			Path:    pathStrings(e.Path),
			Status:  status,
			class:   class,
		}
	case status >= 300:
		err = &StatusError{Status: status, Body: string(body), Class: class}
	default:
		return nil
	}
	if class == upstream.RateLimited {
		return &RateLimitError{Reset: rateLimitReset(h), Err: err}
	}
	return err
}

// pathStrings renders a GraphQL error path, whose elements are field names
// and list indexes.
func pathStrings(path []any) []string {
	if len(path) == 0 {
		return nil
	}
	out := make([]string, len(path))
	for i, p := range path {
		out[i] = fmt.Sprint(p)
	}
	return out
}

// rateLimitReset is when the limit lifts: the later of Linear's request and
// complexity windows' resets, each sent as UTC epoch milliseconds. Linear sends
// no Retry-After, but a 429 from something in front of it may, so that is the
// fallback. Zero when none of the headers is present.
func rateLimitReset(h http.Header) time.Time {
	var reset time.Time
	for _, name := range []string{"X-RateLimit-Requests-Reset", "X-RateLimit-Complexity-Reset"} {
		ms, err := strconv.ParseInt(strings.TrimSpace(h.Get(name)), 10, 64)
		if err != nil || ms <= 0 {
			continue
		}
		if t := time.UnixMilli(ms); t.After(reset) {
			reset = t
		}
	}
	if reset.IsZero() {
		if d, ok := upstream.RetryAfter(h); ok {
			reset = time.Now().Add(d)
		}
	}
	return reset
}

// rateLimitWait is how long a rate-limited attempt waits: until the limit
// resets, at most rateLimitWaitCap. A reset that is absent or already past
// falls back to the backoff, so the retry is never immediate.
func rateLimitWait(h http.Header, attempt int) time.Duration {
	wait := time.Until(rateLimitReset(h))
	if wait <= 0 {
		return backoff(attempt)
	}
	return min(wait, rateLimitWaitCap)
}

func backoff(attempt int) time.Duration {
	return upstream.Backoff(attempt, backoffBase, backoffCap)
}
