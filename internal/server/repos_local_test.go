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

	page := decodeList[repoJSON](t, doJSON(t, s, http.MethodPost, "/api/repos/list", map[string]any{}))
	var found bool
	for _, row := range page.Items {
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
		t.Fatalf("acme/api missing from the repo list; body=%s", rec.Body.String())
	}

	// The single read answers with the same row the list does.
	single := doJSON(t, s, http.MethodGet, "/api/repos/acme/api", nil)
	if single.Code != http.StatusOK {
		t.Fatalf("GET /api/repos/acme/api: status = %d, want 200; body=%s", single.Code, single.Body.String())
	}
	var got repoJSON
	if err := json.Unmarshal(single.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode single read: %v", err)
	}
	if got.Owner != "acme" || got.Repo != "api" || got.BaseBranch != "develop" || !got.CanEdit {
		t.Errorf("single read = %+v, want the acme/api list row", got)
	}
	if miss := doJSON(t, s, http.MethodGet, "/api/repos/acme/nope", nil); miss.Code != http.StatusNotFound {
		t.Errorf("GET an unconfigured repo = %d, want 404", miss.Code)
	}
}
