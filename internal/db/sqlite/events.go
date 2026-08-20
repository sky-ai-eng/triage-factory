package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// eventStore is the SQLite impl of db.EventStore.
// Behavioral notes vs the pre-store internal/db/events.go body:
//
//   - assertLocalOrg(orgID) at every entry point.
//   - created_at is bound from time.Now() (ns resolution) rather
//     than the schema's CURRENT_TIMESTAMP default (second
//     resolution) so same-tx writes order by insertion order.
//
// The constructor takes two queryers for signature parity with the
// Postgres impl, but both collapse onto the same queryer.
type eventStore struct {
	q     queryer
	admin queryer
}

func newEventStore(q, admin queryer) db.EventStore {
	return &eventStore{q: q, admin: admin}
}

var _ db.EventStore = (*eventStore)(nil)

func (s *eventStore) Record(ctx context.Context, orgID string, evt domain.Event) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	return recordEvent(ctx, s.q, evt)
}

func (s *eventStore) RecordSystem(ctx context.Context, orgID string, evt domain.Event) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	return recordEvent(ctx, s.admin, evt)
}

func recordEvent(ctx context.Context, q queryer, evt domain.Event) (string, error) {
	id := evt.ID
	if id == "" {
		id = uuid.New().String()
	}
	// occurred_at is nullable — leave it unset when the caller
	// doesn't have a source timestamp. Factory and other
	// chronological queries COALESCE(occurred_at, created_at).
	var occurredAt any
	if !evt.OccurredAt.IsZero() {
		// UTC at the bind, not just at the callers: occurred_at unions with
		// created_at in the ORDER BY of the events feed and the factory belt,
		// so one local-zone instant misplaces a row against every other.
		occurredAt = evt.OccurredAt.UTC()
	}
	var entityID any
	if evt.EntityID != nil && *evt.EntityID != "" {
		entityID = *evt.EntityID
	}
	// Bind created_at from Go rather than CURRENT_TIMESTAMP so the
	// LIMIT-1 ordering in LatestForEntityTypeAndDedupKey gets
	// ns resolution across same-tx writes. The schema's
	// DEFAULT CURRENT_TIMESTAMP is second-resolution and would tie
	// for any burst-rate inserts (poller diff fanout, carry-over
	// loops), falling through to a rowid-DESC tiebreak that doesn't
	// always agree with insertion order under WAL.
	createdAt := time.Now().UTC()
	if _, err := q.ExecContext(ctx, `
		INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json, occurred_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, id, entityID, evt.EventType, evt.DedupKey, evt.MetadataJSON, occurredAt, createdAt); err != nil {
		return "", err
	}
	return id, nil
}

func (s *eventStore) LatestForEntityTypeAndDedupKey(ctx context.Context, orgID, entityID, eventType, dedupKey string) (*domain.Event, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// Same ordering rule as the Postgres impl — primary sort on
	// COALESCE(occurred_at, created_at) so the audit row picked
	// reflects underlying event time when known, falling back to
	// insertion order otherwise. rowid DESC is the stable
	// final-tiebreaker since the TEXT PK doesn't sort meaningfully.
	row := s.q.QueryRowContext(ctx, `
		SELECT id, entity_id, event_type, dedup_key,
		       COALESCE(metadata_json, ''), occurred_at, created_at
		FROM events
		WHERE entity_id = ? AND event_type = ? AND dedup_key = ?
		ORDER BY COALESCE(occurred_at, created_at) DESC, created_at DESC, rowid DESC
		LIMIT 1
	`, entityID, eventType, dedupKey)
	var evt domain.Event
	var entID sql.NullString
	var occurredAt sql.NullTime
	if err := row.Scan(&evt.ID, &entID, &evt.EventType, &evt.DedupKey,
		&evt.MetadataJSON, &occurredAt, &evt.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if entID.Valid {
		s := entID.String
		evt.EntityID = &s
	}
	if occurredAt.Valid {
		evt.OccurredAt = occurredAt.Time
	}
	return &evt, nil
}

func (s *eventStore) GetMetadataSystem(ctx context.Context, orgID, eventID string) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	var metadata sql.NullString
	err := s.admin.QueryRowContext(ctx, `SELECT metadata_json FROM events WHERE id = ?`, eventID).Scan(&metadata)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return metadata.String, nil
}

func (s *eventStore) GetSystem(ctx context.Context, orgID, eventID string) (*domain.Event, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.admin.QueryRowContext(ctx, `
		SELECT id, entity_id, event_type, dedup_key,
		       COALESCE(metadata_json, ''), occurred_at, created_at
		FROM events WHERE id = ?
	`, eventID)
	var (
		evt        domain.Event
		entID      sql.NullString
		occurredAt sql.NullTime
	)
	if err := row.Scan(&evt.ID, &entID, &evt.EventType, &evt.DedupKey,
		&evt.MetadataJSON, &occurredAt, &evt.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if entID.Valid {
		s := entID.String
		evt.EntityID = &s
	}
	if occurredAt.Valid {
		evt.OccurredAt = occurredAt.Time
	}
	// Stamp the tenant context the worker needs for routing. SQLite is
	// single-tenant so orgID is always the local sentinel.
	evt.OrgID = orgID
	return &evt, nil
}
