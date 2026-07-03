// The within-org prompt marketplace's publish/republish/delist/install
// surface (TFAC-536 publish, TFAC-538 install). Multi-mode only: every
// handler opens with gateMarketplace, which 404s in local mode (the
// marketplace is a multi-mode concept, db.MarketplaceStore's SQLite impl is
// a stub) — never 403, mirroring the invites/org-members precedent that a
// mode mismatch isn't a role failure, it's "this surface doesn't exist for
// you". The within-org marketplace itself has no admin toggle — it's always
// on for every multi-mode org. org_settings.marketplace_enabled exists
// (TFAC-535) but is NOT read here; it's reserved for gating the future
// cross-org marketplace (TFAC-92 phase 2 / TFAC-539), a different, still-
// unbuilt surface.
//
// Every listing snapshot is minted server-side from the caller's own team
// object (buildListingSnapshot) — the client posts a kind + source_id and
// gets back whatever the team object currently contains. This is the
// TFAC-535 copy-on-publish invariant enforced at the write boundary: a
// client can't inject arbitrary snapshot content, and a listing never
// live-references the object it was published from. Install
// (handleMarketplaceInstall) is the mirror invariant on the consume side:
// it materializes detail.CurrentSnapshot — the already-frozen version
// document — into the installing team, never buildListingSnapshot / the
// publisher team's live rows, so a copy survives the publisher team or its
// source object being deleted.

package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/teamscope"
)

// marketplaceHandler serves the /api/marketplace/listings publish/republish/
// delist/relist/by-source family. It holds only the transactional store
// runner + authz checker, mirroring blueprintsHandler/eventHandlersHandler.
type marketplaceHandler struct {
	tx db.TxRunner
	az *authz.Checker
}

// errMarketplaceSourceNotFound is buildListingSnapshot's sentinel for "the
// team object this listing would be minted from doesn't exist (or isn't
// visible to the caller)" — the handler translates it to a 404 rather than
// exposing the raw store nil-check.
var errMarketplaceSourceNotFound = errors.New("marketplace: source object not found")

// gateMarketplace is the shared front gate every handler in this file opens
// with: local mode 404s (the marketplace is multi-mode only) — 404, not 403,
// mirroring the invites/org-members precedent that a mode mismatch isn't a
// role failure, it's "this surface doesn't exist for you". Returns (orgID,
// userID, true) on success. No org-settings toggle check — the within-org
// marketplace has none; see the file doc comment.
func (mh *marketplaceHandler) gateMarketplace(w http.ResponseWriter, r *http.Request) (orgID, userID string, ok bool) {
	if runmode.Current() == runmode.ModeLocal {
		http.NotFound(w, r)
		return "", "", false
	}
	orgID, ok = requireOrg(w, r)
	if !ok {
		return "", "", false
	}
	userID = ClaimsFrom(r.Context()).Subject
	return orgID, userID, true
}

// duplicateSourceMessage renders the 409 body for a source that already has
// a listing — phrased differently for a delisted listing (relist, don't
// publish a duplicate) vs. a published one (republish to push an update).
func duplicateSourceMessage(kind, status string) string {
	if status == domain.ListingStatusDelisted {
		return "this " + kind + " was previously published and is currently delisted — use the relist endpoint instead of publishing a duplicate"
	}
	return "this " + kind + " is already published — use the republish endpoint to push an update"
}

// validateMarketplaceEventTypes rejects any event type not present in the
// events_catalog registry — the same check event_handlers_handler's create
// path runs, reused here so a listing's facets can never reference an id the
// catalog doesn't know.
func validateMarketplaceEventTypes(eventTypes []string) error {
	for _, et := range eventTypes {
		if _, ok := events.Get(et); !ok {
			return fmt.Errorf("unknown event_type: %s", et)
		}
	}
	return nil
}

// buildListingSnapshot loads the source object (a team's blueprint or
// prompt) and mints a domain.ListingSnapshot server-side — the client never
// supplies snapshot content (TFAC-536). For kind=blueprint it loads
// blueprint_steps + each step's prompt and mints one SnapshotStep per step,
// mirroring the BlueprintPlanStep mint in delegate/delegate.go (minus ids —
// the snapshot must contain nothing org-internal, per TFAC-535). For
// kind=prompt it mints a single step with no brief. Returns the resolved
// snapshot and the source object's owning team id (for the write gate);
// snap.Name/Description/EventTypes are left zero — the caller stamps those
// from the request. errMarketplaceSourceNotFound when the source doesn't
// resolve (missing, or a blueprint with zero steps — auto-wrap guarantees
// every live blueprint has ≥1, so an empty one is a data-integrity gap worth
// surfacing as "not found" rather than publishing an empty listing).
func buildListingSnapshot(ctx context.Context, tx db.TxStores, orgID, kind, sourceID string) (domain.ListingSnapshot, string, error) {
	switch kind {
	case domain.ListingKindPrompt:
		p, err := tx.Prompts.Get(ctx, orgID, sourceID)
		if err != nil {
			return domain.ListingSnapshot{}, "", err
		}
		if p == nil {
			return domain.ListingSnapshot{}, "", errMarketplaceSourceNotFound
		}
		return domain.ListingSnapshot{
			SchemaVersion: 1,
			Kind:          domain.ListingKindPrompt,
			Steps: []domain.SnapshotStep{{
				StepIndex:    0,
				Name:         p.Name,
				Body:         p.Body,
				AllowedTools: p.AllowedTools,
				Model:        p.Model,
			}},
		}, p.TeamID, nil

	case domain.ListingKindBlueprint:
		bp, err := tx.Blueprints.Get(ctx, orgID, sourceID)
		if err != nil {
			return domain.ListingSnapshot{}, "", err
		}
		if bp == nil {
			return domain.ListingSnapshot{}, "", errMarketplaceSourceNotFound
		}
		steps, err := tx.Blueprints.ListSteps(ctx, orgID, sourceID)
		if err != nil {
			return domain.ListingSnapshot{}, "", err
		}
		if len(steps) == 0 {
			return domain.ListingSnapshot{}, "", errMarketplaceSourceNotFound
		}
		snapSteps := make([]domain.SnapshotStep, len(steps))
		for i, st := range steps {
			p, err := tx.Prompts.Get(ctx, orgID, st.StepPromptID)
			if err != nil {
				return domain.ListingSnapshot{}, "", err
			}
			if p == nil {
				return domain.ListingSnapshot{}, "", fmt.Errorf("blueprint %s step %d: prompt %s not found", sourceID, st.StepIndex, st.StepPromptID)
			}
			snapSteps[i] = domain.SnapshotStep{
				StepIndex:    st.StepIndex,
				Name:         p.Name,
				Body:         p.Body,
				AllowedTools: p.AllowedTools,
				Model:        p.Model,
				Brief:        st.Brief,
			}
		}
		return domain.ListingSnapshot{
			SchemaVersion: 1,
			Kind:          domain.ListingKindBlueprint,
			Steps:         snapSteps,
		}, bp.TeamID, nil

	default:
		return domain.ListingSnapshot{}, "", fmt.Errorf("unknown listing kind %q", kind)
	}
}

// publishListingRequest is the POST /api/marketplace/listings body.
type publishListingRequest struct {
	Kind        string   `json:"kind"` // domain.ListingKindPrompt | domain.ListingKindBlueprint
	SourceID    string   `json:"source_id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	EventTypes  []string `json:"event_types"`
}

// handleMarketplacePublish publishes a fresh snapshot of a team blueprint or
// standalone prompt as a new org-visible listing.
//
// POST /api/marketplace/listings
func (mh *marketplaceHandler) handleMarketplacePublish(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := mh.gateMarketplace(w, r)
	if !ok {
		return
	}

	var req publishListingRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	if req.Kind != domain.ListingKindPrompt && req.Kind != domain.ListingKindBlueprint {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "kind must be 'prompt' or 'blueprint'"})
		return
	}
	if req.SourceID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "source_id is required"})
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	if err := validateMarketplaceEventTypes(req.EventTypes); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// Resolve the source object + its owning team, and check for an existing
	// listing for this source — published OR delisted (GetBySource, not
	// GetActiveBySource: a delisted listing must route the caller to relist,
	// not let them mint a second listing for the same source_id — the
	// published-only partial unique index wouldn't catch that at the DB
	// layer) — all under one read tx.
	var (
		snap            domain.ListingSnapshot
		teamID          string
		sourceNotFound  bool
		existingListing *domain.ListingSummary
	)
	if err := mh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		snap, teamID, e = buildListingSnapshot(r.Context(), tx, orgID, req.Kind, req.SourceID)
		if errors.Is(e, errMarketplaceSourceNotFound) {
			sourceNotFound = true
			return nil
		}
		if e != nil {
			return e
		}
		existingListing, e = tx.Marketplace.GetBySource(r.Context(), orgID, req.SourceID)
		return e
	}); err != nil {
		internalError(w, "marketplace", err)
		return
	}
	if sourceNotFound {
		notFound(w, req.Kind)
		return
	}
	if existingListing != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": duplicateSourceMessage(req.Kind, existingListing.Status)})
		return
	}

	// Reject a viewer before minting the listing (TFAC-447 shape): the
	// publisher-team gate mirrors gateBlueprintWrite/gateHandlerWrite —
	// resolve the object's team, then RequireTeamWrite.
	if !mh.az.RequireTeamWrite(w, r, orgID, userID, teamID) {
		return
	}

	snap.Name = req.Name
	snap.Description = req.Description
	snap.EventTypes = req.EventTypes

	var listingID string
	if err := mh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		listingID, e = tx.Marketplace.Publish(r.Context(), orgID, domain.MarketplaceListing{
			Kind:            req.Kind,
			Name:            req.Name,
			Description:     req.Description,
			PublisherTeamID: teamID,
			SourceID:        req.SourceID,
		}, snap)
		return e
	}); err != nil {
		if isUniqueViolation(err) {
			// A concurrent publish of the same source won the race between our
			// pre-check and this insert — translate the partial-unique-index hit
			// to the same friendly 409 rather than a raw constraint error.
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "this " + req.Kind + " is already published — use the republish endpoint to push an update",
			})
			return
		}
		internalError(w, "marketplace", err)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{"id": listingID})
}

// publishVersionRequest is the POST /api/marketplace/listings/{id}/versions
// body — republish. The snapshot content itself is always rebuilt
// server-side from the listing's source object; name/description/event_types
// are threaded from the request so a republish can retune the listing's
// live/searchable fields independent of what the frozen snapshot carries.
type publishVersionRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	EventTypes  []string `json:"event_types"`
}

// handleMarketplaceListingVersionCreate republishes: rebuilds the snapshot
// from the listing's source object as it stands right now, appends it as a
// new immutable version, and bumps current_version. The prior version's
// snapshot is untouched — republish never rewrites history.
//
// POST /api/marketplace/listings/{id}/versions
func (mh *marketplaceHandler) handleMarketplaceListingVersionCreate(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := mh.gateMarketplace(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")

	var req publishVersionRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	if err := validateMarketplaceEventTypes(req.EventTypes); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	var (
		listing           *domain.MarketplaceListing
		snap              domain.ListingSnapshot
		listingNotFound   bool
		sourceUnavailable bool
	)
	if err := mh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		detail, e := tx.Marketplace.Get(r.Context(), orgID, id, userID)
		if errors.Is(e, sql.ErrNoRows) {
			listingNotFound = true
			return nil
		}
		if e != nil {
			return e
		}
		listing = &detail.MarketplaceListing
		if listing.SourceID == "" {
			sourceUnavailable = true
			return nil
		}
		var se error
		snap, _, se = buildListingSnapshot(r.Context(), tx, orgID, listing.Kind, listing.SourceID)
		if errors.Is(se, errMarketplaceSourceNotFound) {
			sourceUnavailable = true
			return nil
		}
		return se
	}); err != nil {
		internalError(w, "marketplace", err)
		return
	}
	if listingNotFound {
		notFound(w, "listing")
		return
	}
	if sourceUnavailable {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "the source object this listing was published from no longer exists — it can be delisted but not republished",
		})
		return
	}

	if !mh.az.RequireTeamWrite(w, r, orgID, userID, listing.PublisherTeamID) {
		return
	}

	snap.Name = req.Name
	snap.Description = req.Description
	snap.EventTypes = req.EventTypes

	var newVersion int
	if err := mh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		newVersion, e = tx.Marketplace.PublishVersion(r.Context(), orgID, id, snap, req.Name, req.Description, req.EventTypes)
		return e
	}); err != nil {
		internalError(w, "marketplace", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]int{"version": newVersion})
}

// gateMarketplaceListingWrite resolves a listing's publisher team and applies
// the write gate, mirroring gateBlueprintWrite. Returns (listing, true) on
// success; on failure it has already written the response (404 for a missing
// listing renders via the caller's own check when listing is nil, 403 for a
// non-writer) and returns (nil, false).
func (mh *marketplaceHandler) gateMarketplaceListingWrite(w http.ResponseWriter, r *http.Request, orgID, userID, id string) (*domain.MarketplaceListing, bool) {
	var (
		listing *domain.MarketplaceListing
		missing bool
	)
	if err := mh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		detail, e := tx.Marketplace.Get(r.Context(), orgID, id, userID)
		if errors.Is(e, sql.ErrNoRows) {
			missing = true
			return nil
		}
		if e != nil {
			return e
		}
		listing = &detail.MarketplaceListing
		return nil
	}); err != nil {
		internalError(w, "marketplace", err)
		return nil, false
	}
	if missing {
		notFound(w, "listing")
		return nil, false
	}
	if !mh.az.RequireTeamWrite(w, r, orgID, userID, listing.PublisherTeamID) {
		return nil, false
	}
	return listing, true
}

// handleMarketplaceListingDelist hides a listing from every other member's
// browse. Publisher-team writers only. Copy-not-dependency invariant: this
// never touches the copies other teams already installed from this listing,
// or the votes/install history on the listing row itself — delisting is
// purely a visibility flip (TFAC-535's marketplace_listings_select RLS:
// published OR own-team-write).
//
// POST /api/marketplace/listings/{id}/delist
func (mh *marketplaceHandler) handleMarketplaceListingDelist(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := mh.gateMarketplace(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if _, ok := mh.gateMarketplaceListingWrite(w, r, orgID, userID, id); !ok {
		return
	}
	if err := mh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.Marketplace.Delist(r.Context(), orgID, id)
	}); err != nil {
		internalError(w, "marketplace", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleMarketplaceListingRelist reverses a delist, restoring the listing to
// other members' browse. Publisher-team writers only. Copies + votes already
// on the listing are untouched — same copy-not-dependency invariant as
// delist.
//
// POST /api/marketplace/listings/{id}/relist
func (mh *marketplaceHandler) handleMarketplaceListingRelist(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := mh.gateMarketplace(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if _, ok := mh.gateMarketplaceListingWrite(w, r, orgID, userID, id); !ok {
		return
	}
	if err := mh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.Marketplace.Relist(r.Context(), orgID, id)
	}); err != nil {
		internalError(w, "marketplace", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleMarketplaceListingBySource resolves the org's listing for a
// team-side source object — published OR delisted — or null when none. The
// badge/publish-affordance lookup the editor drives from a blueprint or
// prompt's id: GetBySource (not GetActiveBySource) so a delisted object
// still reports its listing and the editor can offer Relist instead of
// reverting to "never published" (which would let a client mint a
// duplicate listing for the same source on the next publish). Returns the
// full ListingSummary, not just the header — the editor's republish dialog
// seeds its event-type multi-select from this response's EventTypes; a
// bare header would silently drop it and a submit would delete-and-reinsert
// marketplace_listing_events from whatever the client had in memory,
// wiping any facet outside the small trigger-derived suggestion set. RLS
// scopes a delisted row to the publisher team — a non-publisher caller sees
// null, same as before. Any org member may read (no team-write gate — this
// is display-only).
//
// GET /api/marketplace/listings/by-source/{source_id}
func (mh *marketplaceHandler) handleMarketplaceListingBySource(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := mh.gateMarketplace(w, r)
	if !ok {
		return
	}
	sourceID := r.PathValue("source_id")

	var listing *domain.ListingSummary
	if err := mh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		listing, e = tx.Marketplace.GetBySource(r.Context(), orgID, sourceID)
		return e
	}); err != nil {
		internalError(w, "marketplace", err)
		return
	}
	writeJSON(w, http.StatusOK, listing)
}

// handleMarketplaceList serves the browse page: search + event-type/kind
// facets, sorted by installs (default) | votes | recent. Any org member may
// read — RLS on marketplace_listings (published OR own-team-write) scopes
// what List sees without any additional filtering here; a non-publisher
// simply never sees another team's delisted listing.
//
// GET /api/marketplace/listings?query=&event_type=&kind=&sort=
func (mh *marketplaceHandler) handleMarketplaceList(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := mh.gateMarketplace(w, r)
	if !ok {
		return
	}

	f := domain.ListingFilter{
		Query:     strings.TrimSpace(r.URL.Query().Get("query")),
		EventType: strings.TrimSpace(r.URL.Query().Get("event_type")),
		Kind:      strings.TrimSpace(r.URL.Query().Get("kind")),
		Sort:      strings.TrimSpace(r.URL.Query().Get("sort")),
	}
	if f.Kind != "" && f.Kind != domain.ListingKindPrompt && f.Kind != domain.ListingKindBlueprint {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "kind must be 'prompt' or 'blueprint'"})
		return
	}
	if f.Sort != "" && f.Sort != domain.ListingSortInstalls && f.Sort != domain.ListingSortVotes && f.Sort != domain.ListingSortRecent {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sort must be 'installs', 'votes', or 'recent'"})
		return
	}
	if f.Sort == "" {
		// The HTTP contract (this handler's doc comment, the API description
		// in the ticket) says installs is the default — pin it here rather
		// than leaving f.Sort empty and letting MarketplaceStore.List's own
		// internal default (currently "recent", documented on
		// domain.ListingFilter for callers that don't care) decide. Two
		// different defaults for two different reasons: the store's is a
		// safe fallback for any caller; this one is the browse page's
		// product default.
		f.Sort = domain.ListingSortInstalls
	}

	var listings []domain.ListingSummary
	if err := mh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		listings, e = tx.Marketplace.List(r.Context(), orgID, userID, f)
		return e
	}); err != nil {
		internalError(w, "marketplace", err)
		return
	}
	if listings == nil {
		listings = []domain.ListingSummary{}
	}
	writeJSON(w, http.StatusOK, listings)
}

// handleMarketplaceGet serves the listing detail view: header + counts, the
// current snapshot (full step/prompt bodies for preview), and version
// history. Any org member may read, same RLS visibility as List — a listing
// delisted by another team resolves to 404, not just absent from browse.
//
// GET /api/marketplace/listings/{id}
func (mh *marketplaceHandler) handleMarketplaceGet(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := mh.gateMarketplace(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	// Validate up front: id is a uuid column on Postgres, so a malformed
	// value surfaces as a SQLSTATE 22P02 cast error from the store call
	// (→ 500) rather than a clean miss. Treating malformed ids as "not
	// found" mirrors the house pattern (see tasks.go's SKY-330 comment).
	if _, err := uuid.Parse(id); err != nil {
		notFound(w, "listing")
		return
	}

	var (
		detail  domain.ListingDetail
		missing bool
	)
	if err := mh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		detail, e = tx.Marketplace.Get(r.Context(), orgID, id, userID)
		if errors.Is(e, sql.ErrNoRows) {
			missing = true
			return nil
		}
		return e
	}); err != nil {
		internalError(w, "marketplace", err)
		return
	}
	if missing {
		notFound(w, "listing")
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// handleMarketplaceVote records the caller's "recommend" vote on a listing.
// Any org member — no team-write gate, this is a read-adjacent social action,
// not a publisher-team operation. Idempotent: a repeat vote is a no-op, still
// 204.
//
// PUT /api/marketplace/listings/{id}/vote
func (mh *marketplaceHandler) handleMarketplaceVote(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := mh.gateMarketplace(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		notFound(w, "listing")
		return
	}

	// Vote's INSERT carries an FK to marketplace_listings(id) — a
	// well-formed but nonexistent id would otherwise trip that constraint
	// and surface as a 500. Pre-check existence (same RLS visibility Get
	// already applies) and 404 before writing, rather than translating a
	// caught FK-violation error after the fact.
	var missing bool
	if err := mh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if _, e := tx.Marketplace.Get(r.Context(), orgID, id, userID); errors.Is(e, sql.ErrNoRows) {
			missing = true
			return nil
		} else if e != nil {
			return e
		}
		return nil
	}); err != nil {
		internalError(w, "marketplace", err)
		return
	}
	if missing {
		notFound(w, "listing")
		return
	}

	if err := mh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.Marketplace.Vote(r.Context(), orgID, id, userID)
	}); err != nil {
		internalError(w, "marketplace", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleMarketplaceUnvote removes the caller's vote on a listing, if any.
// Idempotent: removing a vote that doesn't exist is a no-op, still 204 —
// including when the listing itself doesn't exist (Unvote's DELETE carries
// no FK, so a nonexistent well-formed id is just a zero-row no-op; no
// existence pre-check needed here unlike handleMarketplaceVote).
//
// DELETE /api/marketplace/listings/{id}/vote
func (mh *marketplaceHandler) handleMarketplaceUnvote(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := mh.gateMarketplace(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		notFound(w, "listing")
		return
	}
	if err := mh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.Marketplace.Unvote(r.Context(), orgID, id, userID)
	}); err != nil {
		internalError(w, "marketplace", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// installListingRequest is the POST /api/marketplace/listings/{id}/install
// body. TeamID is the acting team the write picker supplied — optional; a
// solo/single-team caller omits it and teamscope resolves their sole team
// (see gateActingTeamWrite / teamscope.ResolveActing).
type installListingRequest struct {
	TeamID string `json:"team_id"`
}

// installListingResponse returns the id(s) of everything the install
// created. ID is the root object — a blueprint id for kind=blueprint, the
// prompt id for kind=prompt — the same value recorded as the install's
// root_object_id. PromptIDs is every prompt row created (the step copies
// for kind=blueprint, the single prompt for kind=prompt).
type installListingResponse struct {
	Kind      string   `json:"kind"`
	ID        string   `json:"id"`
	PromptIDs []string `json:"prompt_ids"`
}

// handleMarketplaceInstall is "copy to my team" (TFAC-538): it materializes
// the listing's current-version snapshot into the caller's chosen team as
// brand-new team-owned object(s) — a fork, not a live reference; it never
// updates when the listing republishes and surviving the listing being
// delisted/deleted later. Write role on the *installing* team (not the
// publisher's) is required: gateActingTeamWrite gives a clean 403 for a
// viewer before anything is read; RLS enforces the same gate on the
// marketplace_installs INSERT (marketplace_installs_insert) as defense in
// depth. Repeat installs by the same team are allowed — each is a fresh
// fork, recorded as its own marketplace_installs row (TFAC-538: "forks are
// cheap").
//
// POST /api/marketplace/listings/{id}/install
func (mh *marketplaceHandler) handleMarketplaceInstall(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := mh.gateMarketplace(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		notFound(w, "listing")
		return
	}

	var req installListingRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}

	// Reject a viewer on the target team before touching the listing
	// (TFAC-447 shape) — this gates the *installing* team, independent of
	// the listing/publisher, so it can run before any listing lookup.
	if !gateActingTeamWrite(w, r, mh.tx, mh.az, orgID, userID, req.TeamID, "marketplace") {
		return
	}

	var (
		listingNotFound bool
		delisted        bool
		resp            installListingResponse
	)
	if err := mh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		teamID, e := teamscope.ResolveActing(r.Context(), tx.Teams, tx.Users, orgID, userID, req.TeamID)
		if e != nil {
			return e
		}

		// Load the listing + its current-version snapshot. Get's RLS
		// visibility (published OR own-team-write) means a delisted listing
		// invisible to this caller already resolves to sql.ErrNoRows here —
		// the explicit Status check below only fires for the publisher's
		// own team, who can still see (but must not install) their own
		// delisted listing.
		detail, e := tx.Marketplace.Get(r.Context(), orgID, id, userID)
		if errors.Is(e, sql.ErrNoRows) {
			listingNotFound = true
			return nil
		}
		if e != nil {
			return e
		}
		if detail.Status == domain.ListingStatusDelisted {
			delisted = true
			return nil
		}

		// buildListingSnapshot is deliberately NOT called here: the
		// installer consumes detail.CurrentSnapshot (the already-frozen
		// version document), never the publisher team's live prompt/
		// blueprint rows — the TFAC-535 design constraint that makes this
		// path survive the source object (or its whole team) being deleted.
		rootObjectID, promptIDs, e := tx.Marketplace.MaterializeListing(
			r.Context(), orgID, teamID, detail.CurrentSnapshot, id, detail.CurrentVersion, userID,
		)
		if e != nil {
			return e
		}
		resp = installListingResponse{Kind: detail.Kind, ID: rootObjectID, PromptIDs: promptIDs}
		return nil
	}); err != nil {
		if teamscope.WriteIfSelectionError(w, err) {
			return
		}
		internalError(w, "marketplace", err)
		return
	}
	if listingNotFound {
		notFound(w, "listing")
		return
	}
	if delisted {
		writeJSON(w, http.StatusGone, map[string]string{"error": "this listing has been delisted and can no longer be installed"})
		return
	}

	writeJSON(w, http.StatusCreated, resp)
}
