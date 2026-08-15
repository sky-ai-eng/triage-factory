package server

import (
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The webhook is the LATENCY half of how the mirror learns a grant's width: a
// fresh install's repository_selection lands the moment the delivery does
// instead of at the next poll. The reconcile is what makes it CORRECT, since
// GitHub does not auto-retry a delivery it failed to make and local mode
// receives none at all — so this pins the fast path without letting it become
// the contract.
func TestGitHubWebhook_InstallationCreated_RecordsRepositorySelection(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)

	body := []byte(`{"action":"created","installation":{"id":4242,"account":{"login":"acme","type":"Organization"},` +
		`"created_at":"2026-01-01T00:00:00Z","repository_selection":"selected"}}`)
	if rec := postWebhook(s, "installation", sign(body), body); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	got := onlyInstallation(t, s)
	if got.RepositorySelection != domain.RepositorySelectionSelected {
		t.Errorf("RepositorySelection = %q; want %q", got.RepositorySelection, domain.RepositorySelectionSelected)
	}
	if got.GrantsEveryRepository() {
		t.Error("GrantsEveryRepository() = true for a selective grant; drift is possible here")
	}
}

// A payload with no repository_selection must leave the column unknown rather
// than asserting a width nobody reported. Unknown is what the org page renders
// as "we have not established this", which is a different thing from "drift is
// impossible here" — the distinction the column exists to make.
func TestGitHubWebhook_InstallationCreated_OmittedSelectionStaysUnknown(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedWebhookApp(t, s)

	body := []byte(`{"action":"created","installation":{"id":4242,"account":{"login":"acme","type":"Organization"},"created_at":"2026-01-01T00:00:00Z"}}`)
	if rec := postWebhook(s, "installation", sign(body), body); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	if got := onlyInstallation(t, s); got.RepositorySelection != "" {
		t.Errorf("RepositorySelection = %q for a payload that omitted it; want \"\"", got.RepositorySelection)
	}
}
