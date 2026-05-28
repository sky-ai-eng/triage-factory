package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// factoryReadStore is the SQLite impl of db.FactoryReadStore. SQL
// bodies are moved verbatim from the pre-D2 internal/db/factory.go;
// the only behavioral change is the orgID assertion at each method
// entry. SQLite is single-tenant so the pool-distinction Postgres
// makes (admin vs app) collapses to one connection.
type factoryReadStore struct{ q queryer }

func newFactoryReadStore(q queryer) db.FactoryReadStore { return &factoryReadStore{q: q} }

var _ db.FactoryReadStore = (*factoryReadStore)(nil)

// sqliteFactoryActiveRunStatuses is the set of run.status values we
// treat as "in flight" for the factory view. Matches the X-button
// window in AgentCard — every state before a terminal transition
// (completed | failed | cancelled | task_unsolvable). pending_approval
// counts as active: the run is paused waiting for user input, not done.
var sqliteFactoryActiveRunStatuses = []string{
	"initializing",
	"cloning",
	"fetching",
	"worktree_created",
	"agent_starting",
	"running",
	"pending_approval",
}

func (s *factoryReadStore) EventCountsSince(ctx context.Context, orgID string, since time.Time) (map[string]int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT event_type, COUNT(*)
		FROM events
		WHERE created_at > ?
		GROUP BY event_type
	`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var eventType string
		var count int
		if err := rows.Scan(&eventType, &count); err != nil {
			return nil, err
		}
		out[eventType] = count
	}
	return out, rows.Err()
}

func (s *factoryReadStore) LifetimeDistinctByEventType(ctx context.Context, orgID string) (map[string]int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// Covered by the partial index `idx_events_type_entity (event_type,
	// entity_id) WHERE entity_id IS NOT NULL`: SQLite walks the index
	// in (event_type, entity_id) order, so each event_type group's
	// distinct entity_ids are contiguous and DISTINCT collapses to a
	// per-group adjacency dedupe — no temp B-tree, no table touch.
	rows, err := s.q.QueryContext(ctx, `
		SELECT event_type, COUNT(DISTINCT entity_id)
		FROM events
		WHERE entity_id IS NOT NULL
		GROUP BY event_type
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var eventType string
		var count int
		if err := rows.Scan(&eventType, &count); err != nil {
			return nil, err
		}
		out[eventType] = count
	}
	return out, rows.Err()
}

func (s *factoryReadStore) TaskCountsSince(ctx context.Context, orgID string, since time.Time) (map[string]int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT event_type, COUNT(*)
		FROM tasks
		WHERE created_at > ?
		GROUP BY event_type
	`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var eventType string
		var count int
		if err := rows.Scan(&eventType, &count); err != nil {
			return nil, err
		}
		out[eventType] = count
	}
	return out, rows.Err()
}

func (s *factoryReadStore) ActiveRuns(ctx context.Context, orgID string) ([]domain.FactoryActiveRun, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	placeholders := "?"
	args := make([]any, 0, len(sqliteFactoryActiveRunStatuses))
	args = append(args, sqliteFactoryActiveRunStatuses[0])
	for i := 1; i < len(sqliteFactoryActiveRunStatuses); i++ {
		placeholders += ", ?"
		args = append(args, sqliteFactoryActiveRunStatuses[i])
	}

	// memory_missing is derived from a LEFT JOIN to run_memory rather
	// than read off a column on runs (SKY-204): "the agent has not
	// produced its memory file" === "no run_memory row exists, OR the
	// row's agent_content is NULL/whitespace." NULLIF(TRIM(...), '')
	// collapses both empty strings (legacy carry-over from before
	// SKY-204 normalized them) and whitespace-only writes onto the
	// same NULL signal, so a single condition covers all three forms
	// of noncompliance.
	//
	// The denormalized memory_missing column this replaces drifted
	// from ground truth whenever a memory row was written outside the
	// spawner's gate; the JOIN keeps the projection honest by
	// construction.
	query := `
		SELECT
			r.id, r.task_id, COALESCE(r.prompt_id, ''),
			r.status, COALESCE(r.model, ''), r.started_at, r.completed_at,
			r.total_cost_usd, r.duration_ms, r.num_turns,
			COALESCE(r.stop_reason, ''), COALESCE(r.worktree_path, ''),
			COALESCE(r.result_summary, ''), COALESCE(r.session_id, ''),
			(NULLIF(TRIM(rm.agent_content, ' ' || char(9) || char(10) || char(13)), '') IS NULL) AS memory_missing,
			r.trigger_type, COALESCE(r.trigger_id, ''),
			COALESCE(r.actor_agent_id, ''),
			` + sqliteTaskColumnsWithEntity + `
		FROM runs r
		LEFT JOIN run_memory rm ON rm.run_id = r.id
		JOIN tasks t ON r.task_id = t.id
		JOIN entities e ON t.entity_id = e.id
		WHERE r.status IN (` + placeholders + `)
		ORDER BY r.started_at DESC
	`

	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.FactoryActiveRun
	for rows.Next() {
		var r domain.AgentRun
		var t domain.Task
		var completedAt sql.NullTime
		var costUSD sql.NullFloat64
		var durationMs, numTurns sql.NullInt64

		runTargets := []any{
			&r.ID, &r.TaskID, &r.PromptID,
			&r.Status, &r.Model, &r.StartedAt, &completedAt,
			&costUSD, &durationMs, &numTurns,
			&r.StopReason, &r.WorktreePath,
			&r.ResultSummary, &r.SessionID,
			&r.MemoryMissing, &r.TriggerType, &r.TriggerID,
			&r.ActorAgentID,
		}
		var ts taskScanState
		if err := rows.Scan(append(runTargets, ts.targets(&t)...)...); err != nil {
			return nil, err
		}
		ts.finalize(&t)
		if completedAt.Valid {
			r.CompletedAt = &completedAt.Time
		}
		if costUSD.Valid {
			r.TotalCostUSD = &costUSD.Float64
		}
		if durationMs.Valid {
			v := int(durationMs.Int64)
			r.DurationMs = &v
		}
		if numTurns.Valid {
			v := int(numTurns.Int64)
			r.NumTurns = &v
		}
		out = append(out, domain.FactoryActiveRun{Run: r, Task: t, EntityEventTyp: t.EventType})
	}
	return out, rows.Err()
}

func (s *factoryReadStore) RecentEventsByEntity(ctx context.Context, orgID string, entityIDs []string, perEntity int) (map[string][]domain.FactoryRecentEvent, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	out := map[string][]domain.FactoryRecentEvent{}
	if len(entityIDs) == 0 || perEntity <= 0 {
		return out, nil
	}
	// Chunk to respect SQLite's SQLITE_LIMIT_VARIABLE_NUMBER. The
	// factory caller bounds entityIDs by factoryEntityLimit=500 today
	// so we never hit it, but the guard is cheap.
	const chunkSize = 500
	for start := 0; start < len(entityIDs); start += chunkSize {
		end := start + chunkSize
		if end > len(entityIDs) {
			end = len(entityIDs)
		}
		chunk := entityIDs[start:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		args = append(args, perEntity)

		// Chain ordering is driven by source time (commit committed_at,
		// check-run completed_at, review submitted_at) when available —
		// detection time (created_at) as a fallback. rowid is a
		// tie-breaker for genuine collisions (two events sharing a
		// source timestamp to the second, or both lacking a source time
		// and inserted in the same poll cycle). Inner window partitions
		// on COALESCE so the "most recent" per entity is stable
		// regardless of which column is populated.
		query := `
			SELECT entity_id, event_type, event_at, detected_at
			FROM (
				SELECT entity_id, event_type,
					COALESCE(occurred_at, created_at) AS event_at,
					created_at AS detected_at,
					rowid AS row_id,
					ROW_NUMBER() OVER (
						PARTITION BY entity_id
						ORDER BY COALESCE(occurred_at, created_at) DESC, rowid DESC
					) AS rn
				FROM events
				WHERE entity_id IN (` + strings.Join(placeholders, ",") + `)
			)
			WHERE rn <= ?
			ORDER BY entity_id, event_at ASC, row_id ASC
		`
		rows, err := s.q.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var entityID, eventType, eventAtStr, detectedAtStr string
			// COALESCE over two DATETIME columns loses the column-type
			// metadata the SQLite driver needs to scan directly into
			// time.Time (the driver returns the value as a string in
			// that case). Scan as string and parse ourselves — cheap
			// and consistent across whichever source column contributed
			// the value. detected_at is plain `created_at` but we scan
			// it as a string too for symmetry with the COALESCEd column.
			if err := rows.Scan(&entityID, &eventType, &eventAtStr, &detectedAtStr); err != nil {
				rows.Close()
				return nil, err
			}
			eventAt, err := parseDBDatetime(eventAtStr)
			if err != nil {
				rows.Close()
				return nil, err
			}
			detectedAt, err := parseDBDatetime(detectedAtStr)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[entityID] = append(out[entityID], domain.FactoryRecentEvent{
				EventType:  eventType,
				CreatedAt:  eventAt,
				DetectedAt: detectedAt,
			})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// sqliteFactoryEntitySelectColumns is the SELECT list shared by the
// active and closed-grace queries in Entities. Mirrors the pre-D2
// const of the same shape; the second subquery reads created_at as
// a typed DATETIME (no COALESCE) so the driver scans directly into
// sql.NullTime — no parseDBDatetime detour for this column.
const sqliteFactoryEntitySelectColumns = `
	e.id, e.source, e.source_id, e.kind,
	COALESCE(e.title, ''), COALESCE(e.url, ''),
	COALESCE(e.snapshot_json, ''), COALESCE(e.description, ''),
	e.state, e.created_at, e.last_polled_at, e.closed_at,
	(SELECT event_type FROM events WHERE entity_id = e.id ORDER BY created_at DESC LIMIT 1),
	(SELECT created_at FROM events WHERE entity_id = e.id ORDER BY created_at DESC LIMIT 1)
`

func (s *factoryReadStore) Entities(ctx context.Context, orgID string, limit int) ([]domain.FactoryEntityRow, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// Two separate queries instead of a single OR'd WHERE: the OR
	// spans two columns and prevents SQLite from choosing a
	// single-index plan, forcing a filtered table scan on big entity
	// tables. A combined LIMIT also lets a burst of closures crowd
	// the active set out of the snapshot, silently shrinking the
	// meaning of `limit` — the active half should always get its
	// full budget.
	//
	// Membership semi-join: an entity belongs in a team's factory iff
	// a task for that team has *ever* existed on it (over all task
	// statuses — a long-closed task still counts). Repos are configured
	// org-wide, so polling produces org-wide entities; the entity↔team
	// relationship is derived through tasks (which carry team_id) rather
	// than forked onto the shared entity. Local mode is N=1 (one team),
	// so the EXISTS is just "has any task." The semi-join hits the
	// dedup index on tasks(entity_id); membership is monotonic since
	// tasks terminate to done/dismissed rather than hard-delete.
	active, err := queryFactoryEntities(ctx, s.q, `
		SELECT `+sqliteFactoryEntitySelectColumns+`
		FROM entities e
		WHERE e.state = 'active'
		  AND EXISTS (SELECT 1 FROM tasks t WHERE t.entity_id = e.id)
		ORDER BY e.created_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}

	graceCutoff := time.Now().Add(-db.FactoryClosedGracePeriod)
	closed, err := queryFactoryEntities(ctx, s.q, `
		SELECT `+sqliteFactoryEntitySelectColumns+`
		FROM entities e
		WHERE e.closed_at IS NOT NULL AND e.closed_at > ?
		  AND EXISTS (SELECT 1 FROM tasks t WHERE t.entity_id = e.id)
		ORDER BY e.closed_at DESC
		LIMIT ?
	`, graceCutoff, db.FactoryClosedGraceLimit)
	if err != nil {
		return nil, err
	}

	if len(closed) == 0 {
		return active, nil
	}
	return append(active, closed...), nil
}

func queryFactoryEntities(ctx context.Context, q queryer, query string, args ...any) ([]domain.FactoryEntityRow, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.FactoryEntityRow
	for rows.Next() {
		var row domain.FactoryEntityRow
		var latestType sql.NullString
		var latestAt sql.NullTime
		if err := rows.Scan(
			&row.Entity.ID, &row.Entity.Source, &row.Entity.SourceID, &row.Entity.Kind,
			&row.Entity.Title, &row.Entity.URL,
			&row.Entity.SnapshotJSON, &row.Entity.Description,
			&row.Entity.State, &row.Entity.CreatedAt, &row.Entity.LastPolledAt, &row.Entity.ClosedAt,
			&latestType, &latestAt,
		); err != nil {
			return nil, err
		}
		if latestType.Valid {
			row.LatestEventType = latestType.String
		}
		if latestAt.Valid {
			row.LatestEventAt = &latestAt.Time
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// parseDBDatetime handles the ISO-8601 and SQLite-default datetime
// formats we see in the events table. Direct time.Time scans work for
// plain DATETIME columns because the SQLite driver reads the column
// type and parses; once COALESCE enters the picture that type metadata
// is gone and we get back a raw string.
//
// Accepted shapes, in order:
//   - RFC3339 ("2006-01-02T15:04:05Z07:00")
//   - modernc with _time_format=sqlite ("2006-01-02 15:04:05.999999999-07:00")
//   - SQLite default ("2006-01-02 15:04:05" — CURRENT_TIMESTAMP, naive UTC)
//   - RFC3339 with fractional seconds and 'T' separator
//   - Legacy Go time.String() ("2006-01-02 15:04:05.999999999 -0700 MST"),
//     produced by older modernc-driver writes that bound time.Time as its
//     default String() form. The monotonic-clock suffix " m=+..." is
//     stripped before parsing because time.Parse can't consume it.
func parseDBDatetime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02 15:04:05.999999999-07:00", s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02T15:04:05.999999999Z07:00", s); err == nil {
		return t, nil
	}
	cleaned := s
	if i := strings.Index(cleaned, " m=+"); i >= 0 {
		cleaned = cleaned[:i]
	} else if i := strings.Index(cleaned, " m=-"); i >= 0 {
		cleaned = cleaned[:i]
	}
	return time.Parse("2006-01-02 15:04:05.999999999 -0700 MST", cleaned)
}
