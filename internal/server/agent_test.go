package server

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// TestSteerErrorStatus locks the SendMessage/Interrupt error → (HTTP code,
// reason) map the message and interrupt endpoints depend on: a not-right-now
// run state (no live process, not steerable, lost resume race) is 409 CONFLICT
// so the client re-checks, a concluded conversation is 409 ALREADY_TERMINAL
// (the same answer /stop gives), an expired workspace is 410 Gone (retry won't
// help), and everything else is 500 — which the handler routes through
// internalError rather than surfacing the raw text.
func TestSteerErrorStatus(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		want       int
		wantReason string
	}{
		{"NoLiveProcess", delegate.ErrNoLiveProcess, http.StatusConflict, httpx.ReasonConflict},
		{"NoLiveProcess wrapped", fmt.Errorf("wrap: %w", delegate.ErrNoLiveProcess), http.StatusConflict, httpx.ReasonConflict},
		{"NotSteerable", delegate.ErrConversationNotSteerable, http.StatusConflict, httpx.ReasonConflict},
		{"NotResumable", delegate.ErrConversationNotResumable, http.StatusConflict, httpx.ReasonConflict},
		{"Concluded", delegate.ErrConversationConcluded, http.StatusConflict, httpx.ReasonAlreadyTerminal},
		{"BlueprintCancelled", delegate.ErrBlueprintCancelled, http.StatusConflict, httpx.ReasonAlreadyTerminal},
		{"WorkspaceExpired", delegate.ErrWorkspaceExpired, http.StatusGone, httpx.ReasonConflict},
		{"WorkspaceExpired wrapped", fmt.Errorf("wrap: %w", delegate.ErrWorkspaceExpired), http.StatusGone, httpx.ReasonConflict},
		{"random server error", errors.New("db down"), http.StatusInternalServerError, httpx.ReasonInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := steerErrorStatus(tc.err)
			if got != tc.want || reason != tc.wantReason {
				t.Errorf("steerErrorStatus(%v) = (%d, %s), want (%d, %s)", tc.err, got, reason, tc.want, tc.wantReason)
			}
		})
	}
}
