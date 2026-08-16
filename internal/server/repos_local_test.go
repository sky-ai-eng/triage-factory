package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
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
	repoID := seedConfiguredRepo(t, s, "acme", "api")

	rec := doJSON(t, s, http.MethodPatch, "/api/repos/"+repoID, map[string]string{"base_branch": "develop"})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH /api/repos/{id}: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
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

	// The single read answers with the same row the list does, addressed by
	// the id the list served.
	single := doJSON(t, s, http.MethodGet, "/api/repos/"+repoID, nil)
	if single.Code != http.StatusOK {
		t.Fatalf("GET /api/repos/{id}: status = %d, want 200; body=%s", single.Code, single.Body.String())
	}
	var got repoJSON
	if err := json.Unmarshal(single.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode single read: %v", err)
	}
	if got.ID != repoID || got.Slug != "acme/api" || got.BaseBranch != "develop" || !got.CanEdit {
		t.Errorf("single read = %+v, want the acme/api list row", got)
	}

	// by-name answers the same row, and is the only route that takes a name.
	byName := doJSON(t, s, http.MethodGet, "/api/repos/by-name/acme/api", nil)
	if byName.Code != http.StatusOK {
		t.Fatalf("GET /api/repos/by-name/acme/api: status = %d, want 200; body=%s", byName.Code, byName.Body.String())
	}
	var named repoJSON
	if err := json.Unmarshal(byName.Body.Bytes(), &named); err != nil {
		t.Fatalf("decode by-name read: %v", err)
	}
	if named != got {
		t.Errorf("by-name read = %+v, want the same row as the id read %+v", named, got)
	}

	if miss := doJSON(t, s, http.MethodGet, "/api/repos/by-name/acme/nope", nil); miss.Code != http.StatusNotFound {
		t.Errorf("GET an unconfigured repo by name = %d, want 404", miss.Code)
	}
	if miss := doJSON(t, s, http.MethodGet, "/api/repos/not-an-id", nil); miss.Code != http.StatusNotFound {
		t.Errorf("GET an unknown id = %d, want 404", miss.Code)
	}
	// A name where an id belongs resolves against ids, misses, and 404s.
	// There is no fallback to the by-name lookup.
	if miss := doJSON(t, s, http.MethodGet, "/api/repos/acme%2Fapi", nil); miss.Code != http.StatusNotFound {
		t.Errorf("GET a slug in id position = %d, want 404", miss.Code)
	}
}

// TestReadRepo_AnnotationIsOptional pins the can_edit annotation as something a
// read asks for rather than something it always pays. The branch list resolves
// a repository in order to *use* its name upstream and carries no can_edit
// field, and it is one call per keystroke on a type-to-filter box — so it must
// not spend a non-admin's extra EXISTS computing a value it discards.
//
// Asserted through the return value, which is the contract's observable half:
// noCanEdit answers false even for a caller who demonstrably may edit, so a
// caller that did not ask for the annotation cannot read one either.
func TestReadRepo_AnnotationIsOptional(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	repoID := seedConfiguredRepo(t, s, "acme", "api")

	lookup := func(ctx context.Context, tx db.TxStores) (*domain.Repository, error) {
		return repoByID(ctx, tx.Repos.Get, runmode.LocalDefaultOrgID, repoID)
	}
	orgID, userID := runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID

	row, canEdit, err := s.readRepo(t.Context(), orgID, userID, withCanEdit, lookup)
	if err != nil || row == nil {
		t.Fatalf("withCanEdit: row = %v, err = %v", row, err)
	}
	if !canEdit {
		t.Fatal("withCanEdit: can_edit = false; local mode's single user may edit")
	}

	row, canEdit, err = s.readRepo(t.Context(), orgID, userID, noCanEdit, lookup)
	if err != nil || row == nil {
		t.Fatalf("noCanEdit: row = %v, err = %v", row, err)
	}
	if canEdit {
		t.Error("noCanEdit: can_edit = true; the annotation was resolved for a caller that did not ask for it")
	}
}

// TestRepoAddressing_SurvivesRename is the invariant this addressing exists
// for: a repository renamed between the page render and the client's next
// call. The id the client is holding still resolves, so the PATCH lands and
// the read answers — where a name-addressed request would 404 on a name
// nothing answers to any more.
func TestRepoAddressing_SurvivesRename(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	repoID := seedConfiguredRepo(t, s, "acme", "api")

	// GitHub renames the repository. The registry row keeps its id.
	if _, err := s.db.ExecContext(t.Context(),
		`UPDATE repositories SET repo = 'api-v2' WHERE id = ?`, repoID); err != nil {
		t.Fatalf("rename repository: %v", err)
	}

	rec := doJSON(t, s, http.MethodPatch, "/api/repos/"+repoID, map[string]string{"base_branch": "develop"})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH after rename: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	single := doJSON(t, s, http.MethodGet, "/api/repos/"+repoID, nil)
	if single.Code != http.StatusOK {
		t.Fatalf("GET after rename: status = %d, want 200; body=%s", single.Code, single.Body.String())
	}
	var got repoJSON
	if err := json.Unmarshal(single.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Slug != "acme/api-v2" {
		t.Errorf("slug = %q, want the new name", got.Slug)
	}
	if got.BaseBranch != "develop" {
		t.Errorf("base_branch = %q; the id-keyed PATCH did not land on the renamed row", got.BaseBranch)
	}

	// The old name is gone, and by-name says so rather than resolving it.
	if stale := doJSON(t, s, http.MethodGet, "/api/repos/by-name/acme/api", nil); stale.Code != http.StatusNotFound {
		t.Errorf("by-name on the pre-rename name = %d, want 404", stale.Code)
	}
}
