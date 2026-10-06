package linear

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

var (
	// ErrUnauthorized is Linear refusing the credential: a 401, or a GraphQL
	// error coded AUTHENTICATION_ERROR or FORBIDDEN. It is never retried, and
	// it is what a caller deciding the credential is dead keys on.
	ErrUnauthorized = errors.New("linear: credential refused")

	// ErrRateLimited is a request still rate limited after every attempt the
	// client allows. The error carrying it is a *RateLimitError, which holds
	// when the limit resets.
	ErrRateLimited = errors.New("linear: rate limited")

	// ErrNotFound is an id or identifier Linear resolves to nothing, which it
	// also answers for an entity the credential may not see.
	ErrNotFound = errors.New("linear: not found")
)

// GraphQLError is the first entry of a response's errors list. Status is the
// HTTP status it arrived with, which for Linear is usually 400 and may be 200.
type GraphQLError struct {
	Code    string
	Message string
	Path    []string
	Status  int
	class   upstream.Class
}

func (e *GraphQLError) Error() string {
	var b strings.Builder
	b.WriteString("linear: graphql error")
	if e.Code != "" {
		b.WriteString(" " + e.Code)
	}
	if len(e.Path) > 0 {
		b.WriteString(" at " + strings.Join(e.Path, "."))
	}
	// The message is Linear's own, shown through Excerpt like any upstream
	// error body, so it is bounded and carries no control characters.
	if e.Message != "" {
		body, _ := json.Marshal(map[string]string{"message": e.Message})
		b.WriteString(": " + upstream.Excerpt(body))
	}
	return b.String()
}

// UpstreamClass implements upstream.Classified.
func (e *GraphQLError) UpstreamClass() upstream.Class { return e.class }

// Is matches ErrUnauthorized for an auth-coded error and ErrNotFound for
// Linear's entity-not-found.
func (e *GraphQLError) Is(target error) bool {
	switch target {
	case ErrUnauthorized:
		return e.class == upstream.Auth
	case ErrNotFound:
		return e.Code == codeEntityNotFound || strings.HasPrefix(e.Message, notFoundPrefix)
	}
	return false
}

// StatusError is a failed response that carried no GraphQL errors: an HTTP
// failure before Linear's GraphQL layer answered, or a proxy's page. Body is
// the response body, capped at upstream.MaxErrorBody; the message carries only
// an excerpt of it.
type StatusError struct {
	Status int
	Body   string
	Class  upstream.Class
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("linear: HTTP %d: %s", e.Status, upstream.Excerpt([]byte(e.Body)))
}

// UpstreamClass implements upstream.Classified.
func (e *StatusError) UpstreamClass() upstream.Class { return e.Class }

// Is matches ErrUnauthorized for a 401, or a JSON 403.
func (e *StatusError) Is(target error) bool {
	return target == ErrUnauthorized && e.Class == upstream.Auth
}

// RateLimitError is a request still rate limited after the last attempt.
// Reset is when the limit lifts, zero when the response did not say. Err is
// the response itself: a *GraphQLError for Linear's RATELIMITED, a
// *StatusError for a 429.
type RateLimitError struct {
	Reset time.Time
	Err   error
}

func (e *RateLimitError) Error() string {
	if e.Reset.IsZero() {
		return fmt.Sprintf("%v (rate limited)", e.Err)
	}
	return fmt.Sprintf("%v (rate limited until %s)", e.Err, e.Reset.UTC().Format(time.RFC3339))
}

func (e *RateLimitError) Unwrap() error { return e.Err }

// Is matches ErrRateLimited.
func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

// UpstreamClass implements upstream.Classified.
func (e *RateLimitError) UpstreamClass() upstream.Class { return upstream.RateLimited }
