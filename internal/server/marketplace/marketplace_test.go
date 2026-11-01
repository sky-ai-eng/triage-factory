package marketplace

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// marketplaceRig is the multi-mode fixture for the marketplace handlers: a
// Postgres-backed handler (RLS enforced) with one org carrying a founder (write
// role on the default team), a plain member on a second team, and a viewer on
// the founder's team.
type marketplaceRig struct {
	h      *pgtest.Harness
	mh     *Handler
	orgID  string
	teamID string
	admin  string // founder — write role on teamID
	viewer string // viewer role on teamID
	teamB  string
	member string // write role on teamB, no role on teamID
}

func newMarketplaceRig(t *testing.T) *marketplaceRig {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)

	h := pgtest.Shared(t)
	h.Reset(t)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	orgID, founder, teamID := pgtest.SeedOrgWithUser(t, h, "founder")
	viewer := pgtest.SeedUser(t, h, "viewer")
	pgtest.AddOrgMember(t, h, viewer, orgID, teamID, "member", "viewer")
	teamB := pgtest.SeedTeam(t, h, orgID, "teamB")
	member := pgtest.SeedUser(t, h, "member")
	pgtest.AddOrgMember(t, h, member, orgID, teamB, "member", "member")

	return &marketplaceRig{
		h:      h,
		mh:     New(stores.Tx, authz.New(h.AdminDB, stores.Tx)),
		orgID:  orgID,
		teamID: teamID,
		admin:  founder,
		viewer: viewer,
		teamB:  teamB,
		member: member,
	}
}

// req builds a request to path as callerID with claims + active org injected.
func (r *marketplaceRig) req(method, path, callerID string, body any) *http.Request {
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	ctx := httpx.WithClaims(req.Context(), &verify.Claims{Subject: callerID})
	ctx = httpx.WithOrgID(ctx, r.orgID)
	return req.WithContext(ctx)
}

// seedPrompt inserts a team prompt via the admin pool and returns its id.
func (r *marketplaceRig) seedPrompt(t *testing.T, teamID, name, body, model, allowedTools string) string {
	t.Helper()
	var id string
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO prompts (org_id, creator_user_id, team_id, name, body, source, model, allowed_tools)
		VALUES ($1, $2, $3, $4, $5, 'user', $6, $7) RETURNING id
	`, r.orgID, r.admin, teamID, name, body, model, allowedTools).Scan(&id); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	return id
}

// seedBlueprint inserts a team blueprint (no steps) via the admin pool.
func (r *marketplaceRig) seedBlueprint(t *testing.T, teamID, name string) string {
	t.Helper()
	var id string
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO blueprints (org_id, creator_user_id, team_id, name, source)
		VALUES ($1, $2, $3, $4, 'user') RETURNING id
	`, r.orgID, r.admin, teamID, name).Scan(&id); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	return id
}

// seedBlueprintStep attaches a step to a blueprint at stepIndex.
func (r *marketplaceRig) seedBlueprintStep(t *testing.T, teamID, blueprintID string, stepIndex int, promptID, brief string) {
	t.Helper()
	pgtest.MustExec(t, r.h.AdminDB, `
		INSERT INTO blueprint_steps (org_id, team_id, blueprint_id, step_index, step_prompt_id, brief)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, r.orgID, teamID, blueprintID, stepIndex, promptID, brief)
}

// seedInstall inserts a marketplace_installs row directly, so a sort test can
// set install counts without running the copy-to-team flow.
func (r *marketplaceRig) seedInstall(t *testing.T, listingID, teamID, userID string) {
	t.Helper()
	pgtest.MustExec(t, r.h.AdminDB, `
		INSERT INTO marketplace_installs (listing_id, org_id, version, team_id, user_id, created_at)
		VALUES ($1, $2, 1, $3, $4, now())
	`, listingID, r.orgID, teamID, userID)
}

// seedStats writes a marketplace_listing_stats row directly; the aggregation
// that computes it is pinned in internal/db/pgtest/marketplace_stats_test.go.
func (r *marketplaceRig) seedStats(t *testing.T, listingID string, teamsUsing, totalRuns int) {
	t.Helper()
	pgtest.MustExec(t, r.h.AdminDB, `
		INSERT INTO marketplace_listing_stats (listing_id, org_id, teams_using, total_runs, computed_at)
		VALUES ($1, $2, $3, $4, now())
	`, listingID, r.orgID, teamsUsing, totalRuns)
}

// getListingDetail reads a listing back through the store as callerID, to
// assert on the built snapshot without duplicating the store's scan logic.
func (r *marketplaceRig) getListingDetail(t *testing.T, callerID, listingID string) domain.ListingDetail {
	t.Helper()
	var detail domain.ListingDetail
	if err := r.h.WithUser(t, callerID, r.orgID, func(tx *sql.Tx) error {
		var e error
		detail, e = pgstore.NewForTx(tx, pgtest.SecretKey).Marketplace.Get(t.Context(), r.orgID, listingID, callerID)
		return e
	}); err != nil {
		t.Fatalf("get listing %s: %v", listingID, err)
	}
	return detail
}

// eventTypeFixture is a real seeded events_catalog id — other suites (e.g.
// pgtest/marketplace_test.go) use the same id, so it's guaranteed present.
const eventTypeFixture = "github:pr:opened"

func doMarketplaceJSON(fn func(http.ResponseWriter, *http.Request), req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	fn(rec, req)
	return rec
}

// publishListing publishes body as callerID through the handler and returns
// the new listing's id.
func (r *marketplaceRig) publishListing(t *testing.T, callerID string, body map[string]any) string {
	t.Helper()
	rec := doMarketplaceJSON(r.mh.handleMarketplacePublish, r.req(http.MethodPost, "/api/marketplace/listings", callerID, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish %v: status = %d, want 201; body=%s", body, rec.Code, rec.Body.String())
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode publish response: %v", err)
	}
	return resp.ID
}

// publishPrompt seeds a prompt on the founder's team and publishes it as a
// prompt-kind listing named listingName.
func (r *marketplaceRig) publishPrompt(t *testing.T, name, body, model, tools, listingName string) string {
	t.Helper()
	return r.publishListing(t, r.admin, map[string]any{
		"kind": "prompt", "source_id": r.seedPrompt(t, r.teamID, name, body, model, tools), "name": listingName,
	})
}

// delist delists listingID as the founder.
func (r *marketplaceRig) delist(t *testing.T, listingID string) {
	t.Helper()
	req := r.req(http.MethodPost, "/api/marketplace/listings/"+listingID+"/delist", r.admin, nil)
	req.SetPathValue("id", listingID)
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceListingDelist, req); rec.Code != http.StatusNoContent {
		t.Fatalf("delist: status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
}

// list calls the browse list as callerID; query is a k=v&k=v string turned
// into the request body.
func (r *marketplaceRig) list(t *testing.T, callerID, query string) []domain.ListingSummary {
	t.Helper()
	body := map[string]any{}
	for _, kv := range strings.Split(query, "&") {
		if kv == "" {
			continue
		}
		k, val, _ := strings.Cut(kv, "=")
		decoded, err := url.QueryUnescape(val)
		if err != nil {
			t.Fatalf("unescape %q: %v", val, err)
		}
		body[k] = decoded
	}
	rec := doMarketplaceJSON(r.mh.handleMarketplaceList, r.req(http.MethodPost, "/api/marketplace/listings/list", callerID, body))
	return httpxtest.DecodeList[domain.ListingSummary](t, rec).Items
}

func listingIDs(summaries []domain.ListingSummary) []string {
	ids := make([]string, len(summaries))
	for i, s := range summaries {
		ids[i] = s.ID
	}
	return ids
}

func decodeListingDetail(t *testing.T, rec *httptest.ResponseRecorder) domain.ListingDetail {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var detail domain.ListingDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return detail
}

// installResponse mirrors installListingResponse — the wire shape the
// install tests below decode.
type installResponse struct {
	Kind      string   `json:"kind"`
	ID        string   `json:"id"`
	PromptIDs []string `json:"prompt_ids"`
}

// installAs POSTs .../install as callerID with the given team_id and
// returns the recorder.
func (r *marketplaceRig) installAs(callerID, listingID, teamID string) *httptest.ResponseRecorder {
	req := r.req(http.MethodPost, "/api/marketplace/listings/"+listingID+"/install", callerID, map[string]any{
		"team_id": teamID,
	})
	req.SetPathValue("id", listingID)
	return doMarketplaceJSON(r.mh.handleMarketplaceInstall, req)
}

// TestMarketplace_LocalIs404: every endpoint in the family 404s in local
// mode, regardless of caller identity.
func TestMarketplace_LocalIs404(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "local-gate", "mission", "", "")
	runmode.SetForTest(t, runmode.ModeLocal)

	for _, tc := range []struct {
		name string
		req  *http.Request
		fn   func(http.ResponseWriter, *http.Request)
	}{
		{"publish", r.req(http.MethodPost, "/api/marketplace/listings", r.admin,
			map[string]any{"kind": "prompt", "source_id": promptID, "name": "n"}), r.mh.handleMarketplacePublish},
		{"versions", r.req(http.MethodPost, "/api/marketplace/listings/x/versions", r.admin,
			map[string]any{"name": "n"}), r.mh.handleMarketplaceListingVersionCreate},
		{"delist", r.req(http.MethodPost, "/api/marketplace/listings/x/delist", r.admin, nil), r.mh.handleMarketplaceListingDelist},
		{"relist", r.req(http.MethodPost, "/api/marketplace/listings/x/relist", r.admin, nil), r.mh.handleMarketplaceListingRelist},
		{"by-source", r.req(http.MethodGet, "/api/marketplace/listings/by-source/"+promptID, r.admin, nil), r.mh.handleMarketplaceListingBySource},
		{"list", r.req(http.MethodGet, "/api/marketplace/listings", r.admin, nil), r.mh.handleMarketplaceList},
		{"get", r.req(http.MethodGet, "/api/marketplace/listings/x", r.admin, nil), r.mh.handleMarketplaceGet},
		{"vote", r.req(http.MethodPut, "/api/marketplace/listings/x/vote", r.admin, nil), r.mh.handleMarketplaceVote},
		{"unvote", r.req(http.MethodDelete, "/api/marketplace/listings/x/vote", r.admin, nil), r.mh.handleMarketplaceUnvote},
		{"install", r.req(http.MethodPost, "/api/marketplace/listings/x/install", r.admin,
			map[string]any{"team_id": ""}), r.mh.handleMarketplaceInstall},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.SetPathValue("id", "x")
			tc.req.SetPathValue("source_id", promptID)
			rec := doMarketplaceJSON(tc.fn, tc.req)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s: status = %d, want 404 in local mode; body=%s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestMarketplace_NoToggleGate: org_settings.marketplace_enabled defaults false
// and is reserved for a cross-org gate; the within-org marketplace works
// regardless.
func TestMarketplace_NoToggleGate(t *testing.T) {
	r := newMarketplaceRig(t)
	// The pgtest org fixture materializes no org_settings row; insert one so the
	// column's DEFAULT false is what the handlers run under.
	pgtest.MustExec(t, r.h.AdminDB, `INSERT INTO org_settings (org_id) VALUES ($1)`, r.orgID)
	var enabled bool
	if err := r.h.AdminDB.QueryRow(`SELECT marketplace_enabled FROM org_settings WHERE org_id = $1`, r.orgID).Scan(&enabled); err != nil {
		t.Fatalf("read marketplace_enabled: %v", err)
	}
	if enabled {
		t.Fatalf("marketplace_enabled = true by default, want false (ship-dark default, unrelated to this surface)")
	}

	promptID := r.seedPrompt(t, r.teamID, "no-gate", "mission", "", "")
	rec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.admin,
			map[string]any{"kind": "prompt", "source_id": promptID, "name": "n"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish with marketplace_enabled=false: status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}

	listReq := r.req(http.MethodPost, "/api/marketplace/listings/list", r.admin, map[string]any{})
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceList, listReq); rec.Code != http.StatusOK {
		t.Fatalf("list with marketplace_enabled=false: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestMarketplacePublish_NonWriterIs403: a viewer on the source object's team
// cannot publish it — 403 view-only, and nothing is created.
func TestMarketplacePublish_NonWriterIs403(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "guarded", "mission", "", "")

	rec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.viewer, map[string]any{
			"kind": "prompt", "source_id": promptID, "name": "n",
		}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer publish: status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}

	var count int
	if err := r.h.AdminDB.QueryRow(`SELECT COUNT(*) FROM marketplace_listings WHERE source_id = $1`, promptID).Scan(&count); err != nil {
		t.Fatalf("count listings: %v", err)
	}
	if count != 0 {
		t.Errorf("listing count = %d after blocked viewer publish, want 0", count)
	}
}

// TestMarketplacePublish_CrossTeamIs404: a writer on another team cannot see the
// source prompt under RLS, so publishing it is a 404 rather than the same-team
// viewer's 403, and a prober learns nothing about the prompt.
func TestMarketplacePublish_CrossTeamIs404(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "not-yours", "mission", "", "")

	rec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.member, map[string]any{
			"kind": "prompt", "source_id": promptID, "name": "n",
		}))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-team publish: status = %d, want 404 (invisible under RLS, not merely unwritable); body=%s", rec.Code, rec.Body.String())
	}
}

// TestMarketplacePublish_DuplicateActiveSourceIs409: a source that already
// has an active listing can't be published again — the client should have
// offered republish.
func TestMarketplacePublish_DuplicateActiveSourceIs409(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "dup", "mission", "", "")
	r.publishListing(t, r.admin, map[string]any{"kind": "prompt", "source_id": promptID, "name": "n"})

	second := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.admin, map[string]any{
			"kind": "prompt", "source_id": promptID, "name": "n again",
		}))
	if second.Code != http.StatusConflict {
		t.Fatalf("second publish: status = %d, want 409; body=%s", second.Code, second.Body.String())
	}
}

// TestMarketplacePublish_UnknownEventTypeIs400: an event_type not present in
// events_catalog is rejected before anything is written.
func TestMarketplacePublish_UnknownEventTypeIs400(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "bad-event", "mission", "", "")

	rec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.admin, map[string]any{
			"kind": "prompt", "source_id": promptID, "name": "n",
			"event_types": []string{"not:a:real:event"},
		}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown event_type: status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestMarketplacePublish_ValidationErrors covers unknown-kind and missing-field validation.
func TestMarketplacePublish_ValidationErrors(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "validate-me", "mission", "", "")

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"bad kind", map[string]any{"kind": "widget", "source_id": promptID, "name": "n"}},
		{"missing source_id", map[string]any{"kind": "prompt", "name": "n"}},
		{"missing name", map[string]any{"kind": "prompt", "source_id": promptID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
				r.req(http.MethodPost, "/api/marketplace/listings", r.admin, tc.body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s: status = %d, want 400; body=%s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestMarketplacePublish_SourceNotFoundIs404: a source_id that doesn't
// resolve to a live object in the caller's org 404s rather than 500ing or
// silently publishing an empty snapshot.
func TestMarketplacePublish_SourceNotFoundIs404(t *testing.T) {
	r := newMarketplaceRig(t)

	rec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.admin, map[string]any{
			"kind": "prompt", "source_id": "00000000-0000-0000-0000-000000000000", "name": "n",
		}))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("nonexistent source: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestMarketplaceRepublish_BumpsVersionPreservesV1: republishing rebuilds the
// snapshot from the *current* state of the source object, appends it as
// version 2, bumps current_version, and leaves version 1's frozen snapshot
// untouched.
func TestMarketplaceRepublish_BumpsVersionPreservesV1(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "v1 name", "v1 body", "sonnet", "")
	listingID := r.publishListing(t, r.admin, map[string]any{
		"kind": "prompt", "source_id": promptID, "name": "Listing v1", "description": "d1",
	})

	// Edit the live prompt — the republish must pick up this new content, not
	// what was frozen into v1.
	pgtest.MustExec(t, r.h.AdminDB, `UPDATE prompts SET name = $1, body = $2 WHERE id = $3`,
		"v2 name", "v2 body", promptID)

	repubReq := r.req(http.MethodPost, "/api/marketplace/listings/"+listingID+"/versions", r.admin, map[string]any{
		"name": "Listing v2", "description": "d2", "event_types": []string{eventTypeFixture},
	})
	repubReq.SetPathValue("id", listingID)
	repubRec := doMarketplaceJSON(r.mh.handleMarketplaceListingVersionCreate, repubReq)
	if repubRec.Code != http.StatusOK {
		t.Fatalf("republish: status = %d, want 200; body=%s", repubRec.Code, repubRec.Body.String())
	}
	var updated domain.MarketplaceListing
	if err := json.Unmarshal(repubRec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if updated.CurrentVersion != 2 {
		t.Fatalf("republish response current_version = %d, want 2 (the resource, not a bare version echo)", updated.CurrentVersion)
	}

	detail := r.getListingDetail(t, r.admin, listingID)
	if detail.CurrentVersion != 2 {
		t.Errorf("current_version = %d, want 2", detail.CurrentVersion)
	}
	if detail.Name != "Listing v2" || detail.Description != "d2" {
		t.Errorf("listing name/description = %q/%q after republish, want the v2 values", detail.Name, detail.Description)
	}
	if len(detail.Versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(detail.Versions))
	}
	v1 := detail.Versions[0]
	if v1.Version != 1 || v1.Snapshot.Name != "Listing v1" || v1.Snapshot.Steps[0].Name != "v1 name" || v1.Snapshot.Steps[0].Body != "v1 body" {
		t.Errorf("v1 snapshot = %+v, want the original frozen content (untouched by the republish)", v1)
	}
	v2 := detail.Versions[1]
	if v2.Version != 2 || v2.Snapshot.Steps[0].Name != "v2 name" || v2.Snapshot.Steps[0].Body != "v2 body" {
		t.Errorf("v2 snapshot = %+v, want the edited prompt's current content", v2)
	}
	if detail.CurrentSnapshot.Steps[0].Name != "v2 name" {
		t.Errorf("current_snapshot = %+v, want the v2 content", detail.CurrentSnapshot)
	}
}

// TestMarketplaceRepublish_NonWriterIs403: only a writer on the *publisher*
// team may republish.
func TestMarketplaceRepublish_NonWriterIs403(t *testing.T) {
	r := newMarketplaceRig(t)
	listingID := r.publishPrompt(t, "guarded", "mission", "", "", "n")

	repubReq := r.req(http.MethodPost, "/api/marketplace/listings/"+listingID+"/versions", r.viewer, map[string]any{"name": "n2"})
	repubReq.SetPathValue("id", listingID)
	rec := doMarketplaceJSON(r.mh.handleMarketplaceListingVersionCreate, repubReq)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer republish: status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// TestMarketplaceDelistRelist_RoundTrip: a publisher-team writer can delist,
// which keeps the listing but drops it from every member's browse, the
// publisher's included, and relist it; a viewer on the publisher team cannot.
func TestMarketplaceDelistRelist_RoundTrip(t *testing.T) {
	r := newMarketplaceRig(t)
	listingID := r.publishPrompt(t, "delist-me", "mission", "", "", "n")

	// Viewer cannot delist.
	blockedReq := r.req(http.MethodPost, "/api/marketplace/listings/"+listingID+"/delist", r.viewer, nil)
	blockedReq.SetPathValue("id", listingID)
	blockedRec := doMarketplaceJSON(r.mh.handleMarketplaceListingDelist, blockedReq)
	if blockedRec.Code != http.StatusForbidden {
		t.Fatalf("viewer delist: status = %d, want 403; body=%s", blockedRec.Code, blockedRec.Body.String())
	}

	r.delist(t, listingID)
	var status string
	if err := r.h.AdminDB.QueryRow(`SELECT status FROM marketplace_listings WHERE id = $1`, listingID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != domain.ListingStatusDelisted {
		t.Fatalf("status after delist = %q, want delisted", status)
	}
	for _, caller := range []string{r.member, r.admin} {
		if got := r.list(t, caller, ""); len(got) != 0 {
			t.Errorf("browse as %s after delist = %+v, want empty", caller, got)
		}
	}

	// Founder relists.
	relistReq := r.req(http.MethodPost, "/api/marketplace/listings/"+listingID+"/relist", r.admin, nil)
	relistReq.SetPathValue("id", listingID)
	relistRec := doMarketplaceJSON(r.mh.handleMarketplaceListingRelist, relistReq)
	if relistRec.Code != http.StatusNoContent {
		t.Fatalf("relist: status = %d, want 204; body=%s", relistRec.Code, relistRec.Body.String())
	}
	if err := r.h.AdminDB.QueryRow(`SELECT status FROM marketplace_listings WHERE id = $1`, listingID).Scan(&status); err != nil {
		t.Fatalf("read status after relist: %v", err)
	}
	if status != domain.ListingStatusPublished {
		t.Fatalf("status after relist = %q, want published", status)
	}
	if got := r.list(t, r.member, ""); len(got) != 1 || got[0].ID != listingID {
		t.Errorf("browse as a member after relist = %+v, want the listing back", got)
	}
}

// TestMarketplacePublish_DuplicateDelistedSourceIs409: a source whose listing
// was delisted is not published as a second listing; the handler finds the
// source's listing whatever its status and answers 409, steering to relist.
func TestMarketplacePublish_DuplicateDelistedSourceIs409(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "dup-delisted", "mission", "", "")
	r.delist(t, r.publishListing(t, r.admin, map[string]any{"kind": "prompt", "source_id": promptID, "name": "n"}))

	secondRec := doMarketplaceJSON(r.mh.handleMarketplacePublish,
		r.req(http.MethodPost, "/api/marketplace/listings", r.admin, map[string]any{
			"kind": "prompt", "source_id": promptID, "name": "n again",
		}))
	if secondRec.Code != http.StatusConflict {
		t.Fatalf("publish over a delisted source: status = %d, want 409; body=%s", secondRec.Code, secondRec.Body.String())
	}

	var count int
	if err := r.h.AdminDB.QueryRow(`SELECT COUNT(*) FROM marketplace_listings WHERE source_id = $1`, promptID).Scan(&count); err != nil {
		t.Fatalf("count listings: %v", err)
	}
	if count != 1 {
		t.Fatalf("listing count for source = %d after blocked second publish, want 1 (no duplicate)", count)
	}
}

// TestMarketplaceListingBySource_RoundTrip: the by-source lookup 404s before
// publish, then serves the listing with its full event-type set (the republish
// dialog seeds from it, and a republish replaces the set), and keeps serving it
// once delisted so the editor offers Relist rather than a duplicate Publish.
func TestMarketplaceListingBySource_RoundTrip(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "lookup-me", "mission", "", "")
	bySource := func() *httptest.ResponseRecorder {
		req := r.req(http.MethodGet, "/api/marketplace/listings/by-source/"+promptID, r.admin, nil)
		req.SetPathValue("source_id", promptID)
		return doMarketplaceJSON(r.mh.handleMarketplaceListingBySource, req)
	}
	served := func() domain.ListingSummary {
		t.Helper()
		rec := bySource()
		if rec.Code != http.StatusOK {
			t.Fatalf("by-source: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var listing domain.ListingSummary
		if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return listing
	}

	if rec := bySource(); rec.Code != http.StatusNotFound {
		t.Fatalf("by-source before publish: status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	const secondEventType = "github:pr:ci_check_failed"
	listingID := r.publishListing(t, r.admin, map[string]any{
		"kind": "prompt", "source_id": promptID, "name": "n",
		"event_types": []string{eventTypeFixture, secondEventType},
	})
	listing := served()
	if listing.ID != listingID {
		t.Errorf("by-source id = %q, want %q", listing.ID, listingID)
	}
	got := slices.Clone(listing.EventTypes)
	sort.Strings(got)
	want := []string{eventTypeFixture, secondEventType}
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("by-source event_types = %v, want %v", got, want)
	}

	r.delist(t, listingID)
	if listing := served(); listing.ID != listingID || listing.Status != domain.ListingStatusDelisted {
		t.Errorf("by-source after delist = %q/%q, want %q/delisted", listing.ID, listing.Status, listingID)
	}
}

// TestMarketplaceList_Filters: browse filters narrow the list — query to a name
// or description match, event_type to listings faceted on it, kind to prompts
// or blueprints — and every row names its publisher team, readable by any member.
func TestMarketplaceList_Filters(t *testing.T) {
	r := newMarketplaceRig(t)
	const otherEventType = "github:pr:ci_check_failed"
	triage := r.publishListing(t, r.admin, map[string]any{
		"kind": "prompt", "source_id": r.seedPrompt(t, r.teamID, "Triage Helper", "m", "", ""),
		"name": "Triage Helper", "description": "helps triage", "event_types": []string{eventTypeFixture},
	})
	deploy := r.publishListing(t, r.admin, map[string]any{
		"kind": "prompt", "source_id": r.seedPrompt(t, r.teamID, "Deploy Bot", "m", "", ""),
		"name": "Deploy Bot", "description": "deploys things", "event_types": []string{otherEventType},
	})
	bp := r.seedBlueprint(t, r.teamID, "Chain")
	r.seedBlueprintStep(t, r.teamID, bp, 0, r.seedPrompt(t, r.teamID, "Chain Step", "m", "", ""), "")
	chain := r.publishListing(t, r.admin, map[string]any{"kind": "blueprint", "source_id": bp, "name": "Chain"})

	for _, tc := range []struct {
		name, query string
		want        []string
	}{
		{"no filter", "", []string{triage, deploy, chain}},
		{"query", "query=triage", []string{triage}},
		{"event type", "event_type=" + eventTypeFixture, []string{triage}},
		{"prompt kind", "kind=prompt", []string{triage, deploy}},
		{"blueprint kind", "kind=blueprint", []string{chain}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := r.list(t, r.member, tc.query)
			ids, want := listingIDs(got), slices.Clone(tc.want)
			sort.Strings(ids)
			sort.Strings(want)
			if !slices.Equal(ids, want) {
				t.Fatalf("%s: ids = %v, want %v", tc.query, ids, want)
			}
			for _, l := range got {
				if l.PublisherTeamName != "default" {
					t.Errorf("publisher_team_name = %q, want %q (the seeded team's name)", l.PublisherTeamName, "default")
				}
			}
		})
	}
}

// TestMarketplaceList_InvalidKindOrSortIs400 rejects an unrecognized kind or
// sort value before ever reaching the store.
func TestMarketplaceList_InvalidKindOrSortIs400(t *testing.T) {
	r := newMarketplaceRig(t)
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceList, r.req(http.MethodPost, "/api/marketplace/listings/list", r.admin, map[string]any{"kind": "widget"})); rec.Code != http.StatusBadRequest {
		t.Errorf("bad kind: status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceList, r.req(http.MethodPost, "/api/marketplace/listings/list", r.admin, map[string]any{"sort": "popularity"})); rec.Code != http.StatusBadRequest {
		t.Errorf("bad sort: status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestMarketplaceList_SortOrders pins each accepted sort order, and the
// default, against listings with distinguishable install counts, vote counts,
// run counts and recency.
func TestMarketplaceList_SortOrders(t *testing.T) {
	r := newMarketplaceRig(t)
	pOld := r.seedPrompt(t, r.teamID, "Old One", "m", "", "")
	pMid := r.seedPrompt(t, r.teamID, "Mid One", "m", "", "")
	pNew := r.seedPrompt(t, r.teamID, "New One", "m", "", "")
	old := r.publishListing(t, r.admin, map[string]any{"kind": "prompt", "source_id": pOld, "name": "Old One"})
	mid := r.publishListing(t, r.admin, map[string]any{"kind": "prompt", "source_id": pMid, "name": "Mid One"})
	fresh := r.publishListing(t, r.admin, map[string]any{"kind": "prompt", "source_id": pNew, "name": "New One"})

	// Recency: bump updated_at on each in publish order so fresh > mid > old.
	pgtest.MustExec(t, r.h.AdminDB, `UPDATE marketplace_listings SET updated_at = now() - interval '2 hours' WHERE id = $1`, old)
	pgtest.MustExec(t, r.h.AdminDB, `UPDATE marketplace_listings SET updated_at = now() - interval '1 hour' WHERE id = $1`, mid)
	pgtest.MustExec(t, r.h.AdminDB, `UPDATE marketplace_listings SET updated_at = now() WHERE id = $1`, fresh)

	// Installs: old has the most, fresh has none.
	r.seedInstall(t, old, r.teamB, r.member)
	r.seedInstall(t, old, r.teamID, r.admin)
	r.seedInstall(t, mid, r.teamB, r.member)

	// Votes: mid has the most, old has none.
	voteReq := func(listingID, userID string) *http.Request {
		req := r.req(http.MethodPut, "/api/marketplace/listings/"+listingID+"/vote", userID, nil)
		req.SetPathValue("id", listingID)
		return req
	}
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceVote, voteReq(mid, r.admin)); rec.Code != http.StatusNoContent {
		t.Fatalf("vote mid/admin: status = %d, want 204", rec.Code)
	}
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceVote, voteReq(mid, r.viewer)); rec.Code != http.StatusNoContent {
		t.Fatalf("vote mid/viewer: status = %d, want 204", rec.Code)
	}
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceVote, voteReq(fresh, r.admin)); rec.Code != http.StatusNoContent {
		t.Fatalf("vote fresh/admin: status = %d, want 204", rec.Code)
	}

	if ids := listingIDs(r.list(t, r.admin, "sort=installs")); len(ids) != 3 || ids[0] != old || ids[1] != mid || ids[2] != fresh {
		t.Fatalf("sort=installs: ids = %v, want [%s %s %s] (2, 1, 0 installs)", ids, old, mid, fresh)
	}
	if ids := listingIDs(r.list(t, r.admin, "sort=votes")); len(ids) != 3 || ids[0] != mid || ids[1] != fresh || ids[2] != old {
		t.Fatalf("sort=votes: ids = %v, want [%s %s %s] (2, 1, 0 votes)", ids, mid, fresh, old)
	}
	if ids := listingIDs(r.list(t, r.admin, "sort=recent")); len(ids) != 3 || ids[0] != fresh || ids[1] != mid || ids[2] != old {
		t.Fatalf("sort=recent: ids = %v, want [%s %s %s] (newest updated_at first)", ids, fresh, mid, old)
	}

	// Run-derived usage: fresh has the most total_runs despite having zero
	// installs/votes, old has none — sort=used must order purely on
	// total_runs, independent of the other two signals.
	r.seedStats(t, old, 2, 0)
	r.seedStats(t, mid, 1, 3)
	r.seedStats(t, fresh, 1, 7)
	if ids := listingIDs(r.list(t, r.admin, "sort=used")); len(ids) != 3 || ids[0] != fresh || ids[1] != mid || ids[2] != old {
		t.Fatalf("sort=used: ids = %v, want [%s %s %s] (7, 3, 0 total_runs)", ids, fresh, mid, old)
	}
	// Default (sort omitted) matches "installs" — the handler pins this
	// explicitly rather than falling through to the store's own internal
	// default (recent), so the HTTP contract doesn't depend on the store.
	if ids := listingIDs(r.list(t, r.admin, "")); len(ids) != 3 || ids[0] != old || ids[1] != mid || ids[2] != fresh {
		t.Fatalf("default sort: ids = %v, want [%s %s %s] (installs)", ids, old, mid, fresh)
	}
}

// TestMarketplaceGet_DelistedVisibleToPublisherOnly: the detail endpoint
// (unlike List) still resolves a delisted listing for the publisher team —
// via RLS, not a status filter — so the manage/relist flow keeps working;
// a non-publisher member gets 404, same as any other invisible row.
func TestMarketplaceGet_DelistedVisibleToPublisherOnly(t *testing.T) {
	r := newMarketplaceRig(t)
	id := r.publishPrompt(t, "manage-me", "m", "", "", "manage-me")
	r.delist(t, id)

	getReq := r.req(http.MethodGet, "/api/marketplace/listings/"+id, r.admin, nil)
	getReq.SetPathValue("id", id)
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceGet, getReq); rec.Code != http.StatusOK {
		t.Fatalf("publisher's get(delisted): status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	memberReq := r.req(http.MethodGet, "/api/marketplace/listings/"+id, r.member, nil)
	memberReq.SetPathValue("id", id)
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceGet, memberReq); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-team member's get(delisted): status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestMarketplace_UnknownAndMalformedListingIDs: a listing id that is not a
// UUID, or names no listing, is a 404 on every listing route rather than a 500
// from the uuid cast or a foreign key. Unvote is the one exception.
func TestMarketplace_UnknownAndMalformedListingIDs(t *testing.T) {
	r := newMarketplaceRig(t)
	const unknownID = "00000000-0000-0000-0000-000000000000"
	for _, tc := range []struct {
		name, method, suffix, id string
		body                     any
		fn                       func(http.ResponseWriter, *http.Request)
		want                     int
	}{
		{name: "get unknown", method: http.MethodGet, id: unknownID, fn: r.mh.handleMarketplaceGet, want: http.StatusNotFound},
		{name: "get malformed", method: http.MethodGet, id: "not-a-uuid", fn: r.mh.handleMarketplaceGet, want: http.StatusNotFound},
		{name: "vote unknown", method: http.MethodPut, suffix: "/vote", id: unknownID, fn: r.mh.handleMarketplaceVote, want: http.StatusNotFound},
		{name: "vote malformed", method: http.MethodPut, suffix: "/vote", id: "not-a-uuid", fn: r.mh.handleMarketplaceVote, want: http.StatusNotFound},
		{name: "unvote malformed", method: http.MethodDelete, suffix: "/vote", id: "not-a-uuid", fn: r.mh.handleMarketplaceUnvote, want: http.StatusNotFound},
		// Unvote's DELETE has no foreign key to trip: an unknown id is the same
		// no-op as removing a vote that does not exist.
		{name: "unvote unknown", method: http.MethodDelete, suffix: "/vote", id: unknownID, fn: r.mh.handleMarketplaceUnvote, want: http.StatusNoContent},
		{name: "install unknown", method: http.MethodPost, suffix: "/install", id: unknownID, body: map[string]any{"team_id": ""},
			fn: r.mh.handleMarketplaceInstall, want: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := r.req(tc.method, "/api/marketplace/listings/"+tc.id+tc.suffix, r.admin, tc.body)
			req.SetPathValue("id", tc.id)
			if rec := doMarketplaceJSON(tc.fn, req); rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	// Vote checks the listing before its INSERT, so nothing reaches the table.
	var count int
	if err := r.h.AdminDB.QueryRow(`SELECT COUNT(*) FROM marketplace_votes WHERE listing_id = $1`, unknownID).Scan(&count); err != nil {
		t.Fatalf("count votes: %v", err)
	}
	if count != 0 {
		t.Errorf("vote count for unknown listing = %d, want 0 (nothing written)", count)
	}
}

// TestMarketplaceGet_IncludesSnapshotAndVersions: the detail response carries
// the current snapshot, built from the source prompt's content with no ids or
// brief, and the version history, so a republish shows as v2 beside v1.
func TestMarketplaceGet_IncludesSnapshotAndVersions(t *testing.T) {
	r := newMarketplaceRig(t)
	promptID := r.seedPrompt(t, r.teamID, "v1", "v1 body", "sonnet", "Bash")
	id := r.publishListing(t, r.admin, map[string]any{
		"kind": "prompt", "source_id": promptID, "name": "Detail Listing", "description": "d1",
		"event_types": []string{eventTypeFixture},
	})

	repubReq := r.req(http.MethodPost, "/api/marketplace/listings/"+id+"/versions", r.admin, map[string]any{
		"name": "Detail Listing v2", "description": "d2", "event_types": []string{eventTypeFixture},
	})
	repubReq.SetPathValue("id", id)
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceListingVersionCreate, repubReq); rec.Code != http.StatusOK {
		t.Fatalf("republish: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	getReq := r.req(http.MethodGet, "/api/marketplace/listings/"+id, r.member, nil)
	getReq.SetPathValue("id", id)
	detail := decodeListingDetail(t, doMarketplaceJSON(r.mh.handleMarketplaceGet, getReq))
	if detail.Name != "Detail Listing v2" || detail.Description != "d2" {
		t.Errorf("name/description = %q/%q, want the republished v2 values", detail.Name, detail.Description)
	}
	if detail.Kind != domain.ListingKindPrompt || detail.PublisherTeamID != r.teamID {
		t.Errorf("kind/publisher_team_id = %q/%q, want prompt/%q (the prompt's own team)", detail.Kind, detail.PublisherTeamID, r.teamID)
	}
	if len(detail.CurrentSnapshot.Steps) != 1 {
		t.Fatalf("current_snapshot = %+v, want one step", detail.CurrentSnapshot)
	}
	if step := detail.CurrentSnapshot.Steps[0]; step.Name != "v1" || step.Body != "v1 body" || step.Model != "sonnet" || step.AllowedTools != "Bash" || step.Brief != "" {
		t.Errorf("snapshot step = %+v, want the (unchanged) source prompt's content verbatim and no brief", step)
	}
	if len(detail.Versions) != 2 || detail.Versions[0].Version != 1 || detail.Versions[1].Version != 2 {
		t.Fatalf("versions = %+v, want v1 and v2", detail.Versions)
	}
}

// TestMarketplaceGet_Stats: stats are omitted until the aggregation job has
// written a row for the listing, then served, with success_rate absent while
// total_runs is 0.
func TestMarketplaceGet_Stats(t *testing.T) {
	r := newMarketplaceRig(t)
	id := r.publishPrompt(t, "stats-listing", "m", "", "", "stats-listing")

	getReq := func() *http.Request {
		req := r.req(http.MethodGet, "/api/marketplace/listings/"+id, r.member, nil)
		req.SetPathValue("id", id)
		return req
	}

	if before := decodeListingDetail(t, doMarketplaceJSON(r.mh.handleMarketplaceGet, getReq())); before.Stats != nil {
		t.Errorf("Stats before the job has run = %+v, want nil", before.Stats)
	}

	r.seedStats(t, id, 2, 0)
	zeroRuns := decodeListingDetail(t, doMarketplaceJSON(r.mh.handleMarketplaceGet, getReq()))
	if zeroRuns.Stats == nil || zeroRuns.Stats.TeamsUsing != 2 || zeroRuns.Stats.TotalRuns != 0 {
		t.Fatalf("Stats with zero runs = %+v, want teams_using=2 total_runs=0", zeroRuns.Stats)
	}
	if zeroRuns.Stats.SuccessRate != nil {
		t.Errorf("SuccessRate with zero runs = %v, want nil (no wrong fallbacks)", *zeroRuns.Stats.SuccessRate)
	}

	pgtest.MustExec(t, r.h.AdminDB, `UPDATE marketplace_listing_stats SET total_runs = 4, success_rate = 0.5 WHERE listing_id = $1`, id)
	withRuns := decodeListingDetail(t, doMarketplaceJSON(r.mh.handleMarketplaceGet, getReq()))
	if withRuns.Stats == nil || withRuns.Stats.TotalRuns != 4 || withRuns.Stats.SuccessRate == nil || *withRuns.Stats.SuccessRate != 0.5 {
		t.Fatalf("Stats with runs = %+v, want total_runs=4 success_rate=0.5", withRuns.Stats)
	}
}

// TestMarketplaceUnvote_Idempotent: a vote counts once however often it is
// cast, viewer_voted is the caller's own, and unvoting is a 204 with or without
// a vote to remove.
func TestMarketplaceUnvote_Idempotent(t *testing.T) {
	r := newMarketplaceRig(t)
	id := r.publishPrompt(t, "unvotable", "m", "", "", "unvotable")

	voteReq := func(method string) *http.Request {
		req := r.req(method, "/api/marketplace/listings/"+id+"/vote", r.member, nil)
		req.SetPathValue("id", id)
		return req
	}
	detailAs := func(callerID string) domain.ListingDetail {
		req := r.req(http.MethodGet, "/api/marketplace/listings/"+id, callerID, nil)
		req.SetPathValue("id", id)
		return decodeListingDetail(t, doMarketplaceJSON(r.mh.handleMarketplaceGet, req))
	}
	// No prior vote — still a no-op 204.
	if rec := doMarketplaceJSON(r.mh.handleMarketplaceUnvote, voteReq(http.MethodDelete)); rec.Code != http.StatusNoContent {
		t.Fatalf("unvote with no prior vote: status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	for i := range 2 {
		if rec := doMarketplaceJSON(r.mh.handleMarketplaceVote, voteReq(http.MethodPut)); rec.Code != http.StatusNoContent {
			t.Fatalf("vote %d: status = %d, want 204; body=%s", i+1, rec.Code, rec.Body.String())
		}
	}
	if d := detailAs(r.member); d.VoteCount != 1 || !d.ViewerVoted {
		t.Errorf("voter's view after a repeat vote = count %d, viewer_voted %v; want 1, true", d.VoteCount, d.ViewerVoted)
	}
	if d := detailAs(r.admin); d.VoteCount != 1 || d.ViewerVoted {
		t.Errorf("non-voter's view = count %d, viewer_voted %v; want the shared 1, false", d.VoteCount, d.ViewerVoted)
	}

	if rec := doMarketplaceJSON(r.mh.handleMarketplaceUnvote, voteReq(http.MethodDelete)); rec.Code != http.StatusNoContent {
		t.Fatalf("unvote: status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if d := detailAs(r.member); d.VoteCount != 0 || d.ViewerVoted {
		t.Errorf("after unvote = count %d, viewer_voted %v; want 0, false", d.VoteCount, d.ViewerVoted)
	}
}

// TestMarketplaceInstall_Prompt_HappyPath: installing a prompt-kind listing
// creates a team prompt (source='marketplace', content copied verbatim) in the
// target team and records the install; a repeat install is a second copy made
// from the frozen snapshot, so it succeeds after the source prompt is gone.
func TestMarketplaceInstall_Prompt_HappyPath(t *testing.T) {
	r := newMarketplaceRig(t)
	listingID := r.publishPrompt(t, "Great Prompt", "do the thing", "opus", "Bash,Read", "Great Prompt Listing")

	install := func() installResponse {
		t.Helper()
		rec := r.installAs(r.member, listingID, r.teamB)
		if rec.Code != http.StatusCreated {
			t.Fatalf("install: status = %d, want 201; body=%s", rec.Code, rec.Body.String())
		}
		var resp installResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp
	}
	installCount := func() int {
		t.Helper()
		var n int
		if err := r.h.AdminDB.QueryRow(`SELECT COUNT(*) FROM marketplace_installs WHERE listing_id = $1`, listingID).Scan(&n); err != nil {
			t.Fatalf("count installs: %v", err)
		}
		return n
	}

	resp := install()
	if resp.Kind != domain.ListingKindPrompt || resp.ID == "" || len(resp.PromptIDs) != 1 || resp.PromptIDs[0] != resp.ID {
		t.Fatalf("install response = %+v, want kind=prompt with a single prompt id equal to the root id", resp)
	}

	var name, body, model, tools, source, teamID string
	if err := r.h.AdminDB.QueryRow(`SELECT name, body, model, allowed_tools, source, team_id FROM prompts WHERE id = $1`, resp.ID).
		Scan(&name, &body, &model, &tools, &source, &teamID); err != nil {
		t.Fatalf("read installed prompt: %v", err)
	}
	if name != "Great Prompt" || body != "do the thing" || model != "opus" || tools != "Bash,Read" || source != "marketplace" || teamID != r.teamB {
		t.Errorf("installed prompt = (%q,%q,%q,%q,%q,%q), want the source content copied verbatim into teamB with source=marketplace",
			name, body, model, tools, source, teamID)
	}

	var rootObjectID string
	if err := r.h.AdminDB.QueryRow(`SELECT COALESCE(MAX(root_object_id::text), '') FROM marketplace_installs WHERE listing_id = $1`, listingID).
		Scan(&rootObjectID); err != nil {
		t.Fatalf("read install root: %v", err)
	}
	if n := installCount(); n != 1 || rootObjectID != resp.ID {
		t.Errorf("install record = count=%d root=%q, want count=1 root=%q", n, rootObjectID, resp.ID)
	}

	if res, err := r.h.AdminDB.Exec(`DELETE FROM prompts WHERE id = (SELECT source_id::text FROM marketplace_listings WHERE id = $1)`, listingID); err != nil {
		t.Fatalf("delete source prompt: %v", err)
	} else if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("deleted %d source prompts, want 1", n)
	}
	again := install()
	if again.ID == resp.ID {
		t.Fatalf("repeat install produced the same root object id %q twice, want two distinct copies", again.ID)
	}
	if err := r.h.AdminDB.QueryRow(`SELECT body FROM prompts WHERE id = $1`, again.ID).Scan(&body); err != nil || body != "do the thing" {
		t.Errorf("second copy body = %q (err %v), want the frozen snapshot content", body, err)
	}
	if n := installCount(); n != 2 {
		t.Errorf("install count = %d after two installs by the same team, want 2", n)
	}
}

// TestMarketplaceInstall_Blueprint_HappyPath: installing a blueprint-kind
// listing creates one fresh prompt per step (never the source ids — every
// step gets its own row) plus the blueprint, step order + briefs preserved,
// everything source='marketplace' and owned by the target team.
func TestMarketplaceInstall_Blueprint_HappyPath(t *testing.T) {
	r := newMarketplaceRig(t)
	p1 := r.seedPrompt(t, r.teamID, "Step One", "first mission", "sonnet", "")
	p2 := r.seedPrompt(t, r.teamID, "Step Two", "second mission", "opus", "Grep")
	bp := r.seedBlueprint(t, r.teamID, "Two-Step Chain")
	r.seedBlueprintStep(t, r.teamID, bp, 0, p1, "do step one first")
	r.seedBlueprintStep(t, r.teamID, bp, 1, p2, "")
	listingID := r.publishListing(t, r.admin, map[string]any{"kind": "blueprint", "source_id": bp, "name": "Chain Listing"})

	rec := r.installAs(r.member, listingID, r.teamB)
	if rec.Code != http.StatusCreated {
		t.Fatalf("install: status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp installResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Kind != domain.ListingKindBlueprint || resp.ID == "" || len(resp.PromptIDs) != 2 {
		t.Fatalf("install response = %+v, want kind=blueprint with 2 fresh prompt ids", resp)
	}
	if resp.PromptIDs[0] == p1 || resp.PromptIDs[1] == p2 {
		t.Errorf("install response reused the source prompt ids %v — every step must get a fresh prompt row", resp.PromptIDs)
	}

	var bpName, bpSource, bpTeam string
	if err := r.h.AdminDB.QueryRow(`SELECT name, source, team_id FROM blueprints WHERE id = $1`, resp.ID).Scan(&bpName, &bpSource, &bpTeam); err != nil {
		t.Fatalf("read installed blueprint: %v", err)
	}
	if bpName != "Chain Listing" || bpSource != "marketplace" || bpTeam != r.teamB {
		t.Errorf("installed blueprint = (%q,%q,%q), want (Chain Listing, marketplace, teamB)", bpName, bpSource, bpTeam)
	}

	rows, err := r.h.AdminDB.Query(`
		SELECT bs.step_index, bs.brief, p.name, p.body, p.model, p.allowed_tools
		FROM blueprint_steps bs JOIN prompts p ON p.id = bs.step_prompt_id
		WHERE bs.blueprint_id = $1 ORDER BY bs.step_index
	`, resp.ID)
	if err != nil {
		t.Fatalf("query steps: %v", err)
	}
	defer rows.Close()
	type stepRow struct {
		idx                             int
		brief, name, body, model, tools string
	}
	var steps []stepRow
	for rows.Next() {
		var s stepRow
		if err := rows.Scan(&s.idx, &s.brief, &s.name, &s.body, &s.model, &s.tools); err != nil {
			t.Fatalf("scan: %v", err)
		}
		steps = append(steps, s)
	}
	if len(steps) != 2 {
		t.Fatalf("step count = %d, want 2", len(steps))
	}
	if steps[0].name != "Step One" || steps[0].body != "first mission" || steps[0].model != "sonnet" || steps[0].brief != "do step one first" {
		t.Errorf("step 0 = %+v, want the source step's content verbatim", steps[0])
	}
	if steps[1].name != "Step Two" || steps[1].body != "second mission" || steps[1].model != "opus" || steps[1].tools != "Grep" || steps[1].brief != "" {
		t.Errorf("step 1 = %+v, want the source step's content verbatim", steps[1])
	}
}

// TestMarketplaceInstall_NonWriterOnTargetTeamIs403: write role is checked
// on the *installing* team, not the publisher's — a viewer on the target
// team is blocked even though they can freely browse/read the listing.
func TestMarketplaceInstall_NonWriterOnTargetTeamIs403(t *testing.T) {
	r := newMarketplaceRig(t)
	listingID := r.publishPrompt(t, "guarded", "mission", "", "", "n")

	viewerB := pgtest.SeedUser(t, r.h, "viewerB")
	pgtest.AddOrgMember(t, r.h, viewerB, r.orgID, r.teamB, "member", "viewer")

	rec := r.installAs(viewerB, listingID, r.teamB)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer install: status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}

	var count int
	if err := r.h.AdminDB.QueryRow(`SELECT COUNT(*) FROM marketplace_installs WHERE listing_id = $1`, listingID).Scan(&count); err != nil {
		t.Fatalf("count installs: %v", err)
	}
	if count != 0 {
		t.Errorf("install count = %d after blocked viewer install, want 0", count)
	}
}

// TestMarketplaceInstall_DelistedByPublisherIsGone: the publisher can still
// see (Get) their own delisted listing (RLS: published OR own-team-write),
// but installing it is refused with 410 — delisting must actually block new
// forks, not just hide from browse.
func TestMarketplaceInstall_DelistedByPublisherIsGone(t *testing.T) {
	r := newMarketplaceRig(t)
	listingID := r.publishPrompt(t, "delist-me", "mission", "", "", "n")
	r.delist(t, listingID)

	rec := r.installAs(r.admin, listingID, "")
	if rec.Code != http.StatusGone {
		t.Fatalf("delisted install (publisher, visible): status = %d, want 410; body=%s", rec.Code, rec.Body.String())
	}
}

// TestMarketplaceInstall_DelistedInvisibleToNonPublisherIs404: a
// non-publisher can't see a delisted listing at all (RLS), so their install
// attempt 404s the same as any other unknown id — it never learns the
// listing existed.
func TestMarketplaceInstall_DelistedInvisibleToNonPublisherIs404(t *testing.T) {
	r := newMarketplaceRig(t)
	listingID := r.publishPrompt(t, "delist-me-2", "mission", "", "", "n")
	r.delist(t, listingID)

	rec := r.installAs(r.member, listingID, r.teamB)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delisted install (non-publisher, invisible): status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}
