package authz

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// TestResolveErrorUnwrap pins that a resolveError exposes its cause to
// errors.Is. ResolveTeamID's default-team lookup can fail with a canceled
// request context (TFAC-398); WriteResolveError funnels that into
// httpx.InternalError, whose client-gone check unwraps via errors.Is. Without
// the Unwrap method the cancellation would be invisible through the wrapper and
// log as a 500.
func TestResolveErrorUnwrap(t *testing.T) {
	cause := fmt.Errorf("default team lookup: %w", context.Canceled)
	re := &resolveError{notFound: false, err: cause}

	if !errors.Is(re, context.Canceled) {
		t.Fatalf("errors.Is(resolveError, context.Canceled) = false, want true")
	}
	if errors.Is(re, context.DeadlineExceeded) {
		t.Errorf("errors.Is(resolveError, context.DeadlineExceeded) = true, want false")
	}
}

// TestWriteResolveError_Routing is the end-to-end pin for the resolve-error
// renderer: it exercises the full WriteResolveError → httpx.InternalError wiring
// rather than the Unwrap and InternalError halves in isolation, so a regression
// that stops passing err through directly (e.g. reconstructing it and dropping
// the cause) is caught here. The client-gone row is the TFAC-398 case: a
// canceled lookup must render 499, not 500.
func TestWriteResolveError_Routing(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"not found", &resolveError{notFound: true, err: fmt.Errorf("invalid team_id")}, http.StatusNotFound},
		{"client gone", &resolveError{notFound: false, err: fmt.Errorf("default team lookup: %w", context.Canceled)}, httpx.StatusClientClosedRequest},
		{"real error", &resolveError{notFound: false, err: errors.New("boom")}, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			WriteResolveError(rec, "settings/team", tc.err)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// TestTokenScopeAllows pins the one rule that separates a token from a session
// at these gates, including the part that is easy to get wrong: the comparison
// is between org IDENTITIES, not between the strings two sides happened to
// spell them with.
func TestTokenScopeAllows(t *testing.T) {
	const (
		orgA = "3f7c1b2e-9a41-4d5f-8b06-2c1d4e5f6a70"
		orgB = "8c2d5a1f-4b63-42e7-9f10-77aa33bb44cc"
	)
	withToken := func(tokenOrg string) *http.Request {
		r := httptest.NewRequest("GET", "/api/orgs/x", nil)
		return r.WithContext(httpx.WithTokenAuth(r.Context(),
			&httpx.TokenAuth{TokenID: "tok-1", OrgID: tokenOrg}))
	}

	// A session names any org it likes; this gate has nothing to say about it.
	if !tokenScopeAllows(httptest.NewRequest("GET", "/api/orgs/x", nil), orgB) {
		t.Error("a session-authed request must pass the scope check for any org")
	}

	for _, tc := range []struct {
		name  string
		path  string
		allow bool
	}{
		{"its own org", orgA, true},
		{"its own org uppercased", strings.ToUpper(orgA), true},
		{"its own org unhyphenated", strings.ReplaceAll(orgA, "-", ""), true},
		{"another org", orgB, false},
		{"another org uppercased", strings.ToUpper(orgB), false},
		{"not a uuid", "default", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tokenScopeAllows(withToken(orgA), tc.path); got != tc.allow {
				t.Errorf("tokenScopeAllows(token=%s, path=%q) = %v, want %v",
					orgA, tc.path, got, tc.allow)
			}
		})
	}

	// A non-uuid on BOTH sides still matches exactly, which is what local
	// mode's sentinel org would need if a token could ever reach it.
	if !tokenScopeAllows(withToken("default"), "default") {
		t.Error("two identical non-uuid org ids must compare equal")
	}
}
