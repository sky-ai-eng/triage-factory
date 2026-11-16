package server

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/llm/modelaccess"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// TestSteerErrorStatus pins the SendMessage error → (status, reason) map: a
// not-right-now state is 409 CONFLICT, a concluded or cancelled one 409
// ALREADY_TERMINAL, a model the organization cannot authenticate 409
// NOT_CONFIGURED, an expired workspace 410, and anything else 500.
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
		{"ProviderUnconfigured", fmt.Errorf("%w: connect it", modelaccess.ErrProviderUnconfigured), http.StatusConflict, httpx.ReasonNotConfigured},
		{"NoCredentials", fmt.Errorf("%w: connect a provider", modelaccess.ErrNoCredentials), http.StatusConflict, httpx.ReasonNotConfigured},
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
