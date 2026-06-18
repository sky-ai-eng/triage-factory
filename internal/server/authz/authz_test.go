package authz

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
