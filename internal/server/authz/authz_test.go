package authz

import (
	"context"
	"errors"
	"fmt"
	"testing"
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
