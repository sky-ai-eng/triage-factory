package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=MarketplaceStore --output=./mocks --case=underscore --with-expecter

// MarketplaceStore owns the within-org prompt marketplace tables
// (marketplace_listings, marketplace_listing_versions,
// marketplace_listing_events, marketplace_votes, marketplace_installs) —
// TFAC-535. Copy-on-publish throughout: every write here is either a fresh
// listing/version snapshot or a plain audit row (vote, install); nothing a
// consumer copies ever FKs back to a listing, and a listing never FKs into
// the team-owned prompts/blueprints it was published from. Provenance for a
// copy lives on the install record (RecordInstall's rootObjectID), not on
// the copy itself — prompts/blueprints stay untouched in both dialects.
//
// Multi-mode only (house pattern — see InvitesStore): every marketplace
// table lives in the Postgres baseline only. The SQLite impl
// (internal/db/sqlite/marketplace_store.go) is a stub whose every method
// returns db.ErrNotApplicableInLocal, mirroring internal/db/sqlite/invites.go
// — it exists purely to keep the db.Stores bundle fully wired in both
// modes; local-mode handlers 404 before any call reaches the store.
//
// Admin-pool split (TFAC-540): every request-facing method (an org member
// browsing, publishing to their own team, or voting/installing as
// themselves) runs on the app pool and lets RLS gate the write.
// RecomputeStatsSystem is the one exception — a cross-team, cross-run
// aggregate that can't be computed under any single user's RLS view, so it
// routes through the admin pool like the other `...System`-suffixed methods
// elsewhere in this package (cf. PromptStore.IncrementUsageSystem).
type MarketplaceStore interface {
	// Publish creates a new listing plus its v1 version row. l.ID is
	// ignored — the store mints the id and returns it. l.CurrentVersion is
	// ignored — always starts at 1. The listing's marketplace_listing_events
	// facet rows are seeded from snap.EventTypes. One transaction: listing +
	// version + event rows land atomically or not at all.
	Publish(ctx context.Context, orgID string, l domain.MarketplaceListing, snap domain.ListingSnapshot) (string, error)

	// PublishVersion appends a new immutable version to an existing listing
	// (republish), bumps the listing's current_version, and updates the
	// listing's live/searchable name + description + event-type facets.
	// name/desc/eventTypes are threaded separately from snap because they
	// update the *listing* row (current, mutable, what List/browse reads),
	// while snap is frozen into the new version row verbatim — a republish
	// can retune the facets or blurb without them needing to match what's
	// preserved in the new snapshot. Returns the newly minted version
	// number. One transaction: listing update + version insert + facet
	// replace land atomically or not at all.
	PublishVersion(ctx context.Context, orgID, listingID string, snap domain.ListingSnapshot, name, desc string, eventTypes []string) (int, error)

	// Delist flips status to 'delisted' and stamps delisted_at. The listing
	// row, its versions, votes, and install history all survive — delisting
	// only removes it from other members' browse (RLS: the publisher team
	// still sees it to relist/manage).
	Delist(ctx context.Context, orgID, listingID string) error

	// Relist flips a delisted listing back to 'published' and clears
	// delisted_at.
	Relist(ctx context.Context, orgID, listingID string) error

	// List returns browse-page summaries matching f, most-relevant-first per
	// f.Sort (ListingSortInstalls/Votes/Recent, defaulting to Recent).
	// viewerUserID populates ListingSummary.ViewerVoted; pass "" from a
	// caller with no user context (viewer_voted always false). RLS scopes
	// the underlying rows to org members + published-or-own-team listings —
	// this method applies no additional visibility filtering beyond f.
	List(ctx context.Context, orgID string, viewerUserID string, f domain.ListingFilter, opts ListOpts) ([]domain.ListingSummary, int, error)

	// Get returns one listing's full detail — header, computed counts, the
	// version at listing.current_version as CurrentSnapshot, and the full
	// version history — or (ListingDetail{}, sql.ErrNoRows) if not found /
	// not visible under RLS. viewerUserID populates ViewerVoted; "" is valid
	// (no viewer context).
	Get(ctx context.Context, orgID, listingID, viewerUserID string) (domain.ListingDetail, error)

	// GetActiveBySource resolves the org's currently-*published* listing for
	// a team-side source object (source_id), or (nil, nil) if none. Mirrors
	// the marketplace_listings_source_active partial unique index (which is
	// itself published-only — a delisted row doesn't count against it, so
	// this method must not be used as "does this source have a listing at
	// all" — see GetBySource for that).
	GetActiveBySource(ctx context.Context, orgID, sourceID string) (*domain.MarketplaceListing, error)

	// GetBySource resolves the org's listing for a team-side source object
	// regardless of status (published OR delisted), or (nil, nil) if none —
	// the publish flow's duplicate-source check (a delisted listing must
	// route the caller to relist, not let them mint a second listing for the
	// same source) and the editor's by-source badge lookup. Returns the full
	// ListingSummary (not just the header) — EventTypes in particular, since
	// the editor's republish dialog seeds its event-type multi-select from
	// this response; returning the bare header would silently drop that
	// seed and a submit would blow away every facet not in the small
	// trigger-derived suggestion set. RLS scopes a delisted row to the
	// publisher team the same way Get does — a non-publisher caller simply
	// sees nothing, same as GetActiveBySource.
	GetBySource(ctx context.Context, orgID, sourceID string) (*domain.ListingSummary, error)

	// Vote records userID's 'recommend' vote on listingID. Idempotent — a
	// repeat vote from the same user is a no-op (PK on (listing_id,
	// user_id)), not an error.
	Vote(ctx context.Context, orgID, listingID, userID string) error

	// Unvote removes userID's vote on listingID, if any. Removing a vote
	// that doesn't exist is a no-op, not an error.
	Unvote(ctx context.Context, orgID, listingID, userID string) error

	// RecordInstall appends an audit row for a "copy to my team" install —
	// the substrate for install counts, the TFAC-540 run-derived-stats
	// fast-follow, and provenance (rootObjectID is the id of the copy this
	// install materialized: a blueprint id for kind=blueprint, a prompt id
	// for kind=prompt). No FK on rootObjectID — install history must survive
	// the copy's later deletion, exactly like GetActiveBySource's source_id
	// survives the source's. version is the listing version that was
	// actually installed (may lag listing.current_version if the installer
	// pinned an older snapshot). userID may be "" (system-initiated install
	// has no human actor); teamID is the installing team and is required.
	RecordInstall(ctx context.Context, orgID, listingID string, version int, teamID, userID, rootObjectID string) error

	// MaterializeListing is the "copy to my team" install (TFAC-538): it
	// deep-copies snap into teamID as brand-new, team-owned prompt(s) —
	// kind=prompt mints one; kind=blueprint mints one fresh prompt per step
	// (never shared — every step gets its own row, satisfying the copy-only
	// invariant trivially) plus the blueprint header and its blueprint_steps
	// in snapshot order with briefs — then records the install, all in one
	// transaction. Every copy is source='marketplace' (an app-validated
	// open-set value alongside 'system'/'user'/'imported'; TFAC-535 decided
	// no new columns on prompts/blueprints, not no new source value).
	//
	// Consumes only snap — never listingID/version to look up the listing's
	// source, and never anything from the publisher team — so this method
	// makes no reads of the publisher team's prompts/blueprints. That's the
	// TFAC-535 cross-org insurance in action: the installer consumes a
	// snapshot document, never source rows, so an install still succeeds
	// after the publisher team or its source object is deleted. listingID +
	// version are threaded through only to stamp the install record
	// (RecordInstall's audit row) — its FK to marketplace_listings is the
	// only thing here that depends on the listing still existing.
	//
	// Returns the created root object id (a blueprint id for kind=blueprint,
	// the prompt id for kind=prompt — the same value written to the
	// install's root_object_id) plus every prompt id created (the step
	// copies for kind=blueprint, the single prompt for kind=prompt) so the
	// caller can render/link every row this install created. userID may be
	// "" (system-initiated install has no human actor, mirrors
	// RecordInstall); teamID is the installing team and is required.
	//
	// Rejects snap.SchemaVersion != domain.ListingSnapshotSchemaVersion
	// before writing anything — a snapshot format this method doesn't
	// understand must fail loudly rather than silently materialize rows
	// from a document it can't fully interpret.
	MaterializeListing(ctx context.Context, orgID, teamID string, snap domain.ListingSnapshot, listingID string, version int, userID string) (rootObjectID string, promptIDs []string, err error)

	// RecomputeStatsSystem recomputes and upserts marketplace_listing_stats
	// for every listing in orgID (TFAC-540) — teams_using (distinct
	// installing teams whose copy still exists), total_runs, success_rate,
	// and last_run_at, joined via marketplace_installs.root_object_id into
	// conversations.prompt_id (kind=prompt) or blueprint_runs.blueprint_id
	// (kind=blueprint). total_runs/success_rate/last_run_at count only
	// TERMINAL runs — a still-in-flight run hasn't resolved either way, so
	// it must not count as evidence of usage or (worse) silently score as a
	// failure against success_rate before it has actually failed; see
	// domain.ListingStats' doc comment. Deterministic and idempotent: a
	// re-run with unchanged underlying data overwrites each row with the
	// same computed values (ON CONFLICT (listing_id) DO UPDATE), so
	// overlapping or repeated cycles converge rather than drift. Admin
	// pool — see the interface doc comment. Called by the multi-mode-only
	// marketplacestats.Manager off the system:poll: sentinel, never from a
	// request handler.
	RecomputeStatsSystem(ctx context.Context, orgID string) error
}
