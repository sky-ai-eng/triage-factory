package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// entityStore is the SQLite impl of db.EntityStore. SQL bodies are
// ported from the pre-D2 internal/db/entities.go; the only behavioral
// change is the orgID assertion at each method entry. SQLite tables
// have no org_id column — local mode is single-tenant by construction.
//
// The constructor takes two queryers for signature parity with the
// Postgres impl, but SQLite has only one connection — both
// arguments collapse onto the same queryer. The `...System` admin-
// pool variants therefore run identically to the non-System variants
// here; the pool distinction is purely a multi-mode concept.
type entityStore struct{ q queryer }

func newEntityStore(q, _ queryer) db.EntityStore { return &entityStore{q: q} }

var _ db.EntityStore = (*entityStore)(nil)

// entityIDInChunkSize caps the number of `?` placeholders per batched
// WHERE id IN (...) query so we stay well under SQLite's default
// SQLITE_LIMIT_VARIABLE_NUMBER (999). Chunking runs multiple
// round-trips but keeps the query schema compatible with the default
// build — the scorer's entity set can easily exceed 1k tasks on
// large repos.
const entityIDInChunkSize = 500

const entitySelectCols = `id, source, source_id, kind, COALESCE(title, ''), COALESCE(url, ''),
       COALESCE(snapshot_json, ''), COALESCE(description, ''), state,
       created_at, last_polled_at, closed_at, poll_seq`

// --- Lookup ---

func (s *entityStore) Get(ctx context.Context, orgID, id string) (*domain.Entity, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `SELECT `+entitySelectCols+` FROM entities WHERE id = ?`, id)
	return scanEntityRow(row)
}

func (s *entityStore) GetBySource(ctx context.Context, orgID, source, sourceID string) (*domain.Entity, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `SELECT `+entitySelectCols+` FROM entities WHERE source = ? AND source_id = ?`, source, sourceID)
	return scanEntityRow(row)
}

func (s *entityStore) Descriptions(ctx context.Context, orgID string, ids []string) (map[string]string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}

	for start := 0; start < len(unique); start += entityIDInChunkSize {
		end := start + entityIDInChunkSize
		if end > len(unique) {
			end = len(unique)
		}
		chunk := unique[start:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, id := range chunk {
			placeholders[i] = "?"
			args[i] = id
		}
		query := `SELECT id, COALESCE(description, '') FROM entities WHERE id IN (` +
			strings.Join(placeholders, ",") + `)`
		rows, err := s.q.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, desc string
			if err := rows.Scan(&id, &desc); err != nil {
				rows.Close()
				return nil, err
			}
			if desc != "" {
				out[id] = desc
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	return out, nil
}

func (s *entityStore) ListActive(ctx context.Context, orgID, source string) ([]domain.Entity, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+entitySelectCols+`
		FROM entities
		WHERE source = ? AND state = 'active'
		ORDER BY last_polled_at ASC
	`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.Entity{}
	for rows.Next() {
		e, err := scanEntityFromRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// ListActiveJiraTeamScoped returns the full active Jira set in local
// mode — N=1, so there is no other team to scope away. The Postgres
// impl applies the jira_project_status_rules team semi-join; SQLite
// deliberately does not (see the interface doc on EntityStore for the
// asymmetry rationale, which mirrors FactoryReadStore.Entities).
func (s *entityStore) ListActiveJiraTeamScoped(ctx context.Context, orgID, _ string) ([]domain.Entity, error) {
	// teamID ignored: N=1 local mode has one team, so the deck's team
	// filter has nothing to narrow (the frontend never renders it below
	// 2 teams). Mirrors the FactoryReadStore.Entities asymmetry.
	return s.ListActive(ctx, orgID, "jira")
}

// --- Mutation ---

func (s *entityStore) FindOrCreate(ctx context.Context, orgID, source, sourceID, kind, title, url string) (*domain.Entity, bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, false, err
	}
	existing, err := s.GetBySource(ctx, orgID, source, sourceID)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		return existing, false, nil
	}

	id := uuid.New().String()
	now := time.Now().UTC()
	_, err = s.q.ExecContext(ctx, `
		INSERT INTO entities (id, source, source_id, kind, title, url, state, created_at, last_polled_at)
		VALUES (?, ?, ?, ?, ?, ?, 'active', ?, ?)
	`, id, source, sourceID, kind, title, url, now, now)
	if err != nil {
		// Concurrent first-discovery: another goroutine inserted
		// between our SELECT and INSERT. Re-read so both callers
		// see a populated entity.
		existing, err2 := s.GetBySource(ctx, orgID, source, sourceID)
		if err2 == nil && existing != nil {
			return existing, false, nil
		}
		return nil, false, err
	}

	return &domain.Entity{
		ID:           id,
		Source:       source,
		SourceID:     sourceID,
		Kind:         kind,
		Title:        title,
		URL:          url,
		State:        "active",
		CreatedAt:    now,
		LastPolledAt: &now,
	}, true, nil
}

// scanWrittenEntity decodes an id-keyed UPDATE … RETURNING. scanEntityRow maps
// a scanned-nothing to (nil, nil) — the read convention — so the write layer
// turns that back into sql.ErrNoRows, this store's miss sentinel for an id a
// caller resolved and then wrote against.
func scanWrittenEntity(row *sql.Row) (domain.Entity, error) {
	e, err := scanEntityRow(row)
	if err != nil {
		return domain.Entity{}, err
	}
	if e == nil {
		return domain.Entity{}, sql.ErrNoRows
	}
	return *e, nil
}

func (s *entityStore) UpdateSnapshot(ctx context.Context, orgID, id, snapshotJSON string) (domain.Entity, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.Entity{}, err
	}
	return scanWrittenEntity(s.q.QueryRowContext(ctx, `
		UPDATE entities SET snapshot_json = ?, last_polled_at = ? WHERE id = ?
		RETURNING `+entitySelectCols,
		snapshotJSON, time.Now().UTC(), id))
}

func (s *entityStore) PatchSnapshot(ctx context.Context, orgID, id, snapshotJSON string) (domain.Entity, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.Entity{}, err
	}
	return scanWrittenEntity(s.q.QueryRowContext(ctx,
		`UPDATE entities SET snapshot_json = ? WHERE id = ? RETURNING `+entitySelectCols,
		snapshotJSON, id))
}

func (s *entityStore) UpdateTitle(ctx context.Context, orgID, id, title string) (domain.Entity, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.Entity{}, err
	}
	return scanWrittenEntity(s.q.QueryRowContext(ctx,
		`UPDATE entities SET title = ? WHERE id = ? RETURNING `+entitySelectCols, title, id))
}

func (s *entityStore) UpdateDescription(ctx context.Context, orgID, id, description string) (domain.Entity, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.Entity{}, err
	}
	return scanWrittenEntity(s.q.QueryRowContext(ctx,
		`UPDATE entities SET description = ? WHERE id = ? RETURNING `+entitySelectCols, description, id))
}

func (s *entityStore) MarkClosed(ctx context.Context, orgID, id string) (domain.Entity, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.Entity{}, err
	}
	return scanWrittenEntity(s.q.QueryRowContext(ctx, `
		UPDATE entities SET state = 'closed', closed_at = ? WHERE id = ?
		RETURNING `+entitySelectCols,
		time.Now().UTC(), id))
}

// Close returns nil when the state='active' guard declined — the entity was
// already closed, or no row carries the id. Both are the idempotent no-op the
// method exists for, and neither is an error.
func (s *entityStore) Close(ctx context.Context, orgID, id string) (*domain.Entity, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	return scanEntityRow(s.q.QueryRowContext(ctx, `
		UPDATE entities SET state = 'closed', closed_at = ? WHERE id = ? AND state = 'active'
		RETURNING `+entitySelectCols,
		time.Now().UTC(), id))
}

func (s *entityStore) Reactivate(ctx context.Context, orgID, id string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE entities SET state = 'active', closed_at = NULL WHERE id = ? AND state = 'closed'
	`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// --- Admin-pool (`...System`) variants ---
//
// The `...System` methods on these stores let multi-mode
// consumers without JWT-claims context route through the admin
// pool. SQLite has one connection and no auth concept, so each System
// variant just delegates to its non-System counterpart — preserving
// the same assertLocalOrg gate and behavior.

func (s *entityStore) GetSystem(ctx context.Context, orgID, id string) (*domain.Entity, error) {
	return s.Get(ctx, orgID, id)
}

func (s *entityStore) GetBySourceSystem(ctx context.Context, orgID, source, sourceID string) (*domain.Entity, error) {
	return s.GetBySource(ctx, orgID, source, sourceID)
}

func (s *entityStore) ListActiveSystem(ctx context.Context, orgID, source string) ([]domain.Entity, error) {
	return s.ListActive(ctx, orgID, source)
}

// ListActiveTerminalCandidatesSystem selects active entities whose stored
// snapshot already reads terminal. json_valid guards the extraction so a
// legacy ” / malformed snapshot is skipped rather than failing the scan —
// an entity that never stored a snapshot has said nothing about whether its
// subject finished. json_extract returns 1 for a JSON true, hence the
// integer comparison on $.merged.
func (s *entityStore) ListActiveTerminalCandidatesSystem(ctx context.Context, orgID string, jiraDone []domain.JiraStatusRef, limit int) ([]domain.Entity, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	args := []any{}
	// Each arm is omitted rather than emitted empty: `IN ()` is a syntax
	// error, and a ref set with no ids (or no names) genuinely has nothing to
	// match on that side. With neither, no Jira entity can be terminal.
	arm := func(path string, values []string) string {
		placeholders := make([]string, len(values))
		for i, v := range values {
			placeholders[i] = "?"
			args = append(args, v)
		}
		return `json_extract(snapshot_json, '` + path + `') IN (` + strings.Join(placeholders, ", ") + `)`
	}
	var matches []string
	if ids := domain.JiraStatusIDs(jiraDone); len(ids) > 0 {
		matches = append(matches, arm("$.status_id", ids))
	}
	if names := domain.JiraStatusNames(jiraDone); len(names) > 0 {
		matches = append(matches, arm("$.status", names))
	}
	jiraArm := ""
	if len(matches) > 0 {
		jiraArm = ` OR (source = 'jira' AND (` + strings.Join(matches, " OR ") + `))`
	}
	limitClause := ""
	if limit > 0 {
		limitClause = " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+entitySelectCols+`
		FROM entities
		WHERE state = 'active'
		  AND json_valid(NULLIF(snapshot_json, ''))
		  AND (
		    (source = 'github' AND (
		       json_extract(snapshot_json, '$.merged') = 1
		       OR upper(COALESCE(json_extract(snapshot_json, '$.state'), '')) IN ('CLOSED', 'MERGED')
		    ))`+jiraArm+`
		  )
		ORDER BY created_at ASC`+limitClause, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.Entity{}
	for rows.Next() {
		e, err := scanEntityFromRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

func (s *entityStore) FindOrCreateSystem(ctx context.Context, orgID, source, sourceID, kind, title, url string) (*domain.Entity, bool, error) {
	return s.FindOrCreate(ctx, orgID, source, sourceID, kind, title, url)
}

// UpdateSnapshotCASSystem mirrors the Postgres impl's poll_seq CAS
// (TFAC-579) for interface conformance — real CAS semantics even though
// SQLite/local is single-connection N=1 and has no concurrent-leader race
// for it to guard against. There is no blind-write system variant: every
// tracker snapshot write goes through this CAS.
func (s *entityStore) UpdateSnapshotCASSystem(ctx context.Context, orgID, id, snapshotJSON string, expectedPollSeq int64) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	return updateSnapshotCAS(ctx, s.q, id, snapshotJSON, expectedPollSeq)
}

// updateSnapshotCAS is the CAS statement itself, shared with the event
// queue's EnqueueBatchWithSnapshotCAS — which runs it against its own
// transaction so the snapshot advance and the transitions diffed against
// it commit together. One copy of the SQL so the two callers cannot drift
// on what "the CAS" means. The caller asserts the local org.
func updateSnapshotCAS(ctx context.Context, q queryer, id, snapshotJSON string, expectedPollSeq int64) (bool, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE entities
		SET snapshot_json = ?, last_polled_at = ?, poll_seq = poll_seq + 1
		WHERE id = ? AND poll_seq = ?
	`, snapshotJSON, time.Now().UTC(), id, expectedPollSeq)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MarkPolledSystem stamps last_polled_at alone — no snapshot, no poll_seq
// bump. See the interface doc: it records that the row was read from the
// source without anything having been diffed off it.
// MarkPolledSystem is exempt from the returned-row rule; see the interface doc.
func (s *entityStore) MarkPolledSystem(ctx context.Context, orgID, id string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx,
		`UPDATE entities SET last_polled_at = ? WHERE id = ?`, time.Now().UTC(), id)
	return err
}

func (s *entityStore) RekeyOrMergeSystem(ctx context.Context, orgID, id, newSourceID string) (string, bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", false, err
	}
	var survivor string
	merged := false
	err := inTx(ctx, s.q, func(q queryer) error {
		if err := q.QueryRowContext(ctx, `SELECT id FROM entities WHERE source = (SELECT source FROM entities WHERE id = ?) AND source_id = ?`, id, newSourceID).Scan(&survivor); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			res, err := q.ExecContext(ctx, `UPDATE entities SET source_id = ?, last_polled_at = ? WHERE id = ?`, newSourceID, time.Now().UTC(), id)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return sql.ErrNoRows
			}
			survivor = id
			return nil
		}
		if survivor == id {
			return nil
		}
		merged = true
		// Dismiss active tasks whose dedup slot is already occupied on the survivor.
		if _, err := q.ExecContext(ctx, `UPDATE tasks SET status='dismissed', closed_at=?, close_reason='duplicate_entity_merged' WHERE entity_id=? AND status NOT IN ('done','dismissed') AND EXISTS (SELECT 1 FROM tasks s WHERE s.entity_id=? AND s.event_type=tasks.event_type AND s.dedup_key=tasks.dedup_key AND s.status NOT IN ('done','dismissed'))`, time.Now().UTC(), id, survivor); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `UPDATE blueprint_runs SET cancel_requested=1 WHERE status='running' AND cancel_requested=0 AND id IN (SELECT c.blueprint_run_id FROM conversations c JOIN tasks t ON t.id=c.task_id WHERE t.entity_id=? AND t.close_reason='duplicate_entity_merged' AND c.blueprint_run_id IS NOT NULL AND (c.status IS NULL OR c.status NOT IN ('completed','failed')))`, id); err != nil {
			return err
		}
		for _, stmt := range []string{
			`UPDATE tasks SET entity_id=? WHERE entity_id=?`, `UPDATE events SET entity_id=? WHERE entity_id=?`, `UPDATE event_queue SET entity_id=? WHERE entity_id=?`, `UPDATE pending_firings SET entity_id=? WHERE entity_id=?`, `UPDATE conversation_memory SET entity_id=? WHERE entity_id=?`,
		} {
			if _, err := q.ExecContext(ctx, stmt, survivor, id); err != nil {
				return err
			}
		}
		if _, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO conversation_memory_entities(org_id,conversation_id,entity_id,role,created_at) SELECT org_id,conversation_id,?,role,created_at FROM conversation_memory_entities WHERE entity_id=?`, survivor, id); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `DELETE FROM conversation_memory_entities WHERE entity_id=?`, id); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `DELETE FROM entity_links WHERE (from_entity_id=? OR to_entity_id=?) AND (CASE WHEN from_entity_id=? THEN ? ELSE from_entity_id END)=(CASE WHEN to_entity_id=? THEN ? ELSE to_entity_id END)`, id, id, id, survivor, id, survivor); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO entity_links(from_entity_id,to_entity_id,kind,origin,created_at,org_id) SELECT CASE WHEN from_entity_id=? THEN ? ELSE from_entity_id END,CASE WHEN to_entity_id=? THEN ? ELSE to_entity_id END,kind,origin,created_at,org_id FROM entity_links WHERE from_entity_id=? OR to_entity_id=?`, id, survivor, id, survivor, id, id); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `DELETE FROM entity_links WHERE from_entity_id=? OR to_entity_id=?`, id, id); err != nil {
			return err
		}
		_, err := q.ExecContext(ctx, `DELETE FROM entities WHERE id=?`, id)
		return err
	})
	return survivor, merged, err
}

func (s *entityStore) UpdateTitleSystem(ctx context.Context, orgID, id, title string) (domain.Entity, error) {
	return s.UpdateTitle(ctx, orgID, id, title)
}

func (s *entityStore) UpdateDescriptionSystem(ctx context.Context, orgID, id, description string) (domain.Entity, error) {
	return s.UpdateDescription(ctx, orgID, id, description)
}

// UpdateURLSystem sets the entity's url. No non-System counterpart exists
// (see the interface doc) — SQLite has one connection and no auth concept,
// so this is inlined here rather than delegating to a shared non-System
// method the way the other `...System` variants in this file do.
func (s *entityStore) UpdateURLSystem(ctx context.Context, orgID, id, url string) (domain.Entity, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.Entity{}, err
	}
	return scanWrittenEntity(s.q.QueryRowContext(ctx,
		`UPDATE entities SET url = ? WHERE id = ? RETURNING `+entitySelectCols, url, id))
}

func (s *entityStore) MarkClosedSystem(ctx context.Context, orgID, id string) (domain.Entity, error) {
	return s.MarkClosed(ctx, orgID, id)
}

// CloseSystem mirrors Close (active→closed transition). The router's
// entity-lifecycle path consumes this through the admin pool in
// Postgres; SQLite collapses to the non-System variant.
func (s *entityStore) CloseSystem(ctx context.Context, orgID, id string) (*domain.Entity, error) {
	return s.Close(ctx, orgID, id)
}

func (s *entityStore) ReactivateSystem(ctx context.Context, orgID, id string) (bool, error) {
	return s.Reactivate(ctx, orgID, id)
}

func (s *entityStore) DescriptionsSystem(ctx context.Context, orgID string, ids []string) (map[string]string, error) {
	return s.Descriptions(ctx, orgID, ids)
}

func (s *entityStore) OwningTeamForEntitySystem(ctx context.Context, orgID, entityID string) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	// The owning_team_id override, or "" when unset — the NullString scans
	// that case to "" so the router falls through to its later tiers.
	var team sql.NullString
	err := s.q.QueryRowContext(ctx, `
		SELECT owning_team_id
		FROM entities
		WHERE id = ?
	`, entityID).Scan(&team)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return team.String, nil
}

// StampOwningTeamIfUnsetSystem fills owning_team_id only where it is still
// NULL. The IS NULL predicate is the concurrency story as much as the
// semantic one: it lives in the UPDATE rather than in a read-then-write, so
// two callers racing on the same entity resolve to whichever committed first
// with no lost update, and RowsAffected reports which one that was.
//
// N=1 here, so the race is theoretical — but the statement is the same one
// Postgres runs, and a stamp that meant "if unset" on one dialect and "last
// writer wins" on the other would make the conformance suite the only place
// the difference showed up.
func (s *entityStore) StampOwningTeamIfUnsetSystem(ctx context.Context, orgID, entityID, teamID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	if teamID == "" {
		return false, nil
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE entities
		SET owning_team_id = ?
		WHERE id = ? AND owning_team_id IS NULL
	`, teamID, entityID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// scanEntityRow / scanEntityFromRows return a fresh domain.Entity per
// invocation. The two flavors mirror database/sql's *Row vs *Rows
// types since Scan signatures aren't unifiable through a common
// interface in the standard library.
func scanEntityRow(row *sql.Row) (*domain.Entity, error) {
	var e domain.Entity
	err := row.Scan(&e.ID, &e.Source, &e.SourceID, &e.Kind, &e.Title, &e.URL,
		&e.SnapshotJSON, &e.Description, &e.State,
		&e.CreatedAt, &e.LastPolledAt, &e.ClosedAt, &e.PollSeq)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func scanEntityFromRows(rows *sql.Rows) (*domain.Entity, error) {
	var e domain.Entity
	if err := rows.Scan(&e.ID, &e.Source, &e.SourceID, &e.Kind, &e.Title, &e.URL,
		&e.SnapshotJSON, &e.Description, &e.State,
		&e.CreatedAt, &e.LastPolledAt, &e.ClosedAt, &e.PollSeq); err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *entityStore) ClearSnapshotsForSourceSystem(ctx context.Context, orgID, source string) (int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE entities
		SET snapshot_json = NULL, poll_seq = poll_seq + 1
		WHERE source = ? AND state = 'active'
	`, source)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}
