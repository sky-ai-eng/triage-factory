package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// deliverableStore is the Postgres impl of db.DeliverableStore. The deliverables_* RLS
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
//     deliverables_insert policy's team-write check can never pass. See
//     TFAC-459.
type deliverableStore struct {
	q     queryer
	admin queryer
}

func newDeliverableStore(q, admin queryer) db.DeliverableStore {
	return &deliverableStore{q: q, admin: admin}
}

var _ db.DeliverableStore = (*deliverableStore)(nil)

// pgDeliverableColumns is the SELECT/RETURNING list scanned into a
// domain.Deliverable via scanDeliverable. Nullable text columns are coalesced to
// an empty string so the scan targets are plain strings, the same shape
// pgConversationColumns uses.
const pgDeliverableColumns = `
	id, COALESCE(conversation_id::text, ''), org_id, team_id, provider, kind, scope, target,
	COALESCE(external_id, ''), COALESCE(url, ''), COALESCE(node_id, ''),
	COALESCE(entity_id::text, ''),
	state, dedup_key, COALESCE(details_json, ''), created_at, updated_at
`

// pgDeliverableNonTerminalPredicate is the SQL WHERE fragment selecting the
// reconciler's working set — non-terminal deliverables backed by a fetchable
// GitHub object (TFAC-464). Built from the domain consts (no bind params, all
// literals) so it can't drift from domain.IsReconcilableNonTerminal. The state
// strings carry no quotes, so the concatenation is injection-safe.
// Review is intentionally absent: a review is staged TF-side until the atomic
// submit at approval, so a pending review draft has no GitHub object to
// reconcile (TFAC-494). Mirrors domain.reconcilableNonTerminal.
var pgDeliverableNonTerminalPredicate = fmt.Sprintf(`(
		   (kind = '%s' AND state IN ('%s', '%s'))
		OR (kind = '%s' AND state = '%s')
	)`,
	domain.DeliverableKindPullRequest, domain.DeliverableStatePRDraft, domain.DeliverableStatePROpen,
	domain.DeliverableKindBranch, domain.DeliverableStateBranchPushed,
)

func (s *deliverableStore) Upsert(ctx context.Context, orgID string, a domain.Deliverable) (domain.Deliverable, error) {
	return s.upsert(ctx, s.q, orgID, a)
}

// UpsertSystem runs the same UPSERT on the admin pool (BYPASSRLS) for
// event-triggered exec writers that have no JWT-claims context. See the type
// doc and TFAC-459.
func (s *deliverableStore) UpsertSystem(ctx context.Context, orgID string, a domain.Deliverable) (domain.Deliverable, error) {
	return s.upsert(ctx, s.admin, orgID, a)
}

// InsertDeliverableIfAbsentSystem inserts a only when no (org_id, scope,
// dedup_key) row exists — ON CONFLICT DO NOTHING — on the admin pool
// (BYPASSRLS; the reconciler
// backstop has no JWT-claims context). Returns whether a row was inserted:
// RowsAffected is 1 on insert, 0 when a row already existed. See the interface
// doc for why this never-overwrite path (not UpsertSystem) is what the backstop
// needs. org_id stays bound as defense in depth.
func (s *deliverableStore) InsertDeliverableIfAbsentSystem(ctx context.Context, orgID string, a domain.Deliverable) (bool, error) {
	if err := db.RequireExternalObjectScope(a.Scope); err != nil {
		return false, err
	}
	res, err := s.admin.ExecContext(ctx, `
		INSERT INTO deliverables
			(id, conversation_id, org_id, team_id, provider, kind, target,
			 external_id, url, state, dedup_key, details_json, updated_at, scope,
			 node_id, entity_id)
		VALUES (COALESCE(NULLIF($1, '')::uuid, gen_random_uuid()),
		        NULLIF($2, '')::uuid, $3, $4, $5, $6, $7,
		        NULLIF($8, ''), NULLIF($9, ''), $10, $11, NULLIF($12, ''), now(), $13,
		        NULLIF($14, ''), NULLIF($15, '')::uuid)
		ON CONFLICT (org_id, scope, dedup_key) DO NOTHING`,
		a.ID, a.ConversationID, orgID, a.TeamID, a.Provider, a.Kind, a.Target,
		a.ExternalID, a.URL, a.State, a.DedupKey, a.DetailsJSON, a.Scope,
		a.NodeID, a.EntityID,
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

func (s *deliverableStore) upsert(ctx context.Context, q queryer, orgID string, a domain.Deliverable) (domain.Deliverable, error) {
	if err := db.RequireExternalObjectScope(a.Scope); err != nil {
		return domain.Deliverable{}, err
	}
	// ON CONFLICT(org_id, scope, dedup_key) updates the documented mutable
	// fields from the proposed row (EXCLUDED.*) and bumps updated_at;
	// id/created_at on the existing row are preserved. provider/kind/scope are
	// deliberately NOT updated: they are encoded into the conflict target, so
	// a conflicting row that disagreed on them would be keyed wrong — the
	// insert side pins them, the update side leaves them. A caller-supplied
	// a.ID is honored on insert (parity with SQLite); an empty a.ID falls
	// back to gen_random_uuid() server-side.
	//
	// target/external_id/url/node_id are preserved-on-empty: they are the
	// backing object's stable coordinates (resource key / PR number / issue
	// key, html link, GraphQL node id) — once known they only ever fill in or
	// migrate to a more specific value (pending PR owner/repo →
	// owner/repo#123), never legitimately clear. A later upsert that can't
	// supply them — a reconciliation pass, a Jira mutation whose conversation
	// can't compute the browse URL, or a GitHub comment delete that only knows
	// the comment id, not its PR number — must not blank a value an
	// earlier writer already stored. external_id/url/node_id are NULLIFed to
	// NULL on insert, so COALESCE preserves them; target is NOT NULL, so
	// NULLIF('','') folds an empty incoming target to NULL first. A non-empty
	// incoming value still overwrites (the intentional-change / migration
	// path). details_json is preserved-on-empty for the same reason: a comment
	// delete cannot restate the thread a reply answers. So is entity_id: a
	// writer that resolved no entity must not unlink a row another writer
	// linked. state stays last-writer-wins by design (it tracks the latest
	// action).
	//
	// conversation_id AND team_id are both preserved-on-conflict, NOT last-writer-wins:
	// the deliverable belongs to its CREATING conversation (domain.Deliverable's doc),
	// and team_id is denormalized off THAT SAME conversation — not off whichever
	// writer happens to touch the row later — so a later writer touching the
	// same dedup key (e.g. a different conversation editing a Slack message, or
	// a re-delegate updating a GitHub comment) must not reassign either.
	// Letting team_id follow the last writer while conversation_id stayed
	// pinned to the first would desync the two and mis-scope the deliverable out
	// of its owning team's reads.
	row := q.QueryRowContext(ctx, `
		INSERT INTO deliverables
			(id, conversation_id, org_id, team_id, provider, kind, target,
			 external_id, url, state, dedup_key, details_json, updated_at, scope,
			 node_id, entity_id)
		VALUES (COALESCE(NULLIF($1, '')::uuid, gen_random_uuid()),
		        NULLIF($2, '')::uuid, $3, $4, $5, $6, $7,
		        NULLIF($8, ''), NULLIF($9, ''), $10, $11, NULLIF($12, ''), now(), $13,
		        NULLIF($14, ''), NULLIF($15, '')::uuid)
		ON CONFLICT (org_id, scope, dedup_key) DO UPDATE SET
			conversation_id       = deliverables.conversation_id,
			team_id      = deliverables.team_id,
			target       = COALESCE(NULLIF(EXCLUDED.target, ''), deliverables.target),
			external_id  = COALESCE(EXCLUDED.external_id, deliverables.external_id),
			url          = COALESCE(EXCLUDED.url, deliverables.url),
			node_id      = COALESCE(EXCLUDED.node_id, deliverables.node_id),
			entity_id    = COALESCE(EXCLUDED.entity_id, deliverables.entity_id),
			state        = EXCLUDED.state,
			details_json = COALESCE(EXCLUDED.details_json, deliverables.details_json),
			updated_at   = now()
		RETURNING `+pgDeliverableColumns,
		a.ID, a.ConversationID, orgID, a.TeamID, a.Provider, a.Kind, a.Target,
		a.ExternalID, a.URL, a.State, a.DedupKey, a.DetailsJSON, a.Scope,
		a.NodeID, a.EntityID,
	)
	var out domain.Deliverable
	if err := scanDeliverable(row, &out); err != nil {
		return domain.Deliverable{}, err
	}
	return out, nil
}

// TransitionReviewState is the review CAS: one UPDATE guarded on the current
// state, so of two racing approves exactly one sees a row flip (RowsAffected=1)
// and the loser gets false without touching anything. external_id / url /
// details_json follow Upsert's preserve-on-empty convention. App pool — every
// caller is a request handler under claims; RLS scopes the row by team like
// every other app-side deliverable write.
func (s *deliverableStore) TransitionReviewState(ctx context.Context, orgID, id, from, to, externalID, url, detailsJSON string) (bool, error) {
	return s.transitionReviewState(ctx, s.q, orgID, id, from, to, externalID, url, detailsJSON)
}

// TransitionReviewStateSystem runs the same CAS on the admin pool (BYPASSRLS)
// for event-triggered exec writers that have no JWT-claims context — the same
// split UpdateReviewDetailsIfPendingSystem covers. org_id stays bound as
// defense in depth.
func (s *deliverableStore) TransitionReviewStateSystem(ctx context.Context, orgID, id, from, to, externalID, url, detailsJSON string) (bool, error) {
	return s.transitionReviewState(ctx, s.admin, orgID, id, from, to, externalID, url, detailsJSON)
}

func (s *deliverableStore) transitionReviewState(ctx context.Context, q queryer, orgID, id, from, to, externalID, url, detailsJSON string) (bool, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE deliverables
		SET state        = $1,
		    external_id  = COALESCE(NULLIF($2, ''), external_id),
		    url          = COALESCE(NULLIF($3, ''), url),
		    details_json = COALESCE(NULLIF($4, ''), details_json),
		    updated_at   = now()
		WHERE org_id = $5 AND id = $6 AND kind = $7 AND state = $8
	`, to, externalID, url, detailsJSON, orgID, id, domain.DeliverableKindReview, from)
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
func (s *deliverableStore) UpdateReviewDetailsIfPending(ctx context.Context, orgID, id, detailsJSON string) (bool, error) {
	return s.updateReviewDetailsIfPending(ctx, s.q, orgID, id, detailsJSON)
}

// UpdateReviewDetailsIfPendingSystem runs the same guarded UPDATE on the admin
// pool (BYPASSRLS) for event-triggered exec writers that have no JWT-claims
// context — the same split UpsertSystem covers. org_id stays bound as defense
// in depth.
func (s *deliverableStore) UpdateReviewDetailsIfPendingSystem(ctx context.Context, orgID, id, detailsJSON string) (bool, error) {
	return s.updateReviewDetailsIfPending(ctx, s.admin, orgID, id, detailsJSON)
}

func (s *deliverableStore) updateReviewDetailsIfPending(ctx context.Context, q queryer, orgID, id, detailsJSON string) (bool, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE deliverables
		SET details_json = NULLIF($1, ''), updated_at = now()
		WHERE org_id = $2 AND id = $3 AND kind = $4 AND state = $5
	`, detailsJSON, orgID, id, domain.DeliverableKindReview, domain.DeliverableStateReviewPending)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (s *deliverableStore) Get(ctx context.Context, orgID, id string) (*domain.Deliverable, error) {
	// App pool (RLS-active): every caller is an HTTP handler under request
	// claims. RLS scopes the row to the caller's team exactly like
	// conversations; org_id stays bound as defense in depth.
	row := s.q.QueryRowContext(ctx, `
		SELECT `+pgDeliverableColumns+`
		FROM deliverables
		WHERE org_id = $1 AND id = $2
	`, orgID, id)
	var a domain.Deliverable
	if err := scanDeliverable(row, &a); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

func (s *deliverableStore) ListByConversation(ctx context.Context, orgID, conversationID string, opts db.ListOpts) ([]domain.Deliverable, int, error) {
	query := `SELECT ` + pgDeliverableColumns + `
		FROM deliverables
		WHERE org_id = $1 AND conversation_id = $2
		ORDER BY created_at DESC, id DESC`
	args := []any{orgID, conversationID}
	// An unwindowed read holds every row, so its length is the total and no
	// count runs.
	windowed := opts.Limit > 0 || opts.CountOnly
	var total int
	if windowed {
		if err := s.q.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM deliverables WHERE org_id = $1 AND conversation_id = $2
		`, orgID, conversationID).Scan(&total); err != nil {
			return nil, 0, err
		}
		if opts.CountOnly {
			return []domain.Deliverable{}, total, nil
		}
		query += ` LIMIT $3 OFFSET $4`
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	out, err := scanDeliverableRows(rows)
	if !windowed {
		total = len(out)
	}
	return out, total, err
}

// ListByConversationSystem runs ListByConversation's query on the admin pool
// (BYPASSRLS) for the spawner's park check, which reads a completed
// conversation's deliverables from a goroutine with no JWT-claims context. org_id
// stays in the WHERE clause as defense in depth.
func (s *deliverableStore) ListByConversationSystem(ctx context.Context, orgID, conversationID string) ([]domain.Deliverable, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT `+pgDeliverableColumns+`
		FROM deliverables
		WHERE org_id = $1 AND conversation_id = $2
		ORDER BY created_at DESC, id DESC
	`, orgID, conversationID)
	if err != nil {
		return nil, err
	}
	return scanDeliverableRows(rows)
}

// ListPendingReviewsByEntitySystem runs on the admin pool (BYPASSRLS) for
// the PR coherence feed — a background eventbus subscriber with no JWT-claims
// context that must see every team's pending review drafts about the PR (the
// head-SHA change is org-wide, not team-scoped). org_id stays in the WHERE
// clause as defense in depth.
func (s *deliverableStore) ListPendingReviewsByEntitySystem(ctx context.Context, orgID, entityID string) ([]domain.Deliverable, error) {
	if entityID == "" {
		return nil, nil
	}
	rows, err := s.admin.QueryContext(ctx, `
		SELECT `+pgDeliverableColumns+`
		FROM deliverables
		WHERE org_id = $1 AND entity_id = $2 AND provider = $3 AND kind = $4 AND state = $5
		ORDER BY created_at DESC, id DESC
	`, orgID, entityID, domain.ProviderGitHub, domain.DeliverableKindReview, domain.DeliverableStateReviewPending)
	if err != nil {
		return nil, err
	}
	return scanDeliverableRows(rows)
}

// PresentDedupKeysSystem runs on the admin pool (BYPASSRLS): the recording
// funnel's system path reads it with no claims, and a comment's deliverable
// belongs to whichever team's run posted it. org_id stays in the WHERE clause
// as defense in depth.
func (s *deliverableStore) PresentDedupKeysSystem(ctx context.Context, orgID, scope string, dedupKeys []string) (map[string]bool, error) {
	if err := db.RequireExternalObjectScope(scope); err != nil {
		return nil, err
	}
	present := map[string]bool{}
	if len(dedupKeys) == 0 {
		return present, nil
	}
	rows, err := s.admin.QueryContext(ctx, `
		SELECT dedup_key
		FROM deliverables
		WHERE org_id = $1 AND scope = $2 AND dedup_key = ANY($3)
	`, orgID, scope, dedupKeys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		present[key] = true
	}
	return present, rows.Err()
}

// PresentExternalIDsSystem runs on the admin pool (BYPASSRLS): the tracker
// reads it with no claims, and a review's
// deliverable belongs to whichever team's run submitted it. idx_deliverables_external_id
// serves it. org_id stays in the WHERE clause as defense in depth.
func (s *deliverableStore) PresentExternalIDsSystem(ctx context.Context, orgID, scope, provider, kind string, externalIDs []string) (map[string]bool, error) {
	if err := db.RequireExternalObjectScope(scope); err != nil {
		return nil, err
	}
	present := map[string]bool{}
	if len(externalIDs) == 0 {
		return present, nil
	}
	rows, err := s.admin.QueryContext(ctx, `
		SELECT external_id
		FROM deliverables
		WHERE org_id = $1 AND scope = $2 AND provider = $3 AND kind = $4 AND external_id = ANY($5)
	`, orgID, scope, provider, kind, externalIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		present[id] = true
	}
	return present, rows.Err()
}

// GetByNodeIDSystem reads the newest row in scope carrying nodeID, on the
// admin pool (BYPASSRLS): its callers record a run's writes, and the row may
// belong to whichever team's run first recorded the object. idx_deliverables_node_id
// serves it. org_id stays bound as defense in depth.
func (s *deliverableStore) GetByNodeIDSystem(ctx context.Context, orgID, scope, nodeID string) (*domain.Deliverable, error) {
	if err := db.RequireExternalObjectScope(scope); err != nil {
		return nil, err
	}
	if nodeID == "" {
		return nil, nil
	}
	row := s.admin.QueryRowContext(ctx, `
		SELECT `+pgDeliverableColumns+`
		FROM deliverables
		WHERE org_id = $1 AND scope = $2 AND node_id = $3
		ORDER BY created_at DESC, id DESC
		LIMIT 1
	`, orgID, scope, nodeID)
	var a domain.Deliverable
	if err := scanDeliverable(row, &a); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

func (s *deliverableStore) CountByConversation(ctx context.Context, orgID string, conversationIDs []string) (map[string]int, error) {
	counts := make(map[string]int, len(conversationIDs))
	if len(conversationIDs) == 0 {
		return counts, nil
	}
	// App pool (RLS-active): both callers are HTTP handlers under request
	// claims, so the count is team-scoped exactly like ListByConversation. conversation_id is a
	// uuid column, so the slice goes through pgUUIDArray (a Postgres array
	// literal) rather than a raw []string bind — a []string encodes as text[]
	// and `conversation_id = ANY(text[])` errors with "operator does not exist: uuid =
	// text". A NULL conversation_id never matches ANY, so detached deliverables are excluded.
	rows, err := s.q.QueryContext(ctx, `
		SELECT conversation_id::text, COUNT(*)
		FROM deliverables
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

func (s *deliverableStore) ListByConversations(ctx context.Context, orgID string, conversationIDs []string) ([]domain.Deliverable, error) {
	if len(conversationIDs) == 0 {
		return nil, nil
	}
	// App pool (RLS-active): the caller is the conversation-list handler
	// under request claims, so rows are team-scoped exactly like
	// ListByConversation. conversation_id is a uuid column, so the slice
	// goes through pgUUIDArray (a Postgres array literal), not a raw
	// []string bind — see CountByConversation. A NULL conversation_id never
	// matches ANY, so detached deliverables are excluded.
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgDeliverableColumns+`
		FROM deliverables
		WHERE org_id = $1 AND conversation_id = ANY($2)
		ORDER BY created_at DESC, id DESC
	`, orgID, pgUUIDArray(conversationIDs))
	if err != nil {
		return nil, err
	}
	return scanDeliverableRows(rows)
}

// ListArtifactMatchesByTeamSystem runs on the admin pool (BYPASSRLS) behind
// its caller's gate (see the interface); org_id and team_id stay bound.
// idx_deliverables_entity and idx_deliverables_external_id serve the two arms.
func (s *deliverableStore) ListArtifactMatchesByTeamSystem(ctx context.Context, orgID, teamID string, entityIDs, reviewIDs []string) ([]domain.Deliverable, error) {
	return listPgArtifactMatches(ctx, s.admin, `org_id = $1 AND team_id = $2::uuid`, []any{orgID, teamID}, entityIDs, reviewIDs)
}

// ListArtifactMatchesByOrgSystem is the team read over every team, on the
// admin pool; org_id stays bound.
func (s *deliverableStore) ListArtifactMatchesByOrgSystem(ctx context.Context, orgID string, entityIDs, reviewIDs []string) ([]domain.Deliverable, error) {
	return listPgArtifactMatches(ctx, s.admin, `org_id = $1`, []any{orgID}, entityIDs, reviewIDs)
}

// listPgArtifactMatches is both scopes' read; where and args select the
// scope. An empty set binds as an empty array, which matches nothing.
func listPgArtifactMatches(ctx context.Context, q queryer, where string, args []any, entityIDs, reviewIDs []string) ([]domain.Deliverable, error) {
	if len(entityIDs) == 0 && len(reviewIDs) == 0 {
		return nil, nil
	}
	if reviewIDs == nil {
		reviewIDs = []string{}
	}
	args = append(args, domain.DeliverableKindPullRequest, pgUUIDArray(entityIDs), domain.DeliverableKindReview, reviewIDs)
	n := len(args)
	rows, err := q.QueryContext(ctx, fmt.Sprintf(`
		SELECT `+pgDeliverableColumns+`
		FROM deliverables
		WHERE `+where+`
		  AND ((kind = $%d AND entity_id = ANY($%d::uuid[])) OR (kind = $%d AND external_id = ANY($%d::text[])))
		ORDER BY created_at DESC, id DESC
	`, n-3, n-2, n-1, n), args...)
	if err != nil {
		return nil, err
	}
	return scanDeliverableRows(rows)
}

// ListNonTerminalBySystem runs on the admin pool (BYPASSRLS), org-scoped — the
// reconciler is a background system job with no JWT-claims context and must see
// every team's non-terminal deliverables to keep them fresh against GitHub. Same
// org-wide admin shape the scorer's UnscoredTasks and EntityStore's
// ListActiveTerminalCandidatesSystem use. org_id stays in the WHERE clause as
// defense in depth.
func (s *deliverableStore) ListNonTerminalBySystem(ctx context.Context, orgID string) ([]domain.Deliverable, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT `+pgDeliverableColumns+`
		FROM deliverables
		WHERE org_id = $1 AND `+pgDeliverableNonTerminalPredicate+`
		ORDER BY created_at DESC, id DESC
	`, orgID)
	if err != nil {
		return nil, err
	}
	return scanDeliverableRows(rows)
}

// --- Helpers ---

func scanDeliverableRows(rows *sql.Rows) ([]domain.Deliverable, error) {
	defer rows.Close()
	var out []domain.Deliverable
	for rows.Next() {
		var a domain.Deliverable
		if err := scanDeliverable(rows, &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// scanDeliverable fills a from one row. It takes a rowScanner so it serves both
// the single-row Upsert RETURNING and the list paths.
func scanDeliverable(sc rowScanner, a *domain.Deliverable) error {
	return sc.Scan(
		&a.ID, &a.ConversationID, &a.OrgID, &a.TeamID, &a.Provider, &a.Kind, &a.Scope, &a.Target,
		&a.ExternalID, &a.URL, &a.NodeID, &a.EntityID,
		&a.State, &a.DedupKey, &a.DetailsJSON, &a.CreatedAt, &a.UpdatedAt,
	)
}
