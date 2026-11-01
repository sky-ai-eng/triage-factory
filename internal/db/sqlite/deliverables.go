package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// deliverableNonTerminalPredicate is the SQL WHERE fragment selecting the
// reconciler's working set — non-terminal deliverables backed by a fetchable
// GitHub object. Built from the domain consts (no bind params, all
// literals) so it can't drift from domain.IsReconcilableNonTerminal, which
// TestDeliverableStore_SQLite_ListNonTerminal pins it equal to. The state strings
// carry no quotes, so the concatenation is injection-safe.
// Review is intentionally absent: a review is staged TF-side until the atomic
// submit at approval, so a pending review draft has no GitHub object to
// reconcile (TFAC-494). Mirrors domain.reconcilableNonTerminal.
var deliverableNonTerminalPredicate = fmt.Sprintf(`(kind = '%s' AND state IN ('%s', '%s'))`,
	domain.DeliverableKindPullRequest, domain.DeliverableStatePRDraft, domain.DeliverableStatePROpen,
)

// deliverableStore is the SQLite impl of db.DeliverableStore. SQLite is single-tenant
// (local mode, N=1) with no RLS — org_id exists for parity with the Postgres
// baseline and every caller passes LocalDefaultOrgID (asserted at each entry).
// Mirrors the conversations store (conversation.go) for the single-queryer
// shape and scan conventions. See TFAC-455.
type deliverableStore struct{ q queryer }

func newDeliverableStore(q queryer) db.DeliverableStore { return &deliverableStore{q: q} }

var _ db.DeliverableStore = (*deliverableStore)(nil)

// deliverableColumns is the SELECT list scanned into a domain.Deliverable via
// scanDeliverable. Same order the Postgres impl projects.
const deliverableColumns = `
	id, conversation_id, org_id, team_id, provider, kind, scope, target,
	external_id, url, node_id, entity_id,
	state, dedup_key, details_json, created_at, updated_at, verification
`

func (s *deliverableStore) Upsert(ctx context.Context, orgID string, a domain.Deliverable) (domain.Deliverable, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.Deliverable{}, err
	}
	if err := db.RequireExternalObjectScope(a.Scope); err != nil {
		return domain.Deliverable{}, err
	}
	if err := db.RequireDeliverableKind(a.Kind); err != nil {
		return domain.Deliverable{}, err
	}
	verification, err := db.DeliverableVerificationOrDefault(a.Kind, a.Verification)
	if err != nil {
		return domain.Deliverable{}, err
	}
	id := a.ID
	if id == "" {
		id = uuid.New().String()
	}
	// ON CONFLICT(org_id, scope, dedup_key) updates the documented mutable fields
	// from the proposed row (excluded.*) and bumps updated_at. id/created_at
	// are preserved on the existing row — a conflicting insert keeps the
	// original identity, the same UPSERT contract the Postgres impl has.
	// provider/kind/scope are deliberately NOT updated: they are encoded into
	// the conflict target, so the insert side pins them and the update side
	// leaves them rather than risk a row whose discriminators disagree with
	// its key.
	//
	// target/external_id/url/node_id are preserved-on-empty: they are the
	// backing object's stable coordinates (PR number or review id, html link,
	// GraphQL node id) — once known they only ever fill in, never
	// legitimately clear. A later upsert that can't supply them — a
	// reconciliation pass — must not blank a value an earlier writer already
	// stored. external_id/url/node_id are NULLIFed to NULL on insert, so
	// COALESCE preserves them; target is NOT NULL, so NULLIF('','') folds an
	// empty incoming target to NULL first. A non-empty incoming value still
	// overwrites (the intentional-change path). details_json is
	// preserved-on-empty for the same reason. So is entity_id: a writer that
	// resolved no entity must not unlink a row another writer linked. state
	// stays last-writer-wins by design (it tracks the latest action).
	//
	// conversation_id AND team_id are both preserved-on-conflict, NOT last-writer-wins:
	// the deliverable belongs to its CREATING conversation (domain.Deliverable's doc),
	// and team_id is denormalized off THAT SAME conversation — not off whichever
	// writer happens to touch the row later — so a later writer touching the
	// same dedup key (the reconciler, or the pull request backstop) must not
	// reassign either. Letting team_id follow the last writer while
	// conversation_id stayed pinned to the first would desync the two and
	// mis-scope the deliverable out of its owning team's reads.
	//
	// verification is inserted and never updated here: a row is settled only
	// through SettleVerificationSystem.
	row := s.q.QueryRowContext(ctx, `
		INSERT INTO deliverables
			(id, conversation_id, org_id, team_id, provider, kind, scope, target,
			 external_id, url, node_id, entity_id, state, dedup_key, details_json, updated_at, verification)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, ?)
		ON CONFLICT(org_id, scope, dedup_key) DO UPDATE SET
			conversation_id       = deliverables.conversation_id,
			team_id      = deliverables.team_id,
			target       = COALESCE(NULLIF(excluded.target, ''), deliverables.target),
			external_id  = COALESCE(excluded.external_id, deliverables.external_id),
			url          = COALESCE(excluded.url, deliverables.url),
			node_id      = COALESCE(excluded.node_id, deliverables.node_id),
			entity_id    = COALESCE(excluded.entity_id, deliverables.entity_id),
			state        = excluded.state,
			details_json = COALESCE(excluded.details_json, deliverables.details_json),
			updated_at   = CURRENT_TIMESTAMP
		RETURNING `+deliverableColumns,
		id, nullIfEmpty(a.ConversationID), orgID, a.TeamID, a.Provider, a.Kind, a.Scope, a.Target,
		nullIfEmpty(a.ExternalID), nullIfEmpty(a.URL), nullIfEmpty(a.NodeID), nullIfEmpty(a.EntityID),
		a.State, a.DedupKey, nullIfEmpty(a.DetailsJSON), verification,
	)
	var out domain.Deliverable
	if err := scanDeliverable(row, &out); err != nil {
		return domain.Deliverable{}, err
	}
	return out, nil
}

// UpsertSystem is identical to Upsert in SQLite: local mode is single-tenant
// (N=1) with no RLS, so there is no admin/app pool split — both run on the one
// connection. The method exists for parity with the Postgres store, where the
// exec choke point's event-triggered writers (no JWT-claims context) need an
// admin-pool path. See TFAC-459.
func (s *deliverableStore) UpsertSystem(ctx context.Context, orgID string, a domain.Deliverable) (domain.Deliverable, error) {
	return s.Upsert(ctx, orgID, a)
}

// InsertDeliverableIfAbsentSystem is identical to InsertDeliverableIfAbsent in
// SQLite: local mode has no admin/app pool split.
func (s *deliverableStore) InsertDeliverableIfAbsentSystem(ctx context.Context, orgID string, a domain.Deliverable) (bool, error) {
	return s.InsertDeliverableIfAbsent(ctx, orgID, a)
}

// InsertDeliverableIfAbsent inserts a only when no (org_id, scope, dedup_key)
// row exists — ON CONFLICT DO NOTHING — returning whether a row was inserted.
// See the interface doc: it never overwrites an existing row, so neither the
// reconciler backstop nor a pull request create can move a row another writer
// recorded.
func (s *deliverableStore) InsertDeliverableIfAbsent(ctx context.Context, orgID string, a domain.Deliverable) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	if err := db.RequireExternalObjectScope(a.Scope); err != nil {
		return false, err
	}
	if err := db.RequireDeliverableKind(a.Kind); err != nil {
		return false, err
	}
	verification, err := db.DeliverableVerificationOrDefault(a.Kind, a.Verification)
	if err != nil {
		return false, err
	}
	id := a.ID
	if id == "" {
		id = uuid.New().String()
	}
	res, err := s.q.ExecContext(ctx, `
		INSERT INTO deliverables
			(id, conversation_id, org_id, team_id, provider, kind, scope, target,
			 external_id, url, node_id, entity_id, state, dedup_key, details_json, updated_at, verification)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, ?)
		ON CONFLICT(org_id, scope, dedup_key) DO NOTHING`,
		id, nullIfEmpty(a.ConversationID), orgID, a.TeamID, a.Provider, a.Kind, a.Scope, a.Target,
		nullIfEmpty(a.ExternalID), nullIfEmpty(a.URL), nullIfEmpty(a.NodeID), nullIfEmpty(a.EntityID),
		a.State, a.DedupKey, nullIfEmpty(a.DetailsJSON), verification,
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

// TransitionReviewState is the review CAS: one UPDATE guarded on the current
// state, so of two racing approves exactly one sees a row flip (RowsAffected=1)
// and the loser gets false without touching anything. external_id / url /
// details_json follow Upsert's preserve-on-empty convention.
func (s *deliverableStore) TransitionReviewState(ctx context.Context, orgID, id, from, to, externalID, url, detailsJSON string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE deliverables
		SET state        = ?,
		    external_id  = COALESCE(NULLIF(?, ''), external_id),
		    url          = COALESCE(NULLIF(?, ''), url),
		    details_json = COALESCE(NULLIF(?, ''), details_json),
		    updated_at   = CURRENT_TIMESTAMP
		WHERE org_id = ? AND id = ? AND kind = ? AND state = ?
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

// TransitionReviewStateSystem is identical to TransitionReviewState in SQLite:
// local mode is single-tenant (N=1) with no RLS, so there is no admin/app pool
// split. The method exists for parity with the Postgres store, where the
// event-triggered exec writers (no JWT-claims context) need an admin-pool path.
func (s *deliverableStore) TransitionReviewStateSystem(ctx context.Context, orgID, id, from, to, externalID, url, detailsJSON string) (bool, error) {
	return s.TransitionReviewState(ctx, orgID, id, from, to, externalID, url, detailsJSON)
}

// UpdateReviewDetailsIfPending guards the draft-mutation write on the row still
// being a pending review, so a stale writer can't resurrect a resolved one.
func (s *deliverableStore) UpdateReviewDetailsIfPending(ctx context.Context, orgID, id, detailsJSON string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE deliverables
		SET details_json = ?, updated_at = CURRENT_TIMESTAMP
		WHERE org_id = ? AND id = ? AND kind = ? AND state = ?
	`, nullIfEmpty(detailsJSON), orgID, id, domain.DeliverableKindReview, domain.DeliverableStateReviewPending)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// UpdateReviewDetailsIfPendingSystem is identical to UpdateReviewDetailsIfPending
// in SQLite: local mode is single-tenant (N=1) with no RLS, so there is no
// admin/app pool split. The method exists for parity with the Postgres store,
// where the event-triggered exec writers (no JWT-claims context) need an
// admin-pool path.
func (s *deliverableStore) UpdateReviewDetailsIfPendingSystem(ctx context.Context, orgID, id, detailsJSON string) (bool, error) {
	return s.UpdateReviewDetailsIfPending(ctx, orgID, id, detailsJSON)
}

func (s *deliverableStore) Get(ctx context.Context, orgID, id string) (*domain.Deliverable, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT `+deliverableColumns+`
		FROM deliverables
		WHERE org_id = ? AND id = ?
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
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	query := `SELECT ` + deliverableColumns + `
		FROM deliverables
		WHERE org_id = ? AND conversation_id = ?
		ORDER BY created_at DESC, id DESC`
	args := []any{orgID, conversationID}
	// An unwindowed read holds every row, so its length is the total and no
	// count runs.
	windowed := opts.Limit > 0 || opts.CountOnly
	var total int
	if windowed {
		if err := s.q.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM deliverables WHERE org_id = ? AND conversation_id = ?
		`, orgID, conversationID).Scan(&total); err != nil {
			return nil, 0, err
		}
		if opts.CountOnly {
			return []domain.Deliverable{}, total, nil
		}
		query += ` LIMIT ? OFFSET ?`
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

// ListByConversationSystem is identical to ListByConversation in SQLite: local mode is
// single-tenant (N=1) with no RLS, so there is no admin/app pool split. The
// method exists for parity with the Postgres store, where the spawner's park
// check (no JWT-claims context) needs an admin-pool path.
func (s *deliverableStore) ListByConversationSystem(ctx context.Context, orgID, conversationID string) ([]domain.Deliverable, error) {
	out, _, err := s.ListByConversation(ctx, orgID, conversationID, db.Unwindowed)
	return out, err
}

// ListPendingReviewsByEntitySystem is identical to a plain org read in
// SQLite: local mode is single-tenant (N=1) with no RLS, so there is no
// admin/app pool split.
func (s *deliverableStore) ListPendingReviewsByEntitySystem(ctx context.Context, orgID, entityID string) ([]domain.Deliverable, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	if entityID == "" {
		return nil, nil
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+deliverableColumns+`
		FROM deliverables
		WHERE org_id = ? AND entity_id = ? AND provider = ? AND kind = ? AND state = ?
		ORDER BY created_at DESC, id DESC
	`, orgID, entityID, domain.ProviderGitHub, domain.DeliverableKindReview, domain.DeliverableStateReviewPending)
	if err != nil {
		return nil, err
	}
	return scanDeliverableRows(rows)
}

// presentExternalIDsQuery is PresentExternalIDsSystem's statement, binding
// org_id, scope, provider, kind and the ids as one JSON array, in that order:
// json_each reads the list from one bind, so the statement is the same for any
// number of ids.
const presentExternalIDsQuery = `
	SELECT external_id
	FROM deliverables
	WHERE org_id = ? AND scope = ? AND provider = ? AND kind = ? AND external_id IN (SELECT value FROM json_each(?))`

func (s *deliverableStore) PresentExternalIDsSystem(ctx context.Context, orgID, scope, provider, kind string, externalIDs []string) (map[string]bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	if err := db.RequireExternalObjectScope(scope); err != nil {
		return nil, err
	}
	present := map[string]bool{}
	if len(externalIDs) == 0 {
		return present, nil
	}
	list, err := json.Marshal(externalIDs)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, presentExternalIDsQuery, orgID, scope, provider, kind, string(list))
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

func (s *deliverableStore) CountByConversation(ctx context.Context, orgID string, conversationIDs []string) (map[string]int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(conversationIDs))
	if len(conversationIDs) == 0 {
		return counts, nil
	}
	// ?-placeholder IN list (SQLite has no array bind), chunked to stay
	// inside SQLite's variable limit (chunkIDs) — the batched conversation-
	// list path counts every conversation across many tasks, which can
	// exceed the limit. A NULL conversation_id never matches IN, so
	// detached deliverables are excluded for free.
	for _, chunk := range chunkIDs(conversationIDs) {
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		args = append(args, orgID)
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		rows, err := s.q.QueryContext(ctx, `
			SELECT conversation_id, COUNT(*)
			FROM deliverables
			WHERE org_id = ? AND conversation_id IN (`+strings.Join(placeholders, ", ")+`)
			GROUP BY conversation_id
		`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var conversationID string
			var n int
			if err := rows.Scan(&conversationID, &n); err != nil {
				rows.Close()
				return nil, err
			}
			counts[conversationID] = n
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return counts, nil
}

func (s *deliverableStore) ListByConversations(ctx context.Context, orgID string, conversationIDs []string) ([]domain.Deliverable, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	if len(conversationIDs) == 0 {
		return nil, nil
	}
	// ?-placeholder IN list (SQLite has no array bind), chunked to stay inside
	// SQLite's variable limit (chunkIDs). A NULL conversation_id never matches IN, so
	// detached deliverables are excluded for free. Each conversation_id is in one chunk, so
	// per-conversation grouping by the caller is unaffected by the chunk boundary.
	var arts []domain.Deliverable
	for _, chunk := range chunkIDs(conversationIDs) {
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		args = append(args, orgID)
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		rows, err := s.q.QueryContext(ctx, `
			SELECT `+deliverableColumns+`
			FROM deliverables
			WHERE org_id = ? AND conversation_id IN (`+strings.Join(placeholders, ", ")+`)
			ORDER BY created_at DESC, id DESC
		`, args...)
		if err != nil {
			return nil, err
		}
		batch, err := scanDeliverableRows(rows)
		if err != nil {
			return nil, err
		}
		arts = append(arts, batch...)
	}
	return arts, nil
}

func (s *deliverableStore) ListArtifactMatchesByTeamSystem(ctx context.Context, orgID, teamID string, entityIDs, reviewIDs []string) ([]domain.Deliverable, error) {
	return s.listArtifactMatches(ctx, orgID, `org_id = ? AND team_id = ?`, []any{orgID, teamID}, entityIDs, reviewIDs)
}

// ListArtifactMatchesByOrgSystem is the team read over every team: N=1, no
// pool split.
func (s *deliverableStore) ListArtifactMatchesByOrgSystem(ctx context.Context, orgID string, entityIDs, reviewIDs []string) ([]domain.Deliverable, error) {
	return s.listArtifactMatches(ctx, orgID, `org_id = ?`, []any{orgID}, entityIDs, reviewIDs)
}

// listArtifactMatches is both scopes' read; where and args select the scope.
// Each id set binds as one JSON array read through json_each, so neither
// needs chunking against SQLite's variable limit.
func (s *deliverableStore) listArtifactMatches(ctx context.Context, orgID, where string, args []any, entityIDs, reviewIDs []string) ([]domain.Deliverable, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	if len(entityIDs) == 0 && len(reviewIDs) == 0 {
		return nil, nil
	}
	entities, err := json.Marshal(append([]string{}, entityIDs...))
	if err != nil {
		return nil, err
	}
	reviews, err := json.Marshal(append([]string{}, reviewIDs...))
	if err != nil {
		return nil, err
	}
	args = append(args, domain.DeliverableKindPullRequest, string(entities), domain.DeliverableKindReview, string(reviews))
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+deliverableColumns+`
		FROM deliverables
		WHERE `+where+`
		  AND ((kind = ? AND entity_id IN (SELECT value FROM json_each(?)))
		    OR (kind = ? AND external_id IN (SELECT value FROM json_each(?))))
		ORDER BY created_at DESC, id DESC
	`, args...)
	if err != nil {
		return nil, err
	}
	return scanDeliverableRows(rows)
}

// ListNonTerminalBySystem is identical to a plain org read in SQLite: local
// mode is single-tenant (N=1) with no RLS, so there is no admin/app pool
// split. The method exists for parity with the Postgres store, where the
// reconciler (a background system job with no JWT-claims context) needs the
// admin pool to see every team's non-terminal deliverables. TFAC-464.
func (s *deliverableStore) ListNonTerminalBySystem(ctx context.Context, orgID string) ([]domain.Deliverable, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+deliverableColumns+`
		FROM deliverables
		WHERE org_id = ? AND `+deliverableNonTerminalPredicate+`
		ORDER BY created_at DESC, id DESC
	`, orgID)
	if err != nil {
		return nil, err
	}
	return scanDeliverableRows(rows)
}

func (s *deliverableStore) SettleVerificationSystem(ctx context.Context, orgID, id, verdict, detailsJSON string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	if err := db.RequireSettledVerification(verdict); err != nil {
		return false, err
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE deliverables
		SET verification = ?,
		    state        = CASE WHEN ? = ? THEN ? ELSE state END,
		    details_json = COALESCE(NULLIF(?, ''), details_json),
		    updated_at   = CURRENT_TIMESTAMP
		WHERE org_id = ? AND id = ? AND kind = ? AND verification = ?
	`, verdict, verdict, domain.DeliverableVerificationForeign, domain.DeliverableStatePRClosed, detailsJSON,
		orgID, id, domain.DeliverableKindPullRequest, domain.DeliverableVerificationPending)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
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

// scanDeliverable fills a from one row. rowScanner (defined in repos.go)
// unifies *sql.Row and *sql.Rows so this serves both the single-row Upsert
// RETURNING and the list paths.
func scanDeliverable(sc rowScanner, a *domain.Deliverable) error {
	var conversationID, externalID, url, nodeID, entityID, detailsJSON sql.NullString
	if err := sc.Scan(
		&a.ID, &conversationID, &a.OrgID, &a.TeamID, &a.Provider, &a.Kind, &a.Scope, &a.Target,
		&externalID, &url, &nodeID, &entityID,
		&a.State, &a.DedupKey, &detailsJSON, &a.CreatedAt, &a.UpdatedAt, &a.Verification,
	); err != nil {
		return err
	}
	a.ConversationID = conversationID.String
	a.ExternalID = externalID.String
	a.URL = url.String
	a.NodeID = nodeID.String
	a.EntityID = entityID.String
	a.DetailsJSON = detailsJSON.String
	return nil
}
