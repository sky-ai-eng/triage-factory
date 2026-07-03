package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// marketplaceStore is the Postgres impl of db.MarketplaceStore. Single app
// pool — every method is request-facing (an org member browsing, publishing
// to their own team, or voting/installing as themselves) and RLS gates
// every write; there is no admin-pool split like PromptStore.SeedOrUpdate
// needs. Multi-mode only — the SQLite impl is a stub (see
// internal/db/sqlite/marketplace_store.go).
//
// snapshot is stored as jsonb; every read casts it to ::text so it scans
// cleanly into a Go string (mirrors event_handlers.scope_predicate_json),
// and every write casts the marshaled JSON string to ::jsonb. listing_id /
// id / source_id / user_id / team_id / root_object_id are uuid columns;
// every Go-string parameter bound against one carries an explicit ::uuid
// cast (mirrors secrets.go / staged_injections.go) rather than relying on
// implicit inference.
type marketplaceStore struct {
	q queryer
}

func newMarketplaceStore(q queryer) db.MarketplaceStore {
	return &marketplaceStore{q: q}
}

var _ db.MarketplaceStore = (*marketplaceStore)(nil)

func (s *marketplaceStore) Publish(ctx context.Context, orgID string, l domain.MarketplaceListing, snap domain.ListingSnapshot) (string, error) {
	if l.PublisherTeamID == "" {
		return "", errors.New("postgres marketplace Publish: publisher_team_id required (handler must thread the resolved acting team)")
	}
	scope := l.Scope
	if scope == "" {
		scope = domain.ListingScopeOrg
	}
	kind := l.Kind
	if kind == "" {
		kind = snap.Kind
	}
	snapJSON, err := json.Marshal(snap)
	if err != nil {
		return "", fmt.Errorf("marshal snapshot: %w", err)
	}

	var id string
	err = inTx(ctx, s.q, func(q queryer) error {
		if err := q.QueryRowContext(ctx, `
			INSERT INTO marketplace_listings
				(org_id, scope, kind, status, name, description, publisher_team_id, creator_user_id, source_id, current_version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7::uuid,
				COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $1)),
				$8::uuid, 1, now(), now())
			RETURNING id
		`, orgID, scope, kind, domain.ListingStatusPublished, l.Name, l.Description, l.PublisherTeamID, nullIfEmpty(l.SourceID),
		).Scan(&id); err != nil {
			return fmt.Errorf("insert listing: %w", err)
		}
		if _, err := q.ExecContext(ctx, `
			INSERT INTO marketplace_listing_versions (listing_id, org_id, version, snapshot, creator_user_id, created_at)
			VALUES ($1::uuid, $2, 1, $3::jsonb,
				COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
				now())
		`, id, orgID, string(snapJSON)); err != nil {
			return fmt.Errorf("insert version 1: %w", err)
		}
		return insertListingEventsPG(ctx, q, orgID, id, snap.EventTypes)
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *marketplaceStore) PublishVersion(ctx context.Context, orgID, listingID string, snap domain.ListingSnapshot, name, desc string, eventTypes []string) (int, error) {
	snapJSON, err := json.Marshal(snap)
	if err != nil {
		return 0, fmt.Errorf("marshal snapshot: %w", err)
	}

	var newVersion int
	err = inTx(ctx, s.q, func(q queryer) error {
		switch err := q.QueryRowContext(ctx, `
			UPDATE marketplace_listings
			SET name = $1, description = $2, current_version = current_version + 1, updated_at = now()
			WHERE org_id = $3 AND id = $4::uuid
			RETURNING current_version
		`, name, desc, orgID, listingID).Scan(&newVersion); {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("marketplace listing %s not found", listingID)
		case err != nil:
			return fmt.Errorf("update listing: %w", err)
		}
		if _, err := q.ExecContext(ctx, `
			INSERT INTO marketplace_listing_versions (listing_id, org_id, version, snapshot, creator_user_id, created_at)
			VALUES ($1::uuid, $2, $3, $4::jsonb,
				COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
				now())
		`, listingID, orgID, newVersion, string(snapJSON)); err != nil {
			return fmt.Errorf("insert version %d: %w", newVersion, err)
		}
		if _, err := q.ExecContext(ctx, `DELETE FROM marketplace_listing_events WHERE org_id = $1 AND listing_id = $2::uuid`, orgID, listingID); err != nil {
			return fmt.Errorf("clear listing events: %w", err)
		}
		return insertListingEventsPG(ctx, q, orgID, listingID, eventTypes)
	})
	if err != nil {
		return 0, err
	}
	return newVersion, nil
}

// insertListingEventsPG inserts the (deduped) facet rows for a listing.
// Dedup guards the PK (listing_id, event_type) against a caller passing the
// same event type twice.
func insertListingEventsPG(ctx context.Context, q queryer, orgID, listingID string, eventTypes []string) error {
	seen := make(map[string]bool, len(eventTypes))
	for _, et := range eventTypes {
		if et == "" || seen[et] {
			continue
		}
		seen[et] = true
		if _, err := q.ExecContext(ctx, `
			INSERT INTO marketplace_listing_events (listing_id, org_id, event_type) VALUES ($1::uuid, $2, $3)
		`, listingID, orgID, et); err != nil {
			return fmt.Errorf("insert listing event %q: %w", et, err)
		}
	}
	return nil
}

func (s *marketplaceStore) Delist(ctx context.Context, orgID, listingID string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE marketplace_listings SET status = $1, delisted_at = now(), updated_at = now() WHERE org_id = $2 AND id = $3::uuid
	`, domain.ListingStatusDelisted, orgID, listingID)
	return err
}

func (s *marketplaceStore) Relist(ctx context.Context, orgID, listingID string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE marketplace_listings SET status = $1, delisted_at = NULL, updated_at = now() WHERE org_id = $2 AND id = $3::uuid
	`, domain.ListingStatusPublished, orgID, listingID)
	return err
}

// listingColumnsPG is the canonical unqualified column order
// scanListingRowPG expects — for plain single-table reads (no join, so no
// ambiguity risk).
const listingColumnsPG = `id, org_id, scope, kind, status, name, description,
	publisher_team_id, creator_user_id, source_id, current_version, created_at, updated_at, delisted_at`

// listingColumnsLPG is listingColumnsPG qualified with the `l` alias used by
// List/Get, whose joins (marketplace_votes as mv) also carry an org_id
// column — unqualified names would be ambiguous once joined.
const listingColumnsLPG = `l.id, l.org_id, l.scope, l.kind, l.status, l.name, l.description,
	l.publisher_team_id, l.creator_user_id, l.source_id, l.current_version, l.created_at, l.updated_at, l.delisted_at`

func scanListingRowPG(scanFn func(dst ...any) error) (domain.MarketplaceListing, error) {
	var l domain.MarketplaceListing
	var publisherTeamID, creatorUserID, sourceID sql.NullString
	var delistedAt sql.NullTime
	if err := scanFn(&l.ID, &l.OrgID, &l.Scope, &l.Kind, &l.Status, &l.Name, &l.Description,
		&publisherTeamID, &creatorUserID, &sourceID, &l.CurrentVersion, &l.CreatedAt, &l.UpdatedAt, &delistedAt); err != nil {
		return l, err
	}
	if publisherTeamID.Valid {
		l.PublisherTeamID = publisherTeamID.String
	}
	if creatorUserID.Valid {
		l.CreatorUserID = creatorUserID.String
	}
	if sourceID.Valid {
		l.SourceID = sourceID.String
	}
	if delistedAt.Valid {
		l.DelistedAt = &delistedAt.Time
	}
	return l, nil
}

// scanListingSummaryRowPG scans a listingColumnsLPG row plus the three
// trailing computed columns (vote_count, install_count, viewer_voted) that
// List/Get append. EventTypes is left nil — callers attach it via
// fetchEventTypesForListingsPG.
func scanListingSummaryRowPG(scanFn func(dst ...any) error) (domain.ListingSummary, error) {
	var sum domain.ListingSummary
	var publisherTeamID, creatorUserID, sourceID sql.NullString
	var delistedAt sql.NullTime
	var voteCount, installCount, viewerVoted int
	if err := scanFn(
		&sum.ID, &sum.OrgID, &sum.Scope, &sum.Kind, &sum.Status, &sum.Name, &sum.Description,
		&publisherTeamID, &creatorUserID, &sourceID, &sum.CurrentVersion, &sum.CreatedAt, &sum.UpdatedAt, &delistedAt,
		&voteCount, &installCount, &viewerVoted,
	); err != nil {
		return sum, err
	}
	if publisherTeamID.Valid {
		sum.PublisherTeamID = publisherTeamID.String
	}
	if creatorUserID.Valid {
		sum.CreatorUserID = creatorUserID.String
	}
	if sourceID.Valid {
		sum.SourceID = sourceID.String
	}
	if delistedAt.Valid {
		sum.DelistedAt = &delistedAt.Time
	}
	sum.VoteCount = voteCount
	sum.InstallCount = installCount
	sum.ViewerVoted = viewerVoted != 0
	return sum, nil
}

// fetchEventTypesForListingsPG groups marketplace_listing_events rows by
// listing_id for the given ids. A second small query rather than a
// GROUP_CONCAT/array_agg in the header SQL, since org-scale N is small
// (matches the store's no-denormalized-counters stance elsewhere).
func fetchEventTypesForListingsPG(ctx context.Context, q queryer, orgID string, listingIDs []string) (map[string][]string, error) {
	out := make(map[string][]string, len(listingIDs))
	if len(listingIDs) == 0 {
		return out, nil
	}
	rows, err := q.QueryContext(ctx, `
		SELECT listing_id, event_type FROM marketplace_listing_events
		WHERE org_id = $1 AND listing_id = ANY($2) ORDER BY event_type
	`, orgID, pgUUIDArray(listingIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var lid, et string
		if err := rows.Scan(&lid, &et); err != nil {
			return nil, err
		}
		out[lid] = append(out[lid], et)
	}
	return out, rows.Err()
}

// listingSummaryQueryPG is the shared List/Get header query: listing
// columns plus the computed vote/install counts and the requesting
// viewer's vote state, joined so a single round trip covers all three.
// $1 is the viewer user id (NULL when no viewer context — e.g. a
// background caller). Callers append their own WHERE/ORDER BY.
const listingSummaryQueryPG = `
	SELECT ` + listingColumnsLPG + `,
		COALESCE(v.vote_count, 0), COALESCE(i.install_count, 0),
		CASE WHEN mv.user_id IS NOT NULL THEN 1 ELSE 0 END
	FROM marketplace_listings l
	LEFT JOIN (SELECT listing_id, COUNT(*) AS vote_count FROM marketplace_votes GROUP BY listing_id) v ON v.listing_id = l.id
	LEFT JOIN (SELECT listing_id, COUNT(*) AS install_count FROM marketplace_installs GROUP BY listing_id) i ON i.listing_id = l.id
	LEFT JOIN marketplace_votes mv ON mv.listing_id = l.id AND mv.user_id = $1::uuid
`

func (s *marketplaceStore) List(ctx context.Context, orgID string, viewerUserID string, f domain.ListingFilter) ([]domain.ListingSummary, error) {
	args := []any{nullIfEmpty(viewerUserID), orgID, domain.ListingStatusPublished}
	q := listingSummaryQueryPG + ` WHERE l.org_id = $2 AND l.status = $3`
	if f.Kind != "" {
		args = append(args, f.Kind)
		q += fmt.Sprintf(" AND l.kind = $%d", len(args))
	}
	if f.Query != "" {
		args = append(args, "%"+f.Query+"%")
		q += fmt.Sprintf(" AND (l.name ILIKE $%d OR l.description ILIKE $%d)", len(args), len(args))
	}
	if f.EventType != "" {
		args = append(args, f.EventType)
		q += fmt.Sprintf(" AND EXISTS (SELECT 1 FROM marketplace_listing_events e WHERE e.listing_id = l.id AND e.event_type = $%d)", len(args))
	}
	switch f.Sort {
	case domain.ListingSortInstalls:
		q += ` ORDER BY COALESCE(i.install_count, 0) DESC, l.updated_at DESC`
	case domain.ListingSortVotes:
		q += ` ORDER BY COALESCE(v.vote_count, 0) DESC, l.updated_at DESC`
	default:
		q += ` ORDER BY l.updated_at DESC`
	}
	rows, err := s.q.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.ListingSummary
	for rows.Next() {
		sum, err := scanListingSummaryRowPG(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, sum)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	ids := make([]string, len(out))
	for i, l := range out {
		ids[i] = l.ID
	}
	eventTypes, err := fetchEventTypesForListingsPG(ctx, s.q, orgID, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].EventTypes = eventTypes[out[i].ID]
	}
	return out, nil
}

// Get returns the full detail for one listing, or propagates sql.ErrNoRows
// when it doesn't exist / isn't visible under RLS.
func (s *marketplaceStore) Get(ctx context.Context, orgID, listingID, viewerUserID string) (domain.ListingDetail, error) {
	q := listingSummaryQueryPG + ` WHERE l.org_id = $2 AND l.id = $3::uuid`
	summary, err := scanListingSummaryRowPG(s.q.QueryRowContext(ctx, q, nullIfEmpty(viewerUserID), orgID, listingID).Scan)
	if err != nil {
		return domain.ListingDetail{}, err
	}

	eventTypes, err := fetchEventTypesForListingsPG(ctx, s.q, orgID, []string{listingID})
	if err != nil {
		return domain.ListingDetail{}, err
	}
	summary.EventTypes = eventTypes[listingID]

	versions, err := listListingVersionsPG(ctx, s.q, orgID, listingID)
	if err != nil {
		return domain.ListingDetail{}, err
	}
	var current domain.ListingSnapshot
	var foundCurrent bool
	for _, v := range versions {
		if v.Version == summary.CurrentVersion {
			current = v.Snapshot
			foundCurrent = true
			break
		}
	}
	if !foundCurrent {
		// Data-integrity guard: current_version is written by PublishVersion
		// in the same tx as the version row, so this should be unreachable —
		// surface it loudly rather than silently handing back a zero-value
		// snapshot that reads as "empty listing" to the caller.
		return domain.ListingDetail{}, fmt.Errorf("marketplace listing %s: current_version %d has no matching version row", listingID, summary.CurrentVersion)
	}
	return domain.ListingDetail{ListingSummary: summary, CurrentSnapshot: current, Versions: versions}, nil
}

func listListingVersionsPG(ctx context.Context, q queryer, orgID, listingID string) ([]domain.ListingVersion, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT listing_id, version, snapshot::text, creator_user_id, created_at
		FROM marketplace_listing_versions WHERE org_id = $1 AND listing_id = $2::uuid ORDER BY version
	`, orgID, listingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.ListingVersion
	for rows.Next() {
		var v domain.ListingVersion
		var snapJSON string
		var creator sql.NullString
		if err := rows.Scan(&v.ListingID, &v.Version, &snapJSON, &creator, &v.CreatedAt); err != nil {
			return nil, err
		}
		if creator.Valid {
			v.CreatorUserID = creator.String
		}
		if err := json.Unmarshal([]byte(snapJSON), &v.Snapshot); err != nil {
			return nil, fmt.Errorf("unmarshal snapshot v%d: %w", v.Version, err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetActiveBySource resolves the org's currently-published listing for a
// team-side source object, or (nil, nil) if none. sourceID == "" always
// misses (source_id NULL never equals an empty string, and the column is
// uuid so binding "" would fail the cast) — short-circuit before the query.
func (s *marketplaceStore) GetActiveBySource(ctx context.Context, orgID, sourceID string) (*domain.MarketplaceListing, error) {
	if sourceID == "" {
		return nil, nil
	}
	l, err := scanListingRowPG(s.q.QueryRowContext(ctx, `
		SELECT `+listingColumnsPG+`
		FROM marketplace_listings WHERE org_id = $1 AND source_id = $2::uuid AND status = $3
	`, orgID, sourceID, domain.ListingStatusPublished).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// GetBySource mirrors GetActiveBySource but drops the status filter — it
// resolves a listing for source_id regardless of published/delisted state —
// and returns the full ListingSummary (event types + counts), reusing the
// same listingSummaryQueryPG join List/Get use. No viewer context (this is
// not a per-user browse read), so ViewerVoted is always false here.
// sourceID == "" short-circuits for the same reason GetActiveBySource does.
func (s *marketplaceStore) GetBySource(ctx context.Context, orgID, sourceID string) (*domain.ListingSummary, error) {
	if sourceID == "" {
		return nil, nil
	}
	q := listingSummaryQueryPG + ` WHERE l.org_id = $2 AND l.source_id = $3::uuid`
	summary, err := scanListingSummaryRowPG(s.q.QueryRowContext(ctx, q, nullIfEmpty(""), orgID, sourceID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	eventTypes, err := fetchEventTypesForListingsPG(ctx, s.q, orgID, []string{summary.ID})
	if err != nil {
		return nil, err
	}
	summary.EventTypes = eventTypes[summary.ID]
	return &summary, nil
}

func (s *marketplaceStore) Vote(ctx context.Context, orgID, listingID, userID string) error {
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO marketplace_votes (listing_id, org_id, user_id, created_at) VALUES ($1::uuid, $2, $3::uuid, now())
		ON CONFLICT (listing_id, user_id) DO NOTHING
	`, listingID, orgID, userID)
	return err
}

func (s *marketplaceStore) Unvote(ctx context.Context, orgID, listingID, userID string) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM marketplace_votes WHERE org_id = $1 AND listing_id = $2::uuid AND user_id = $3::uuid`, orgID, listingID, userID)
	return err
}

func (s *marketplaceStore) RecordInstall(ctx context.Context, orgID, listingID string, version int, teamID, userID, rootObjectID string) error {
	if teamID == "" {
		return errors.New("postgres marketplace: RecordInstall requires team_id")
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO marketplace_installs (listing_id, org_id, version, team_id, user_id, root_object_id, created_at)
		VALUES ($1::uuid, $2, $3, $4::uuid, $5::uuid, $6::uuid, now())
	`, listingID, orgID, version, teamID, nullIfEmpty(userID), nullIfEmpty(rootObjectID))
	return err
}
