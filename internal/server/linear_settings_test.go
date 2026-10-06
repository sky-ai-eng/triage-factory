package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/zalando/go-keyring"
)

// TestOrgSettingsPatch_LinearPollInterval: the Linear cadence is an ordinary
// settings field with the bounds its siblings have, and a change re-dues the
// org's Linear poll.
func TestOrgSettingsPatch_LinearPollInterval(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	kicked := linearKicks(t, s)

	if got := orgSettingsSnapshot(t, s)["linear_poll_interval"]; got != "5m0s" {
		t.Errorf("default linear_poll_interval = %v, want 5m0s", got)
	}

	patchOrgSettingsOK(t, s, map[string]any{"linear_poll_interval": "15m"})
	if got := orgSettingsSnapshot(t, s)["linear_poll_interval"]; got != "15m0s" {
		t.Errorf("linear_poll_interval after the save = %v, want 15m0s", got)
	}
	if !kicked() {
		t.Error("changing the Linear cadence did not re-due Linear polling")
	}

	// The floor is the one the Jira cadence has: a value below it is refused
	// for both, with the field named.
	for _, field := range []string{"jira_poll_interval", "linear_poll_interval"} {
		rec := patchOrgSettings(t, s, map[string]any{field: "5s"})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("a 5s %s = %d, want 422; body=%s", field, rec.Code, rec.Body.String())
		}
		assertFirstError(t, rec, httpx.ReasonOutOfRange, field)
	}
	rec := patchOrgSettings(t, s, map[string]any{"linear_poll_interval": "soon"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an unparseable cadence = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}

	// A resave of the stored value changes nothing worth re-polling for.
	patchOrgSettingsOK(t, s, map[string]any{"linear_poll_interval": "15m0s"})
	if kicked() {
		t.Error("resaving the same Linear cadence re-dued Linear polling")
	}
}

// TestOrgSettingsPatch_LinearWorkspaceIsNotASetting: the workspace is learned
// from the bound credential, so the settings PATCH has no field for it — a
// caller naming one gets UNKNOWN_FIELD, and a save of something else carries
// the stored workspace through untouched.
func TestOrgSettingsPatch_LinearWorkspaceIsNotASetting(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)
	ctx := t.Context()

	// What a credential bind would have written.
	set, err := s.orgs.GetSettingsSystem(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	set.LinearWorkspaceID, set.LinearWorkspaceURLKey = "workspace-uuid", "acme"
	if _, err := s.orgs.UpdateSettings(ctx, runmode.LocalDefaultOrgID, set); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}

	for _, field := range []string{"linear_workspace_id", "linear_workspace_url_key"} {
		rec := patchOrgSettings(t, s, map[string]any{field: "someone-elses"})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("PATCH naming %s = %d, want 400; body=%s", field, rec.Code, rec.Body.String())
		}
		assertFirstError(t, rec, httpx.ReasonUnknownField, field)
	}

	patchOrgSettingsOK(t, s, map[string]any{"linear_poll_interval": "20m"})
	got, err := s.orgs.GetSettingsSystem(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if got.LinearWorkspaceID != "workspace-uuid" || got.LinearWorkspaceURLKey != "acme" {
		t.Errorf("a settings save moved the workspace: id=%q url_key=%q", got.LinearWorkspaceID, got.LinearWorkspaceURLKey)
	}
	if got.LinearPollInterval != 20*time.Minute {
		t.Errorf("LinearPollInterval = %v, want 20m", got.LinearPollInterval)
	}
}
