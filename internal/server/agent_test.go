package server

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/delegate"
)

// TestSteerErrorStatus locks the SendMessage/Interrupt error → HTTP code map
// the message and interrupt endpoints depend on: a not-right-now run state
// (no live process, not steerable, lost resume race) is 409 so the client
// re-checks; everything else is 500.
func TestSteerErrorStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"NoLiveProcess", delegate.ErrNoLiveProcess, http.StatusConflict},
		{"NoLiveProcess wrapped", fmt.Errorf("wrap: %w", delegate.ErrNoLiveProcess), http.StatusConflict},
		{"NotSteerable", delegate.ErrRunNotSteerable, http.StatusConflict},
		{"NotResumable", delegate.ErrRunNotResumable, http.StatusConflict},
		{"random server error", errors.New("db down"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := steerErrorStatus(tc.err); got != tc.want {
				t.Errorf("steerErrorStatus(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}
