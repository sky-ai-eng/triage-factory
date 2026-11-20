package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// pgListedEntityCols is pgEntitySelectCols over the alias e, followed by the
// latest event's type and detection time from the lateral join in List.
const pgListedEntityCols = `e.id, e.source, e.scope, e.source_id, COALESCE(e.external_id, ''), e.kind,
       COALESCE(e.title, ''), COALESCE(e.url, ''),
       COALESCE(e.snapshot_json::text, ''), COALESCE(e.description, ''), e.state,
       e.created_at, e.last_polled_at, e.closed_at, e.poll_seq, COALESCE(e.linear_team_id, ''),
       COALESCE(e.repository_id::text, ''), COALESCE(e.jira_project_id::text, ''),
       latest.event_type, latest.created_at`

// pgEntityListWhere renders db.EntityListFilter as a WHERE body over the alias
// e plus its args, org_id at $1. ok=false means the filter can match nothing,
// so the caller answers without querying.
func pgEntityListWhere(orgID string, f db.EntityListFilter) (string, []any, bool) {
	args := []any{orgID}
	clauses := []string{"e.org_id = $1"}
	if len(f.IDs) > 0 {
		// entities.id is a uuid column, so an id that does not parse is
		// dropped rather than failing the bind; a set of only such ids asked
		// for no entity, which must not widen to every one.
		ids := filterValidUUIDs(f.IDs)
		if len(ids) == 0 {
			return "", nil, false
		}
		args = append(args, pgUUIDArray(ids))
		clauses = append(clauses, fmt.Sprintf("e.id = ANY($%d::uuid[])", len(args)))
	}
	if len(f.Sources) > 0 {
		args = append(args, f.Sources)
		clauses = append(clauses, fmt.Sprintf("e.source = ANY($%d)", len(args)))
	}
	if len(f.Kinds) > 0 {
		args = append(args, f.Kinds)
		clauses = append(clauses, fmt.Sprintf("e.kind = ANY($%d)", len(args)))
	}
	if len(f.States) > 0 {
		args = append(args, f.States)
		clauses = append(clauses, fmt.Sprintf("e.state = ANY($%d)", len(args)))
	}
	if f.ClosedSince != nil {
		args = append(args, f.ClosedSince.UTC())
		clauses = append(clauses, fmt.Sprintf("(e.closed_at IS NULL OR e.closed_at >= $%d)", len(args)))
	}
	if len(f.TeamIDs) > 0 && len(filterValidUUIDs(f.TeamIDs)) != len(f.TeamIDs) {
		// The team ids bind against uuid columns. The route refuses a
		// malformed one; a caller that reaches here with one gets nothing
		// rather than a cast error.
		return "", nil, false
	}
	tracked, args := entityTracked(f.TeamIDs, args)
	clauses = append(clauses, tracked)
	return strings.Join(clauses, " AND "), args, true
}

func (s *entityStore) List(ctx context.Context, orgID string, f db.EntityListFilter, opts db.ListOpts) ([]domain.ListedEntity, int, error) {
	where, args, ok := pgEntityListWhere(orgID, f)
	if !ok {
		return []domain.ListedEntity{}, 0, nil
	}
	var total int
	if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM entities e WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []domain.ListedEntity{}, total, nil
	}

	// The latest event is one probe of idx_events_org_entity_created per row
	// of the page. seq breaks a created_at tie the way it breaks every one in
	// this table: two events can share a timestamp, and id is a random UUID.
	query := `
		SELECT ` + pgListedEntityCols + `
		FROM entities e
		LEFT JOIN LATERAL (
			SELECT ev.event_type, ev.created_at
			FROM events ev
			WHERE ev.org_id = e.org_id AND ev.entity_id = e.id
			ORDER BY ev.created_at DESC, ev.seq DESC
			LIMIT 1
		) latest ON TRUE
		WHERE ` + where + `
		ORDER BY e.created_at DESC, e.id DESC`
	if opts.Limit > 0 {
		args = append(args, opts.Limit, opts.Offset)
		query += fmt.Sprintf(`
		LIMIT $%d OFFSET $%d`, len(args)-1, len(args))
	}
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []domain.ListedEntity{}
	for rows.Next() {
		var item domain.ListedEntity
		e := &item.Entity
		var latestType sql.NullString
		var latestAt sql.NullTime
		if err := rows.Scan(&e.ID, &e.Source, &e.Scope, &e.SourceID, &e.ExternalID, &e.Kind, &e.Title, &e.URL,
			&e.SnapshotJSON, &e.Description, &e.State,
			&e.CreatedAt, &e.LastPolledAt, &e.ClosedAt, &e.PollSeq, &e.LinearTeamID, &e.RepositoryID, &e.JiraProjectID,
			&latestType, &latestAt); err != nil {
			return nil, 0, err
		}
		item.LatestEventType = latestType.String
		if latestAt.Valid {
			at := latestAt.Time.UTC()
			item.LatestEventAt = &at
		}
		out = append(out, item)
	}
	return out, total, rows.Err()
}

// pgEventListWhere renders db.EventListFilter as a WHERE body over the event
// alias ev joined to its entity e, plus its args, org_id at $1. ok=false means
// the filter can match nothing.
func pgEventListWhere(orgID string, f db.EventListFilter) (string, []any, bool) {
	args := []any{orgID}
	clauses := []string{"ev.org_id = $1"}
	if len(f.IDs) > 0 {
		ids := filterValidUUIDs(f.IDs)
		if len(ids) == 0 {
			return "", nil, false
		}
		args = append(args, pgUUIDArray(ids))
		clauses = append(clauses, fmt.Sprintf("ev.id = ANY($%d::uuid[])", len(args)))
	}
	if len(f.EntityIDs) > 0 {
		ids := filterValidUUIDs(f.EntityIDs)
		if len(ids) == 0 {
			return "", nil, false
		}
		args = append(args, pgUUIDArray(ids))
		clauses = append(clauses, fmt.Sprintf("ev.entity_id = ANY($%d::uuid[])", len(args)))
	}
	if f.Since != nil {
		args = append(args, f.Since.UTC())
		clauses = append(clauses, fmt.Sprintf("ev.created_at >= $%d", len(args)))
	}
	// events RLS is org-wide, so the entity's tracked set is what keeps an
	// event of another team's entity out, as it keeps the entity itself out
	// of the entity list.
	clauses = append(clauses, entityTrackedExists)
	return strings.Join(clauses, " AND "), args, true
}

func (s *eventStore) List(ctx context.Context, orgID string, f db.EventListFilter, opts db.ListOpts) ([]db.ListedEvent, int, error) {
	where, args, ok := pgEventListWhere(orgID, f)
	if !ok {
		return []db.ListedEvent{}, 0, nil
	}
	// The inner join to entities is what drops a system event: its NULL
	// entity_id joins to no row.
	from := `
		FROM events ev
		JOIN entities e ON e.id = ev.entity_id AND e.org_id = ev.org_id
		WHERE ` + where
	if f.PerEntity > 0 {
		// Ranked before the keyset cut below, so a row's rank is the same on
		// every page of one walk.
		args = append(args, f.PerEntity)
		from = `
		FROM (
			SELECT ev.*, ROW_NUMBER() OVER (PARTITION BY ev.entity_id ORDER BY ev.seq DESC) AS entity_rank` + from + `
		) ev
		WHERE ev.entity_rank <= ` + fmt.Sprintf("$%d", len(args))
	}
	var total int
	if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*)`+from, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []db.ListedEvent{}, total, nil
	}
	keyset := ""
	if len(opts.After) > 0 {
		after, err := db.EventListCursor(opts.After)
		if err != nil {
			return nil, 0, err
		}
		args = append(args, after)
		keyset = fmt.Sprintf(" AND ev.seq < $%d", len(args))
	}
	query := `
		SELECT ev.id::text, ev.entity_id::text, ev.event_type, ev.dedup_key,
		       COALESCE(ev.metadata_json::text, ''), ev.occurred_at, ev.created_at, ev.seq` +
		from + keyset + `
		ORDER BY ev.seq DESC`
	if opts.Limit > 0 {
		args = append(args, opts.Limit)
		query += fmt.Sprintf(` LIMIT $%d`, len(args))
	}
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []db.ListedEvent{}
	for rows.Next() {
		var item db.ListedEvent
		var entityID string
		var occurredAt sql.NullTime
		if err := rows.Scan(&item.Event.ID, &entityID, &item.Event.EventType, &item.Event.DedupKey,
			&item.Event.MetadataJSON, &occurredAt, &item.Event.CreatedAt, &item.Position); err != nil {
			return nil, 0, err
		}
		item.Event.OrgID = orgID
		item.Event.EntityID = &entityID
		if occurredAt.Valid {
			item.Event.OccurredAt = occurredAt.Time.UTC()
		}
		item.Event.CreatedAt = item.Event.CreatedAt.UTC()
		out = append(out, item)
	}
	return out, total, rows.Err()
}
