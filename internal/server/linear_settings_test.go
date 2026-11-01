package server

import (
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/eventing/poller"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
	"github.com/zalando/go-keyring"
)

// TestOrgSettingsPatch_LinearPollInterval: the Linear cadence is an ordinary
// settings field, and a change re-dues the org's Linear poll. The workspace is
// learned from the credential, so the PATCH has no field for it.
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

	rec := patchOrgSettings(t, s, map[string]any{"linear_poll_interval": "soon"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an unparseable cadence = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}

	// A resave of the stored value changes nothing worth re-polling for.
	patchOrgSettingsOK(t, s, map[string]any{"linear_poll_interval": "15m0s"})
	if kicked() {
		t.Error("resaving the same Linear cadence re-dued Linear polling")
	}

	for _, field := range []string{"linear_workspace_id", "linear_workspace_url_key"} {
		rec := patchOrgSettings(t, s, map[string]any{field: "someone-elses"})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("PATCH naming %s = %d, want 400; body=%s", field, rec.Code, rec.Body.String())
		}
		httpxtest.AssertFirstError(t, rec, httpx.ReasonUnknownField, field)
	}
}

// TestOrgSettingsPatch_PollIntervalFloor: a cadence shorter than the
// scheduler's tick would be polled at the tick anyway, so every source's
// cadence below it is refused with the field named.
func TestOrgSettingsPatch_PollIntervalFloor(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	for _, field := range []string{"github_poll_interval", "jira_poll_interval", "linear_poll_interval"} {
		for _, short := range []string{"29s", "0s", "-5m"} {
			rec := patchOrgSettings(t, s, map[string]any{field: short})
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("%s = %q: status %d, want 422; body=%s", field, short, rec.Code, rec.Body.String())
			}
			httpxtest.AssertFirstError(t, rec, httpx.ReasonOutOfRange, field)
		}
		patchOrgSettingsOK(t, s, map[string]any{field: poller.MinPollInterval.String()})
		if got := orgSettingsSnapshot(t, s)[field]; got != "30s" {
			t.Errorf("%s after saving the floor = %v, want 30s", field, got)
		}
	}
}
