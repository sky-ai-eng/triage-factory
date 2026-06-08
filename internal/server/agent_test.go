package server

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/delegate"
)

// TestTakeoverErrorStatus locks in the sentinel → HTTP code mapping.
// The frontend treats 4xx vs 5xx differently (toast text, retry
// behavior) so a regression here is user-visible.
func TestTakeoverErrorStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"InvalidState", delegate.ErrTakeoverInvalidState, http.StatusBadRequest},
		{"InvalidState wrapped", fmt.Errorf("wrap: %w", delegate.ErrTakeoverInvalidState), http.StatusBadRequest},
		{"InProgress", delegate.ErrTakeoverInProgress, http.StatusConflict},
		{"InProgress wrapped", fmt.Errorf("wrap: %w", delegate.ErrTakeoverInProgress), http.StatusConflict},
		{"RaceLost", delegate.ErrTakeoverRaceLost, http.StatusConflict},
		{"RaceLost wrapped", fmt.Errorf("wrap: %w", delegate.ErrTakeoverRaceLost), http.StatusConflict},
		// Server-side failures (DB, filesystem, git) — the previous bug
		// was collapsing all of these to 400; confirm they're 500 now.
		{"random error", errors.New("disk full"), http.StatusInternalServerError},
		{"wrapped random", fmt.Errorf("copy worktree: %w", errors.New("permission denied")), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := takeoverErrorStatus(tc.err)
			if got != tc.want {
				t.Errorf("takeoverErrorStatus(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// TestShellQuote covers the safe-paste contract for the resume_command
// the takeover endpoint hands back to the UI. The quoting strategy
// is single-quote wrapping with the standard '"'"' escape for
// embedded single quotes — the standard shape POSIX shells understand.
func TestShellQuote(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain path", "/home/user/foo", `'/home/user/foo'`},
		{"path with spaces", "/home/jane doe/foo", `'/home/jane doe/foo'`},
		{"single quote", "Bobby's path", `'Bobby'"'"'s path'`},
		{"multiple single quotes", "a'b'c", `'a'"'"'b'"'"'c'`},
		{"empty string", "", `''`},
		{"only single quote", "'", `''"'"''`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shellQuote(tc.in)
			if got != tc.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

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
