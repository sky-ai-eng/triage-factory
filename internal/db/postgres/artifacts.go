package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// artifactStore is the Postgres impl of db.ArtifactStore. The artifacts_* RLS
// policies scope reads/writes by team_id exactly like conversations; org_id
// stays in every WHERE/INSERT clause as defense in depth. Mirrors the
// conversations store (conversation.go) for $N placeholders + scan conventions.
// See TFAC-455.
//
// Holds two pools, the same split Conversations / ConversationWorktrees use:
//
//   - q: app pool (tf_app, RLS-active). Manual conversations' exec writes
//     (under synthetic claims) and conversation-detail / C2 reads route here.
//
//   - admin: admin pool (BYPASSRLS). UpsertSystem routes here for the exec
//     choke point on an event-triggered conversation, whose insert is
//     unreachable through tf_app — the conversation carries no creator user,
//     so the
//     artifacts_insert policy's team-write check can never pass. See
//     TFAC-459.
type artifactStore struct {
	q     queryer
	admin queryer
}

func newArtifactStore(q, admin queryer) db.ArtifactStore {
	return &artifactStore{q: q, admin: admin}
}

var _ db.ArtifactStore = (*artifactStore)(nil)

// pgArtifactColumns is the SELECT/RETURNING list scanned into a
// domain.Artifact via scanArtifact. Nullable text columns are coalesced to
// an empty string so the scan targets are plain strings, the same shape
// pgConversationColumns uses.
const pgArtifactColumns = `
	id, COALESCE(conversation_id::text, ''), org_id, team_id, provider, kind, target,
	COALESCE(external_id, ''), COALESCE(url, ''), state, dedup_key,
	COALESCE(details_json, ''), created_at, updated_at
`

// pgArtifactNonTerminalPredicate is the SQL WHERE fragment selecting the
// reconciler's working set — non-terminal artifacts backed by a fetchable
// GitHub object (TFAC-464). Built from the domain consts (no bind params, all
// literals) so it can't drift from domain.IsReconcilableNonTerminal. The state
// strings carry no quotes, so the concatenation is injection-safe.
// Review is intentionally absent: a review is staged TF-side until the atomic
// submit at approval, so a pending review draft has no GitHub object to
// reconcile (TFAC-494). Mirrors domain.reconcilableNonTerminal.
var pgArtifactNonTerminalPredicate = fmt.Sprintf(`(
		   (kind = '%s' AND state IN ('%s', '%s'))
		OR (kind = '%s' AND state = '%s')
	)`,
	domain.ArtifactKindPullRequest, domain.ArtifactStatePRDraft, domain.ArtifactStatePROpen,
	domain.ArtifactKindBranch, domain.ArtifactStateBranchPushed,
)

func (s *artifactStore) Upsert(ctx context.Context, orgID string, a domain.Artifact) (domain.Artifact, error) {
	return s.upsert(ctx, s.q, orgID, a)
}

// UpsertSystem runs the same UPSERT on the admin pool (BYPASSRLS) for
// event-triggered exec writers that have no JWT-claims context. See the type
// doc and TFAC-459.
func (s *artifactStore) UpsertSystem(ctx context.Context, orgID string, a domain.Artifact) (domain.Artifact, error) {
	return s.upsert(ctx, s.admin, orgID, a)
}

// InsertArtifactIfAbsentSystem inserts a only when no (org_id, dedup_key) row
// exists — ON CONFLICT DO NOTHING — on the admin pool (BYPASSRLS; the reconciler
// backstop has no JWT-claims context). Returns whether a row was inserted:
// RowsAffected is 1 on insert, 0 when a row already existed. See the interface
// doc for why this never-overwrite path (not UpsertSystem) is what the backstop
// needs. org_id stays bound as defense in depth.
func (s *artifactStore) InsertArtifactIfAbsentSystem(ctx context.Context, orgID string, a domain.Artifact) (bool, error) {
	res, err := s.admin.ExecContext(ctx, `
		INSERT INTO artifacts
			(id, conversation_id, org_id, team_id, provider, kind, target,
			 external_id, url, state, dedup_key, details_json, updated_at)
		VALUES (COALESCE(NULLIF($1, '')::uuid, gen_random_uuid()),
		        NULLIF($2, '')::uuid, $3, $4, $5, $6, $7,
		        NULLIF($8, ''), NULLIF($9, ''), $10, $11, NULLIF($12, ''), now())
		ON CONFLICT (org_id, dedup_key) DO NOTHING`,
		a.ID, a.ConversationID, orgID, a.TeamID, a.Provider, a.Kind, a.Target,
		a.ExternalID, a.URL, a.State, a.DedupKey, a.DetailsJSON,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (s *artifactStore) upsert(ctx context.Context, q queryer, orgID string, a domain.Artifact) (domain.Artifact, error) {
	// ON CONFLICT(org_id, dedup_key) updates the documented mutable fields
	// from the proposed row (EXCLUDED.*) and bumps updated_at; id/created_at
	// on the existing row are preserved. provider/kind are deliberately NOT
	// updated: they are encoded into dedup_key (the conflict target), so a
	// conflicting row that disagreed on them would be keyed wrong — the
	// insert side pins them, the update side leaves them. A caller-supplied
	// a.ID is honored on insert (parity with SQLite); an empty a.ID falls
	// back to gen_random_uuid() server-side.
	//
	// target/external_id/url are preserved-on-empty: they are the backing
	// object's stable coordinates (resource key / PR number / issue key, html
	// link) — once known they only ever fill in or migrate to a more specific
	// value (pending PR owner/repo → owner/repo#123), never legitimately clear.
	// A later upsert that can't supply them — a reconciliation pass, a Jira
	// mutation whose conversation can't compute the browse URL, or a GitHub
	// comment-update/delete that only knows the comment id, not its PR number —
	// must not blank a value an earlier writer already stored. external_id/url
	// are NULLIFed to NULL on insert, so COALESCE preserves them; target is NOT
	// NULL, so NULLIF('','') folds an empty incoming target to NULL first. A
	// non-empty incoming value still overwrites (the intentional-change /
	// migration path). state/details_json stay last-writer-wins by design
	// (state tracks the latest action).
	//
	// conversation_id AND team_id are both preserved-on-conflict, NOT last-writer-wins:
	// the artifact belongs to its CREATING conversation (domain.Artifact's doc),
	// and team_id is denormalized off THAT SAME conversation — not off whichever
	// writer happens to touch the row later — so a later writer touching the
	// same dedup key (e.g. a different conversation editing a Slack message, or
	// a re-delegate updating a GitHub comment) must not reassign either.
	// Letting team_id follow the last writer while conversation_id stayed
	// pinned to the first would desync the two and mis-scope the artifact out
	// of its owning team's reads.
	row := q.QueryRowContext(ctx, `
		INSERT INTO artifacts
			(id, conversation_id, org_id, team_id, provider, kind, target,
			 external_id, url, state, dedup_key, details_json, updated_at)
		VALUES (COALESCE(NULLIF($1, '')::uuid, gen_random_uuid()),
		        NULLIF($2, '')::uuid, $3, $4, $5, $6, $7,
		        NULLIF($8, ''), NULLIF($9, ''), $10, $11, NULLIF($12, ''), now())
		ON CONFLICT (org_id, dedup_key) DO UPDATE SET
			conversation_id       = artifacts.conversation_id,
			team_id      = artifacts.team_id,
			target       = COALESCE(NULLIF(EXCLUDED.target, ''), artifacts.target),
			external_id  = COALESCE(EXCLUDED.external_id, artifacts.external_id),
			url          = COALESCE(EXCLUDED.url, artifacts.url),
			state        = EXCLUDED.state,
			details_json = EXCLUDED.details_json,
			updated_at   = now()
		RETURNING `+pgArtifactColumns,
		a.ID, a.ConversationID, orgID, a.TeamID, a.Provider, a.Kind, a.Target,
		a.ExternalID, a.URL, a.State, a.DedupKey, a.DetailsJSON,
	)
	var out domain.Artifact
	if err := scanArtifact(row, &out); err != nil {
		return domain.Artifact{}, err
	}
	return out, nil
}

// TransitionReviewState is the review CAS: one UPDATE guarded on the current
// state, so of two racing approves exactly one sees a row flip (RowsAffected=1)
// and the loser gets false without touching anything. external_id / url /
// details_json follow Upsert's preserve-on-empty convention. App pool — every
// caller is a request handler under claims; RLS scopes the row by team like
// every other app-side artifact write.
func (s *artifactStore) TransitionReviewState(ctx context.Context, orgID, id, from, to, externalID, url, detailsJSON string) (bool, error) {
	return s.transitionReviewState(ctx, s.q, orgID, id, from, to, externalID, url, detailsJSON)
}

// TransitionReviewStateSystem runs the same CAS on the admin pool (BYPASSRLS)
// for event-triggered exec writers that have no JWT-claims context — the same
// split UpdateReviewDetailsIfPendingSystem covers. org_id stays bound as
// defense in depth.
func (s *artifactStore) TransitionReviewStateSystem(ctx context.Context, orgID, id, from, to, externalID, url, detailsJSON string) (bool, error) {
	return s.transitionReviewState(ctx, s.admin, orgID, id, from, to, externalID, url, detailsJSON)
}

func (s *artifactStore) transitionReviewState(ctx context.Context, q queryer, orgID, id, from, to, externalID, url, detailsJSON string) (bool, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE artifacts
		SET state        = $1,
		    external_id  = COALESCE(NULLIF($2, ''), external_id),
		    url          = COALESCE(NULLIF($3, ''), url),
		    details_json = COALESCE(NULLIF($4, ''), details_json),
		    updated_at   = now()
		WHERE org_id = $5 AND id = $6 AND kind = $7 AND state = $8
	`, to, externalID, url, detailsJSON, orgID, id, domain.ArtifactKindReview, from)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// UpdateReviewDetailsIfPending guards the draft-mutation write on the row still
// being a pending review, so a stale writer can't resurrect a resolved one.
// App pool (RLS-active), under request or synthetic claims.
func (s *artifactStore) UpdateReviewDetailsIfPending(ctx context.Context, orgID, id, detailsJSON string) (bool, error) {
	return s.updateReviewDetailsIfPending(ctx, s.q, orgID, id, detailsJSON)
}

// UpdateReviewDetailsIfPendingSystem runs the same guarded UPDATE on the admin
// pool (BYPASSRLS) for event-triggered exec writers that have no JWT-claims
// context — the same split UpsertSystem covers. org_id stays bound as defense
// in depth.
func (s *artifactStore) UpdateReviewDetailsIfPendingSystem(ctx context.Context, orgID, id, detailsJSON string) (bool, error) {
	return s.updateReviewDetailsIfPending(ctx, s.admin, orgID, id, detailsJSON)
}

func (s *artifactStore) updateReviewDetailsIfPending(ctx context.Context, q queryer, orgID, id, detailsJSON string) (bool, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE artifacts
		SET details_json = NULLIF($1, ''), updated_at = now()
		WHERE org_id = $2 AND id = $3 AND kind = $4 AND state = $5
	`, detailsJSON, orgID, id, domain.ArtifactKindReview, domain.ArtifactStateReviewPending)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (s *artifactStore) Get(ctx context.Context, orgID, id string) (*domain.Artifact, error) {
	// App pool (RLS-active): every caller is an HTTP handler under request
	// claims. RLS scopes the row to the caller's team exactly like
	// conversations; org_id stays bound as defense in depth.
	row := s.q.QueryRowContext(ctx, `
		SELECT `+pgArtifactColumns+`
		FROM artifacts
		WHERE org_id = $1 AND id = $2
	`, orgID, id)
	var a domain.Artifact
	if err := scanArtifact(row, &a); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

func (s *artifactStore) ListByConversation(ctx context.Context, orgID, conversationID string) ([]domain.Artifact, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgArtifactColumns+`
		FROM artifacts
		WHERE org_id = $1 AND conversation_id = $2
		ORDER BY created_at DESC, id DESC
	`, orgID, conversationID)
	if err != nil {
		return nil, err
	}
	return scanArtifactRows(rows)
}

// ListByConversationSystem runs ListByConversation's query on the admin pool
// (BYPASSRLS) for the spawner's park check, which reads a completed
// conversation's artifacts from a goroutine with no JWT-claims context. org_id
// stays in the WHERE clause as defense in depth.
func (s *artifactStore) ListByConversationSystem(ctx context.Context, orgID, conversationID string) ([]domain.Artifact, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT `+pgArtifactColumns+`
		FROM artifacts
		WHERE org_id = $1 AND conversation_id = $2
		ORDER BY created_at DESC, id DESC
	`, orgID, conversationID)
	if err != nil {
		return nil, err
	}
	return scanArtifactRows(rows)
}

// ListPendingReviewsByTargetSystem runs on the admin pool (BYPASSRLS) for the
// PR coherence feed — a background eventbus subscriber with no JWT-claims
// context that must see every team's pending review drafts anchored to the PR
// (the head-SHA change is org-wide, not team-scoped). Filters to pending review
// drafts on the given PR target. org_id stays in the WHERE clause as defense in
// depth.
func (s *artifactStore) ListPendingReviewsByTargetSystem(ctx context.Context, orgID, target string) ([]domain.Artifact, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT `+pgArtifactColumns+`
		FROM artifacts
		WHERE org_id = $1 AND kind = $2 AND state = $3 AND target = $4
		ORDER BY created_at DESC, id DESC
	`, orgID, domain.ArtifactKindReview, domain.ArtifactStateReviewPending, target)
	if err != nil {
		return nil, err
	}
	return scanArtifactRows(rows)
}

func (s *artifactStore) CountByConversation(ctx context.Context, orgID string, conversationIDs []string) (map[string]int, error) {
	counts := make(map[string]int, len(conversationIDs))
	if len(conversationIDs) == 0 {
		return counts, nil
	}
	// App pool (RLS-active): both callers are HTTP handlers under request
	// claims, so the count is team-scoped exactly like ListByConversation. conversation_id is a
	// uuid column, so the slice goes through pgUUIDArray (a Postgres array
	// literal) rather than a raw []string bind — a []string encodes as text[]
	// and `conversation_id = ANY(text[])` errors with "operator does not exist: uuid =
	// text". A NULL conversation_id never matches ANY, so detached artifacts are excluded.
	rows, err := s.q.QueryContext(ctx, `
		SELECT conversation_id::text, COUNT(*)
		FROM artifacts
		WHERE org_id = $1 AND conversation_id = ANY($2)
		GROUP BY conversation_id
	`, orgID, pgUUIDArray(conversationIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var conversationID string
		var n int
		if err := rows.Scan(&conversationID, &n); err != nil {
			return nil, err
		}
		counts[conversationID] = n
	}
	return counts, rows.Err()
}

func (s *artifactStore) ListByConversations(ctx context.Context, orgID string, conversationIDs []string) ([]domain.Artifact, error) {
	if len(conversationIDs) == 0 {
		return nil, nil
	}
	// App pool (RLS-active): the caller is the conversation-list handler
	// under request claims, so rows are team-scoped exactly like
	// ListByConversation. conversation_id is a uuid column, so the slice
	// goes through pgUUIDArray (a Postgres array literal), not a raw
	// []string bind — see CountByConversation. A NULL conversation_id never
	// matches ANY, so detached artifacts are excluded.
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgArtifactColumns+`
		FROM artifacts
		WHERE org_id = $1 AND conversation_id = ANY($2)
		ORDER BY created_at DESC, id DESC
	`, orgID, pgUUIDArray(conversationIDs))
	if err != nil {
		return nil, err
	}
	return scanArtifactRows(rows)
}

func (s *artifactStore) ListByTeam(ctx context.Context, orgID, teamID string, opts db.ArtifactListOpts) ([]domain.Artifact, int, error) {
	total, err := countPgArtifacts(ctx, s.q, `org_id = $1 AND team_id = $2`, []any{orgID, teamID}, opts)
	if err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []domain.Artifact{}, total, nil
	}
	// Base WHERE only — the filter helper appends the optional predicates, the
	// ORDER BY, and LIMIT/OFFSET so this and ListByOrgSystem share one builder.
	query := `SELECT ` + pgArtifactColumns + ` FROM artifacts WHERE org_id = $1 AND team_id = $2`
	query, args := appendPgArtifactFilters(query, []any{orgID, teamID}, opts)
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	out, err := scanArtifactRows(rows)
	return out, total, err
}

// appendPgArtifactFilters appends opts' optional provider/kind/state/time
// predicates, the newest-first ORDER BY, and LIMIT/OFFSET paging to a SELECT
// whose WHERE clause is already open (the caller emitted at least `WHERE org_id
// = $1`). Placeholders are numbered from len(args)+1, so a caller can prepend
// any number of leading binds (org_id, team_id) without a filter colliding with
// LIMIT — the same defensive numbering ListByTeam used for its lone LIMIT.
func appendPgArtifactFilters(query string, args []any, opts db.ArtifactListOpts) (string, []any) {
	query, args = appendPgArtifactPredicates(query, args, opts)
	query += " ORDER BY created_at DESC, id DESC"
	if opts.Limit > 0 {
		args = append(args, opts.Limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
		// OFFSET only with a LIMIT — paging the newest-first feed.
		if opts.Offset > 0 {
			args = append(args, opts.Offset)
			query += fmt.Sprintf(" OFFSET $%d", len(args))
		}
	}
	return query, args
}

// appendPgArtifactPredicates is the filter half alone — no ordering, no window
// — so the count and the page apply exactly the same predicate.
func appendPgArtifactPredicates(query string, args []any, opts db.ArtifactListOpts) (string, []any) {
	if opts.Provider != "" {
		args = append(args, opts.Provider)
		query += fmt.Sprintf(" AND provider = $%d", len(args))
	}
	if opts.Kind != "" {
		args = append(args, opts.Kind)
		query += fmt.Sprintf(" AND kind = $%d", len(args))
	}
	if opts.State != "" {
		args = append(args, opts.State)
		query += fmt.Sprintf(" AND state = $%d", len(args))
	}
	if !opts.Since.IsZero() {
		args = append(args, opts.Since)
		query += fmt.Sprintf(" AND created_at >= $%d", len(args))
	}
	if !opts.Until.IsZero() {
		args = append(args, opts.Until)
		query += fmt.Sprintf(" AND created_at < $%d", len(args))
	}
	return query, args
}

func countPgArtifacts(ctx context.Context, q queryer, where string, base []any, opts db.ArtifactListOpts) (int, error) {
	query, args := appendPgArtifactPredicates(`SELECT COUNT(*) FROM artifacts WHERE `+where, base, opts)
	var n int
	err := q.QueryRowContext(ctx, query, args...).Scan(&n)
	return n, err
}

// ListByOrgSystem runs on the admin pool (BYPASSRLS), org-wide and across every
// team — the bot-activity audit feed's org source (TFAC-483). It returns ALL
// states (terminal included), in contrast to ListNonTerminalBySystem's
// reconciler working set. Same org-wide admin shape as ListNonTerminalBySystem;
// org_id stays in the WHERE clause as defense in depth.
func (s *artifactStore) ListByOrgSystem(ctx context.Context, orgID string, opts db.ArtifactListOpts) ([]domain.Artifact, int, error) {
	total, err := countPgArtifacts(ctx, s.admin, `org_id = $1`, []any{orgID}, opts)
	if err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []domain.Artifact{}, total, nil
	}
	query := `SELECT ` + pgArtifactColumns + ` FROM artifacts WHERE org_id = $1`
	query, args := appendPgArtifactFilters(query, []any{orgID}, opts)
	rows, err := s.admin.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	out, err := scanArtifactRows(rows)
	return out, total, err
}

// ListNonTerminalBySystem runs on the admin pool (BYPASSRLS), org-scoped — the
// reconciler is a background system job with no JWT-claims context and must see
// every team's non-terminal artifacts to keep them fresh against GitHub. Same
// org-wide admin shape the scorer's UnscoredTasks and EntityStore's
// ListActiveTerminalCandidatesSystem use. org_id stays in the WHERE clause as
// defense in depth.
func (s *artifactStore) ListNonTerminalBySystem(ctx context.Context, orgID string) ([]domain.Artifact, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT `+pgArtifactColumns+`
		FROM artifacts
		WHERE org_id = $1 AND `+pgArtifactNonTerminalPredicate+`
		ORDER BY created_at DESC, id DESC
	`, orgID)
	if err != nil {
		return nil, err
	}
	return scanArtifactRows(rows)
}

// --- Helpers ---

func scanArtifactRows(rows *sql.Rows) ([]domain.Artifact, error) {
	defer rows.Close()
	var out []domain.Artifact
	for rows.Next() {
		var a domain.Artifact
		if err := scanArtifact(rows, &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// rowScanner unifies *sql.Row and *sql.Rows so scanArtifact serves both
// the single-row Upsert RETURNING and the list paths.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanArtifact(sc rowScanner, a *domain.Artifact) error {
	return sc.Scan(
		&a.ID, &a.ConversationID, &a.OrgID, &a.TeamID, &a.Provider, &a.Kind, &a.Target,
		&a.ExternalID, &a.URL, &a.State, &a.DedupKey, &a.DetailsJSON, &a.CreatedAt, &a.UpdatedAt,
	)
}
