package domain

import "time"

// Marketplace listing kind / scope / status. App-validated open sets — no DB
// CHECK constraint, same reasoning as Prompt.Source (prompts.source,
// baseline.sql:68-72): 'global' scope and moderation statuses (e.g.
// 'pending_review') must be addable later without a SQLite table rebuild.
const (
	ListingKindPrompt    = "prompt"
	ListingKindBlueprint = "blueprint"

	// ListingScopeOrg is the only scope V1 writes. 'global' is reserved for
	// the cross-org phase (TFAC-92 epic, phase 2) — nothing in code today
	// produces or reads it.
	ListingScopeOrg = "org"

	ListingStatusPublished = "published"
	ListingStatusDelisted  = "delisted"
)

// Listing sort orders accepted by ListingFilter.Sort.
const (
	ListingSortInstalls = "installs"
	ListingSortVotes    = "votes"
	ListingSortRecent   = "recent"
)

// ListingSnapshotSchemaVersion is the only ListingSnapshot.SchemaVersion V1
// ever writes or reads — the single source of truth so the write side
// (buildListingSnapshot) and the install-time validation
// (MaterializeListing) can't drift apart. A future incompatible snapshot
// format bumps this and teaches the read/install paths to branch on it;
// nothing does today.
const ListingSnapshotSchemaVersion = 1

// MarketplaceListing is one published entry in the within-org prompt
// marketplace — a durable pointer at a versioned, self-contained snapshot of
// a team's prompt or blueprint (TFAC-535). Copy-on-publish throughout:
// nothing here FKs into the team-owned prompts/blueprints tables (SourceID
// is a plain lookup key, not a foreign key), and nothing a consumer copies
// FKs back to a listing — deliberate cross-org insurance so a future global
// marketplace can feed the same installer with the same snapshot documents.
type MarketplaceListing struct {
	ID    string `json:"id"`
	OrgID string `json:"org_id"`
	// Scope is ListingScopeOrg today; ListingScope* documents the reserved
	// 'global' value for the cross-org phase.
	Scope string `json:"scope"`
	// Kind is ListingKind* — the listing unit is either a standalone prompt
	// (a building block consumers compose into their own blueprints) or a
	// full blueprint (its step prompts are inlined into the snapshot).
	Kind string `json:"kind"`
	// Status is ListingStatus*. Delisted listings stay resolvable to their
	// publisher (relist / manage) but drop out of every other member's browse.
	Status      string `json:"status"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// PublisherTeamID is nullable: a listing outlives the team that
	// published it (ON DELETE SET NULL). An orphaned listing (NULL here)
	// becomes unwritable — tf.user_can_write_team(NULL) is always false —
	// which is the accepted tradeoff; no ticket reassigns orphaned listings.
	PublisherTeamID string `json:"publisher_team_id,omitempty"`
	// CreatorUserID is nullable: a listing outlives its creator (ON DELETE
	// SET NULL). Attribution only; RLS write checks key on PublisherTeamID.
	CreatorUserID string `json:"creator_user_id,omitempty"`
	// SourceID is the team-side blueprint/prompt id this listing was
	// published from — plain text, NO FK, so the listing survives source
	// deletion. Used by GetActiveBySource to resolve the republish target.
	SourceID       string     `json:"source_id,omitempty"`
	CurrentVersion int        `json:"current_version"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DelistedAt     *time.Time `json:"delisted_at,omitempty"`
}

// ListingSnapshot is the immutable, self-contained document a listing
// version carries (TFAC-535's canonical definition — publish and install
// both consume this). It must contain nothing org-internal: no ids, no
// source references, no publisher attribution (that lives on the listing
// row, never in the snapshot) — so it can cross an org boundary verbatim
// once the cross-org marketplace (phase 2) lands. The installer consumes
// this document, never source rows.
type ListingSnapshot struct {
	SchemaVersion int            `json:"schema_version"` // ListingSnapshotSchemaVersion
	Kind          string         `json:"kind"`           // ListingKind*
	Name          string         `json:"name"`
	Description   string         `json:"description"`
	EventTypes    []string       `json:"event_types"`
	Steps         []SnapshotStep `json:"steps"` // exactly 1 for kind=prompt
}

// SnapshotStep is one step of a ListingSnapshot. Modeled on
// BlueprintPlanStep (blueprint.go:112) but with no IDs and no source field —
// the snapshot must contain nothing org-internal. Brief is "" for
// kind=prompt (a standalone prompt listing has exactly one step and no
// wrapper brief).
type SnapshotStep struct {
	StepIndex    int    `json:"step_index"`
	Name         string `json:"name"` // prompt name
	Body         string `json:"body"`
	AllowedTools string `json:"allowed_tools"`
	Model        string `json:"model"`
	Brief        string `json:"brief"` // "" for kind=prompt
}

// ListingVersion is one immutable, already-written snapshot revision of a
// listing — the marketplace_listing_versions row shape.
type ListingVersion struct {
	ListingID     string          `json:"listing_id"`
	Version       int             `json:"version"`
	Snapshot      ListingSnapshot `json:"snapshot"`
	CreatorUserID string          `json:"creator_user_id,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

// ListingFilter bounds a MarketplaceStore.List browse read. Every field is
// optional: an empty string drops that clause. Query does a name/description
// substring match; EventType narrows to listings faceted on that event type
// (marketplace_listing_events, sourced from events_catalog); Kind narrows to
// ListingKindPrompt/ListingKindBlueprint. Sort is one of ListingSort* and
// defaults to ListingSortRecent when empty.
type ListingFilter struct {
	Query     string
	EventType string
	Kind      string
	Sort      string
}

// ListingSummary is one row of a browse list: the listing header plus
// computed counts. No denormalized counters (TFAC-535) — VoteCount /
// InstallCount are COUNT(*) joins done at read time, not columns on
// MarketplaceListing. ViewerVoted reflects the requesting user's own vote
// state so the browse UI can render a filled/unfilled vote control without a
// second round trip.
type ListingSummary struct {
	MarketplaceListing
	EventTypes   []string `json:"event_types"`
	VoteCount    int      `json:"vote_count"`
	InstallCount int      `json:"install_count"`
	ViewerVoted  bool     `json:"viewer_voted"`
	// PublisherTeamName is a joined display label for MarketplaceListing's
	// PublisherTeamID — the browse card and detail view show "who published
	// this" and the org-wide teams RLS policy (org_id + user_has_org_access,
	// no per-team membership check) lets any org member resolve any team's
	// name, unlike the caller-scoped /api/teams list. Empty when
	// PublisherTeamID is "" (the publishing team was later deleted).
	PublisherTeamName string `json:"publisher_team_name,omitempty"`
}

// ListingDetail is the full single-listing view: the summary (header +
// counts) plus the current snapshot content and the version history — what
// the listing detail page and the install flow both read.
type ListingDetail struct {
	ListingSummary
	CurrentSnapshot ListingSnapshot  `json:"current_snapshot"`
	Versions        []ListingVersion `json:"versions"`
}
