package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestHandleRepoUpdate_LocalMode pins that tightening the repo-mutation gate
// to admins leaves local mode alone. N=1 has one implicit owner and no team
// boundary, so isOrgAdmin's local short-circuit resolves the gate before any
// team lookup: the single user still writes base_branch, and still sees the
// editable control (can_edit true).
func TestHandleRepoUpdate_LocalMode(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	seedConfiguredRepo(t, s, "acme", "api")

	rec := doJSON(t, s, http.MethodPatch, "/api/repos/acme/api", map[string]string{"base_branch": "develop"})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH /api/repos/acme/api: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, s, http.MethodGet, "/api/repos", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/repos: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var rows []struct {
		Owner      string `json:"owner"`
		Repo       string `json:"repo"`
		BaseBranch string `json:"base_branch"`
		CanEdit    bool   `json:"can_edit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode repo list: %v; body=%s", err, rec.Body.String())
	}
	var found bool
	for _, row := range rows {
		if row.Owner != "acme" || row.Repo != "api" {
			continue
		}
		found = true
		if row.BaseBranch != "develop" {
			t.Errorf("base_branch = %q, want develop (the local PATCH should have landed)", row.BaseBranch)
		}
		if !row.CanEdit {
			t.Error("can_edit = false in local mode; the single user must keep the editable control")
		}
	}
	if !found {
		t.Fatalf("acme/api missing from GET /api/repos; body=%s", rec.Body.String())
	}
}
