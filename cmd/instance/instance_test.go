package instance

import (
	"testing"
	"time"
)

func TestHeartbeatAge(t *testing.T) {
	if got := heartbeatAge(time.Time{}); got != "never" {
		t.Errorf("heartbeatAge(zero) = %q, want %q", got, "never")
	}
	if got := heartbeatAge(time.Now().Add(-5 * time.Second)); got != "5s ago" {
		t.Errorf("heartbeatAge(5s ago) = %q, want %q", got, "5s ago")
	}
	// last in the future (clock skew between the DB server and this CLI's
	// host) must not render a negative duration.
	if got := heartbeatAge(time.Now().Add(5 * time.Second)); got != "just now" {
		t.Errorf("heartbeatAge(future) = %q, want %q", got, "just now")
	}
}
