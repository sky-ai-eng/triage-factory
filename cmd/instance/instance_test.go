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
}
