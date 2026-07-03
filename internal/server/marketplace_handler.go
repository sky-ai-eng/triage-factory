// The within-org prompt marketplace's publish/republish/delist surface
// (TFAC-536). Multi-mode only: every handler opens with gateMarketplace,
// which 404s on two independent axes — local mode (the marketplace is a
// multi-mode concept, db.MarketplaceStore's SQLite impl is a stub) and the
// org's ship-dark marketplace_enabled toggle (off by default until TFAC-539
// flips it visible). Both conditions render 404, never 403 — the mode/toggle
// axis isn't a role failure, it's "this surface doesn't exist for you".
//
// Every listing snapshot is minted server-side from the caller's own team
// object (buildListingSnapshot) — the client posts a kind + source_id and
// gets back whatever the team object currently contains. This is the
// TFAC-535 copy-on-publish invariant enforced at the write boundary: a
// client can't inject arbitrary snapshot content, and a listing never
// live-references the object it was published from.

package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
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
// with: local mode 404s (the marketplace is multi-mode only), then the org's
// marketplace_enabled toggle 404s when off (ship-dark until TFAC-539's launch
// flip). Both conditions are deliberately 404, not 403 — mirrors the
// invites/org-members precedent for the mode axis, and extends the same
// posture to the feature toggle so a org that hasn't opted in doesn't even
// learn the surface exists. Returns (orgID, userID, true) on success.
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

	var enabled bool
	if err := mh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		set, e := tx.Orgs.GetSettings(r.Context(), orgID)
		if e != nil {
			return e
		}
		enabled = set.MarketplaceEnabled
		return nil
	}); err != nil {
		internalError(w, "marketplace", err)
		return "", "", false
	}
	if !enabled {
		http.NotFound(w, r)
		return "", "", false
	}
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
