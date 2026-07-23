package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// marketplaceStore is the Postgres impl of db.MarketplaceStore. Every
// request-facing method runs on the app pool (RLS gates the write); q is
// that pool (or a tx bound to it). RecomputeStatsSystem is the one
// `...System` method (TFAC-540) and runs on admin instead — see the
// interface doc comment.
//
// snapshot is stored as jsonb; every read casts it to ::text so it scans
// cleanly into a Go string (mirrors event_handlers.scope_predicate_json),
// and every write casts the marshaled JSON string to ::jsonb. listing_id /
// id / source_id / user_id / team_id / root_object_id are uuid columns;
// every Go-string parameter bound against one carries an explicit ::uuid
// cast (mirrors secrets.go / staged_injections.go) rather than relying on
// implicit inference.
type marketplaceStore struct {
	q     queryer
	admin queryer
}

func newMarketplaceStore(q, admin queryer) db.MarketplaceStore {
	return &marketplaceStore{q: q, admin: admin}
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

// scanListingSummaryRowPG scans a listingColumnsLPG row plus the trailing
// computed columns (vote_count, install_count, viewer_voted,
// publisher_team_name, and the TFAC-540 stats columns) that List/Get
// append. EventTypes is left nil — callers attach it via
// fetchEventTypesForListingsPG.
func scanListingSummaryRowPG(scanFn func(dst ...any) error) (domain.ListingSummary, error) {
	var sum domain.ListingSummary
	var publisherTeamID, creatorUserID, sourceID, publisherTeamName sql.NullString
	var delistedAt sql.NullTime
	var voteCount, installCount, viewerVoted int
	var teamsUsing, totalRuns sql.NullInt64
	var successRate sql.NullFloat64
	var lastRunAt, computedAt sql.NullTime
	if err := scanFn(
		&sum.ID, &sum.OrgID, &sum.Scope, &sum.Kind, &sum.Status, &sum.Name, &sum.Description,
		&publisherTeamID, &creatorUserID, &sourceID, &sum.CurrentVersion, &sum.CreatedAt, &sum.UpdatedAt, &delistedAt,
		&voteCount, &installCount, &viewerVoted, &publisherTeamName,
		&teamsUsing, &totalRuns, &successRate, &lastRunAt, &computedAt,
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
	if publisherTeamName.Valid {
		sum.PublisherTeamName = publisherTeamName.String
	}
	// teamsUsing is the sentinel: RecomputeStatsSystem has never run for this
	// listing (no marketplace_listing_stats row to join) iff it's NULL — Stats
	// stays nil rather than a synthesized zero-value block (no wrong
	// fallbacks).
	if teamsUsing.Valid {
		stats := &domain.ListingStats{
			TeamsUsing: int(teamsUsing.Int64),
			TotalRuns:  int(totalRuns.Int64),
		}
		if successRate.Valid {
			stats.SuccessRate = &successRate.Float64
		}
		if lastRunAt.Valid {
			stats.LastRunAt = &lastRunAt.Time
		}
		if computedAt.Valid {
			stats.ComputedAt = computedAt.Time
		}
		sum.Stats = stats
	}
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
// columns plus the computed vote/install counts, the requesting viewer's
// vote state, and the TFAC-540 run-derived stats (a plain LEFT JOIN — the
// row is pre-computed by RecomputeStatsSystem, never derived here, so
// there's no RLS-crossing read at browse/detail time). $1 is the viewer
// user id (NULL when no viewer context — e.g. a background caller).
// Callers append their own WHERE/ORDER BY.
const listingSummaryQueryPG = `
	SELECT ` + listingColumnsLPG + `,
		COALESCE(v.vote_count, 0), COALESCE(i.install_count, 0),
		CASE WHEN mv.user_id IS NOT NULL THEN 1 ELSE 0 END,
		pt.name,
		ms.teams_using, ms.total_runs, ms.success_rate, ms.last_run_at, ms.computed_at
	FROM marketplace_listings l
	LEFT JOIN (SELECT listing_id, COUNT(*) AS vote_count FROM marketplace_votes GROUP BY listing_id) v ON v.listing_id = l.id
	LEFT JOIN (SELECT listing_id, COUNT(*) AS install_count FROM marketplace_installs GROUP BY listing_id) i ON i.listing_id = l.id
	LEFT JOIN marketplace_votes mv ON mv.listing_id = l.id AND mv.user_id = $1::uuid
	LEFT JOIN teams pt ON pt.id = l.publisher_team_id
	LEFT JOIN marketplace_listing_stats ms ON ms.listing_id = l.id
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
	case domain.ListingSortMostUsed:
		q += ` ORDER BY COALESCE(ms.total_runs, 0) DESC, l.updated_at DESC`
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

// MaterializeListing deep-copies snap into teamID and records the install,
// atomically. Mirrors BlueprintStore.DuplicatePrompts' deep-copy mechanics
// (fresh uuid.New() ids, creator_user_id = COALESCE(current_user_id, org
// owner)) but every copy is source='marketplace' rather than 'user' — see
// the interface doc comment. Consumes only snap/teamID/orgID: no query here
// ever references the listing's source_id or the publisher team, so a
// deleted source object or publisher team can't break an install.
func (s *marketplaceStore) MaterializeListing(ctx context.Context, orgID, teamID string, snap domain.ListingSnapshot, listingID string, version int, userID string) (string, []string, error) {
	if teamID == "" {
		return "", nil, errors.New("postgres marketplace: MaterializeListing requires team_id")
	}
	if snap.SchemaVersion != domain.ListingSnapshotSchemaVersion {
		return "", nil, fmt.Errorf("postgres marketplace: MaterializeListing unsupported snapshot schema_version %d (want %d)", snap.SchemaVersion, domain.ListingSnapshotSchemaVersion)
	}
	if len(snap.Steps) == 0 {
		return "", nil, errors.New("postgres marketplace: MaterializeListing requires at least one snapshot step")
	}
	if snap.Kind == domain.ListingKindPrompt && len(snap.Steps) != 1 {
		return "", nil, fmt.Errorf("postgres marketplace: MaterializeListing kind=prompt requires exactly one snapshot step, got %d", len(snap.Steps))
	}

	var rootObjectID string
	var promptIDs []string
	err := inTx(ctx, s.q, func(q queryer) error {
		promptIDs = make([]string, len(snap.Steps))
		for i, step := range snap.Steps {
			pid := uuid.New().String()
			if _, err := q.ExecContext(ctx, `
				INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body, source, allowed_tools, model, usage_count, created_at, updated_at)
				VALUES ($1, $2,
					COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
					$3::uuid, $4, $5, 'marketplace', $6, $7, 0, now(), now())
			`, pid, orgID, teamID, step.Name, step.Body, step.AllowedTools, step.Model); err != nil {
				return fmt.Errorf("copy snapshot step %d: %w", i, err)
			}
			promptIDs[i] = pid
		}

		switch snap.Kind {
		case domain.ListingKindPrompt:
			rootObjectID = promptIDs[0]
		case domain.ListingKindBlueprint:
			bpID := uuid.New().String()
			if _, err := q.ExecContext(ctx, `
				INSERT INTO blueprints (id, org_id, creator_user_id, team_id, name, source, usage_count, created_at, updated_at)
				VALUES ($1, $2,
					COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
					$3::uuid, $4, 'marketplace', 0, now(), now())
			`, bpID, orgID, teamID, snap.Name); err != nil {
				return fmt.Errorf("create blueprint from snapshot: %w", err)
			}
			for i, step := range snap.Steps {
				if _, err := q.ExecContext(ctx, `
					INSERT INTO blueprint_steps (org_id, team_id, blueprint_id, step_index, step_prompt_id, brief, created_at)
					VALUES ($1, $2::uuid, $3, $4, $5, $6, now())
				`, orgID, teamID, bpID, i, promptIDs[i], step.Brief); err != nil {
					return fmt.Errorf("insert blueprint step %d: %w", i, err)
				}
			}
			rootObjectID = bpID
		default:
			return fmt.Errorf("postgres marketplace: MaterializeListing unknown snapshot kind %q", snap.Kind)
		}

		_, err := q.ExecContext(ctx, `
			INSERT INTO marketplace_installs (listing_id, org_id, version, team_id, user_id, root_object_id, created_at)
			VALUES ($1::uuid, $2, $3, $4::uuid, $5::uuid, $6::uuid, now())
		`, listingID, orgID, version, teamID, nullIfEmpty(userID), rootObjectID)
		return err
	})
	if err != nil {
		return "", nil, err
	}
	return rootObjectID, promptIDs, nil
}

// blueprintRunTerminalStatusesSQL is the blueprint_runs.status counterpart
// to runTerminalStatusesSQL (run_queue.go) — the terminal set per
// domain.BlueprintRunStatus.Terminal(). No shared constant exists for this
// table today; defined here since this is the first blueprint_runs query
// that needs to distinguish terminal from in-flight.
const blueprintRunTerminalStatusesSQL = `'completed','aborted','failed','cancelled'`

// recomputePromptListingStatsPG upserts marketplace_listing_stats for every
// kind=prompt listing in $1. teams distinct-counts installing teams whose
// copy (a prompts row) still exists and isn't soft-deleted.
//
// runs_agg counts only TERMINAL runs (runTerminalStatusesSQL, run_queue.go)
// against any copy this listing has ever produced — a queued/cloning/running
// run hasn't resolved yet, so it must count toward neither total_runs nor
// success_rate until it does. Counting it as a not-yet-completed run would
// silently score it as a failure (dragging success_rate toward 0% for a run
// that hasn't actually failed) and inflate total_runs — and therefore
// sort=used — for listings with nothing but a burst of just-triggered,
// unresolved runs. Filtering the population once and deriving all three
// metrics (total_runs, success_rate, last_run_at) from it keeps them
// mutually consistent: a UI showing "N runs · X% success" is always X% of
// exactly N, never X% of some other, unfiltered N.
//
// Deliberately excludes copy existence from this count — root_object_id
// survives the copy's deletion on the install row (TFAC-535), so a
// listing's lifetime usage doesn't drop when a consumer cleans up their
// copy. Every prompt listing in the org gets a row, even one with zero
// installs (LEFT JOINs default to 0/NULL) — Get/List's Stats field then
// distinguishes "computed, zero activity" from "never computed" purely by
// row presence.
const recomputePromptListingStatsPG = `
	INSERT INTO marketplace_listing_stats (listing_id, org_id, teams_using, total_runs, success_rate, last_run_at, computed_at)
	SELECT
		l.id, l.org_id,
		COALESCE(teams.teams_using, 0),
		COALESCE(runs_agg.total_runs, 0),
		CASE WHEN COALESCE(runs_agg.total_runs, 0) > 0
			THEN runs_agg.completed_runs::double precision / runs_agg.total_runs
			ELSE NULL END,
		runs_agg.last_run_at,
		now()
	FROM marketplace_listings l
	LEFT JOIN (
		SELECT mi.listing_id, COUNT(DISTINCT mi.team_id) AS teams_using
		FROM marketplace_installs mi
		JOIN prompts p ON p.id = mi.root_object_id::text AND p.org_id = mi.org_id AND p.deleted_at IS NULL
		WHERE mi.org_id = $1
		GROUP BY mi.listing_id
	) teams ON teams.listing_id = l.id
	LEFT JOIN (
		SELECT mi.listing_id,
			COUNT(r.id) AS total_runs,
			SUM(CASE WHEN r.status = 'completed' THEN 1 ELSE 0 END) AS completed_runs,
			MAX(r.started_at) AS last_run_at
		FROM (SELECT DISTINCT listing_id, root_object_id FROM marketplace_installs WHERE org_id = $1 AND root_object_id IS NOT NULL) mi
		JOIN conversations r ON r.prompt_id = mi.root_object_id::text AND r.org_id = $1
		WHERE r.status IN (` + runTerminalStatusesSQL + `)
		GROUP BY mi.listing_id
	) runs_agg ON runs_agg.listing_id = l.id
	WHERE l.org_id = $1 AND l.kind = 'prompt'
	ON CONFLICT (listing_id) DO UPDATE SET
		org_id = EXCLUDED.org_id,
		teams_using = EXCLUDED.teams_using,
		total_runs = EXCLUDED.total_runs,
		success_rate = EXCLUDED.success_rate,
		last_run_at = EXCLUDED.last_run_at,
		computed_at = EXCLUDED.computed_at
`

// recomputeBlueprintListingStatsPG mirrors recomputePromptListingStatsPG for
// kind=blueprint listings: copies live in blueprints (not prompts), runs
// live in blueprint_runs.blueprint_id (not runs.prompt_id) filtered to
// blueprintRunTerminalStatusesSQL instead of runTerminalStatusesSQL —
// everything else, including the terminal-only rationale, is identical.
const recomputeBlueprintListingStatsPG = `
	INSERT INTO marketplace_listing_stats (listing_id, org_id, teams_using, total_runs, success_rate, last_run_at, computed_at)
	SELECT
		l.id, l.org_id,
		COALESCE(teams.teams_using, 0),
		COALESCE(runs_agg.total_runs, 0),
		CASE WHEN COALESCE(runs_agg.total_runs, 0) > 0
			THEN runs_agg.completed_runs::double precision / runs_agg.total_runs
			ELSE NULL END,
		runs_agg.last_run_at,
		now()
	FROM marketplace_listings l
	LEFT JOIN (
		SELECT mi.listing_id, COUNT(DISTINCT mi.team_id) AS teams_using
		FROM marketplace_installs mi
		JOIN blueprints b ON b.id = mi.root_object_id::text AND b.org_id = mi.org_id AND b.deleted_at IS NULL
		WHERE mi.org_id = $1
		GROUP BY mi.listing_id
	) teams ON teams.listing_id = l.id
	LEFT JOIN (
		SELECT mi.listing_id,
			COUNT(br.id) AS total_runs,
			SUM(CASE WHEN br.status = 'completed' THEN 1 ELSE 0 END) AS completed_runs,
			MAX(br.started_at) AS last_run_at
		FROM (SELECT DISTINCT listing_id, root_object_id FROM marketplace_installs WHERE org_id = $1 AND root_object_id IS NOT NULL) mi
		JOIN blueprint_runs br ON br.blueprint_id = mi.root_object_id::text AND br.org_id = $1
		WHERE br.status IN (` + blueprintRunTerminalStatusesSQL + `)
		GROUP BY mi.listing_id
	) runs_agg ON runs_agg.listing_id = l.id
	WHERE l.org_id = $1 AND l.kind = 'blueprint'
	ON CONFLICT (listing_id) DO UPDATE SET
		org_id = EXCLUDED.org_id,
		teams_using = EXCLUDED.teams_using,
		total_runs = EXCLUDED.total_runs,
		success_rate = EXCLUDED.success_rate,
		last_run_at = EXCLUDED.last_run_at,
		computed_at = EXCLUDED.computed_at
`

// RecomputeStatsSystem recomputes every listing's stats in orgID in one
// transaction (prompt listings, then blueprint listings) — see the
// db.MarketplaceStore interface doc comment for why this runs on the admin
// pool rather than the app pool every other method here uses.
func (s *marketplaceStore) RecomputeStatsSystem(ctx context.Context, orgID string) error {
	return inTx(ctx, s.admin, func(q queryer) error {
		if _, err := q.ExecContext(ctx, recomputePromptListingStatsPG, orgID); err != nil {
			return fmt.Errorf("recompute prompt listing stats: %w", err)
		}
		if _, err := q.ExecContext(ctx, recomputeBlueprintListingStatsPG, orgID); err != nil {
			return fmt.Errorf("recompute blueprint listing stats: %w", err)
		}
		return nil
	})
}
