package sqlite

import (
	"context"
	"database/sql"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// sqliteListedEntityCols is entitySelectCols over the alias e, followed by the
// latest event's type and detection time. rowid breaks a created_at tie: the
// TEXT id does not sort in insertion order.
const sqliteListedEntityCols = `e.id, e.source, e.scope, e.source_id, COALESCE(e.external_id, ''), e.kind,
       COALESCE(e.title, ''), COALESCE(e.url, ''),
       COALESCE(e.snapshot_json, ''), COALESCE(e.description, ''), e.state,
       e.created_at, e.last_polled_at, e.closed_at, e.poll_seq, COALESCE(e.linear_team_id, ''),
       COALESCE(e.repository_id, ''), COALESCE(e.jira_project_id, ''),
       (SELECT ev.event_type FROM events ev WHERE ev.entity_id = e.id ORDER BY ev.created_at DESC, ev.rowid DESC LIMIT 1),
       (SELECT ev.created_at FROM events ev WHERE ev.entity_id = e.id ORDER BY ev.created_at DESC, ev.rowid DESC LIMIT 1)`

// sqliteEntityListWhere renders db.EntityListFilter as a WHERE body over the
// alias e plus its args. TeamIDs is not read: this dialect is N=1, and its
// one team's tracked set is everything the local tracker discovered.
func sqliteEntityListWhere(f db.EntityListFilter) (string, []any) {
	clauses := []string{"1=1"}
	var args []any
	in := func(column string, values []string) {
		placeholders, a := inListArgs(values)
		clauses = append(clauses, column+" IN ("+placeholders+")")
		args = append(args, a...)
	}
	if len(f.IDs) > 0 {
		in("e.id", f.IDs)
	}
	if len(f.Sources) > 0 {
		in("e.source", f.Sources)
	}
	if len(f.Kinds) > 0 {
		in("e.kind", f.Kinds)
	}
	if len(f.States) > 0 {
		in("e.state", f.States)
	}
	if f.ClosedSince != nil {
		// julianday() on both sides, so the stored text and the bound instant
		// compare as instants whatever width each was written at.
		clauses = append(clauses, "(e.closed_at IS NULL OR julianday(e.closed_at) >= julianday(?))")
		args = append(args, f.ClosedSince.UTC())
	}
	return strings.Join(clauses, " AND "), args
}

func (s *entityStore) List(ctx context.Context, orgID string, f db.EntityListFilter, opts db.ListOpts) ([]domain.ListedEntity, int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	where, args := sqliteEntityListWhere(f)
	var total int
	if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM entities e WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []domain.ListedEntity{}, total, nil
	}
	query := `
		SELECT ` + sqliteListedEntityCols + `
		FROM entities e
		WHERE ` + where + `
		ORDER BY e.created_at DESC, e.id DESC`
	if opts.Limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, opts.Limit, opts.Offset)
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
		var latestAt subselectDatetime
		if err := rows.Scan(&e.ID, &e.Source, &e.Scope, &e.SourceID, &e.ExternalID, &e.Kind, &e.Title, &e.URL,
			&e.SnapshotJSON, &e.Description, &e.State,
			&e.CreatedAt, &e.LastPolledAt, &e.ClosedAt, &e.PollSeq, &e.LinearTeamID, &e.RepositoryID, &e.JiraProjectID,
			&latestType, &latestAt); err != nil {
			return nil, 0, err
		}
		item.LatestEventType = latestType.String
		if latestAt.Valid {
			at := latestAt.Time
			item.LatestEventAt = &at
		}
		out = append(out, item)
	}
	return out, total, rows.Err()
}

// sqliteEventListWhere renders db.EventListFilter as a WHERE body over the
// event alias ev plus its args. An event with no entity is never listed.
func sqliteEventListWhere(f db.EventListFilter) (string, []any) {
	clauses := []string{"ev.entity_id IS NOT NULL"}
	var args []any
	if len(f.EntityIDs) > 0 {
		placeholders, a := inListArgs(f.EntityIDs)
		clauses = append(clauses, "ev.entity_id IN ("+placeholders+")")
		args = append(args, a...)
	}
	if f.Since != nil {
		clauses = append(clauses, "julianday(ev.created_at) >= julianday(?)")
		args = append(args, f.Since.UTC())
	}
	return strings.Join(clauses, " AND "), args
}

func (s *eventStore) List(ctx context.Context, orgID string, f db.EventListFilter, opts db.ListOpts) ([]db.ListedEvent, int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	where, args := sqliteEventListWhere(f)
	var total int
	if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM events ev WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []db.ListedEvent{}, total, nil
	}
	if len(opts.After) > 0 {
		after, err := db.EventListCursor(opts.After)
		if err != nil {
			return nil, 0, err
		}
		where += " AND ev.rowid < ?"
		args = append(args, after)
	}
	query := `
		SELECT ev.id, ev.entity_id, ev.event_type, ev.dedup_key,
		       COALESCE(ev.metadata_json, ''), ev.occurred_at, ev.created_at, ev.rowid
		FROM events ev
		WHERE ` + where + `
		ORDER BY ev.rowid DESC`
	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
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
