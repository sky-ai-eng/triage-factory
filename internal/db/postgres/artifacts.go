package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// artifactStore is the Postgres impl of db.ArtifactStore: reads over
// external_actions grouped by pgActionObjectIDSQL. q is the app pool (or the
// claims tx), where external_actions_all scopes by org; admin is the admin
// pool for the ...System reads. org_id stays bound in every statement.
type artifactStore struct {
	q     queryer
	admin queryer
}

func newArtifactStore(q, admin queryer) db.ArtifactStore {
	return &artifactStore{q: q, admin: admin}
}

var _ db.ArtifactStore = (*artifactStore)(nil)

// pgArtifactRowFilter is what every artifact read adds to its own WHERE: the
// rows of an artifact performer that name an object, of an action that can
// name one.
var pgArtifactRowFilter = ` AND ` + db.ArtifactRowSQL() + ` AND (` + pgActionObjectIDSQL + `) IS NOT NULL`

// pgArtifactCTEs opens the grouped read over the rows where selects: scoped
// holds the rows with their object id, and grouped ranks each object's rows.
// Each group's latest row (greatest occurred_at, id) supplies the action and
// the target; the window aggregates supply the count, the earliest
// occurred_at, and for the display rule (pgArtifactColumns) the latest url
// any row carries and the entity of the latest row that names one.
func pgArtifactCTEs(where string) string {
	return `
	WITH scoped AS (
		SELECT ea.id, ea.org_id, ea.provider, ea.scope, ea.action, ea.target,
		       COALESCE(ea.current_url, ea.url, '') AS url, ea.entity_id, ea.repository_id,
		       ea.object_kind, ea.object_key, ea.occurred_at, ea.conversation_id, ea.team_id,
		       ` + pgActionObjectIDSQL + ` AS object_id
		  FROM external_actions ea
		 WHERE ` + where + pgArtifactRowFilter + `
	), grouped AS (
		SELECT s.*,
		       ROW_NUMBER() OVER (PARTITION BY s.object_id ORDER BY s.occurred_at DESC, s.id DESC) AS rn,
		       COUNT(*) OVER (PARTITION BY s.object_id) AS action_count,
		       MIN(s.occurred_at) OVER (PARTITION BY s.object_id) AS first_at,
		       FIRST_VALUE(s.url) OVER (PARTITION BY s.object_id
		           ORDER BY s.url = '', s.occurred_at DESC, s.id DESC) AS display_url,
		       FIRST_VALUE(s.entity_id) OVER (PARTITION BY s.object_id
		           ORDER BY s.entity_id IS NULL, s.occurred_at DESC, s.id DESC) AS display_entity_id
		  FROM scoped s
	)`
}

// pgArtifactColumns is what scanPgArtifacts reads, off the latest row g,
// except the entity id, which is the entity the display values are read
// from. The entity's key wins over the row's target, and for a pull request
// or an issue, which the entity is, its url wins over the rows'; the row's
// values stand in when the entity is not readable or has no url yet.
const pgArtifactColumns = `g.object_id, g.object_kind, g.provider, g.scope,
	       COALESCE(g.display_entity_id::text, ''), COALESCE(g.repository_id::text, ''), COALESCE(g.object_key, ''),
	       COALESCE(e.source_id, g.target),
	       CASE WHEN g.object_kind IN ('pull_request', 'issue') THEN COALESCE(NULLIF(e.url, ''), g.display_url)
	            ELSE g.display_url END,
	       g.action, g.action_count, g.first_at, g.occurred_at`

// pgArtifactEntityJoin joins the entity the group's rows name, for a kind
// that is one or lives on one: a pull request or an issue, and a comment,
// review or message, whose rows name the entity it is on. A comment's edit
// and reaction rows name only the repository, so the entity is the latest
// row's that names one.
const pgArtifactEntityJoin = `
	  LEFT JOIN entities e ON e.id = g.display_entity_id AND e.org_id = g.org_id
	       AND g.object_kind IN ('pull_request', 'issue', 'comment', 'review', 'message')`

// pgArtifactSelect is the grouped read over the rows where selects, ending
// in a WHERE the caller may extend with ORDER BY and a window.
func pgArtifactSelect(where string) string {
	return pgArtifactCTEs(where) + `
	SELECT ` + pgArtifactColumns + `
	  FROM grouped g` + pgArtifactEntityJoin + `
	 WHERE g.rn = 1`
}

// pgObjectActivitySelect is pgArtifactSelect plus each object's distinct
// conversations and teams, and the number of objects in all. Neither dialect
// takes DISTINCT inside a window aggregate, so spread counts them with a
// GROUP BY and joins back on the object id. Team ids are uuids, so a comma
// joins them unambiguously. The total is a window over the latest rows, which
// is evaluated before any LIMIT, so the page carries it.
func pgObjectActivitySelect(where string) string {
	return pgArtifactCTEs(where) + `, spread AS (
		SELECT s.object_id, COUNT(DISTINCT s.conversation_id) AS conversation_count,
		       COALESCE(string_agg(DISTINCT s.team_id::text, ','), '') AS team_ids
		  FROM scoped s
		 GROUP BY s.object_id
	)
	SELECT ` + pgArtifactColumns + `, sp.conversation_count, sp.team_ids, COUNT(*) OVER () AS total
	  FROM grouped g
	  JOIN spread sp ON sp.object_id = g.object_id` + pgArtifactEntityJoin + `
	 WHERE g.rn = 1`
}

// pgArtifactObjectIDOrder orders object ids bytewise, as SQLite's BINARY
// collation does, so the tiebreaker is the same in both dialects.
const pgArtifactObjectIDOrder = `g.object_id COLLATE "C"`

func (s *artifactStore) ListByConversation(ctx context.Context, orgID, conversationID string, opts db.ListOpts) ([]domain.Artifact, int, error) {
	return listPgArtifactsByConversation(ctx, s.q, orgID, conversationID, opts)
}

func (s *artifactStore) ListByConversationSystem(ctx context.Context, orgID, conversationID string, opts db.ListOpts) ([]domain.Artifact, int, error) {
	return listPgArtifactsByConversation(ctx, s.admin, orgID, conversationID, opts)
}

// listPgArtifactsByConversation is both pools' ListByConversation. The
// conversation_id bind casts to uuid so a malformed id is a query error
// rather than a text comparison against a uuid column. An unwindowed read
// holds every group, so its length is the total and no count runs.
func listPgArtifactsByConversation(ctx context.Context, q queryer, orgID, conversationID string, opts db.ListOpts) ([]domain.Artifact, int, error) {
	if err := db.RequireArtifactOffsetPage(opts); err != nil {
		return nil, 0, err
	}
	const where = `ea.org_id = $1 AND ea.conversation_id = $2::uuid`
	args := []any{orgID, conversationID}
	windowed := opts.Limit > 0 || opts.CountOnly
	var total int
	if windowed {
		if err := q.QueryRowContext(ctx, `
			SELECT COUNT(DISTINCT `+pgActionObjectIDSQL+`)
			  FROM external_actions ea
			 WHERE `+where+pgArtifactRowFilter, args...).Scan(&total); err != nil {
			return nil, 0, err
		}
		if opts.CountOnly {
			return []domain.Artifact{}, total, nil
		}
	}
	query := pgArtifactSelect(where) + ` ORDER BY g.occurred_at DESC, ` + pgArtifactObjectIDOrder
	if opts.Limit > 0 {
		args = append(args, opts.Limit, opts.Offset)
		query += fmt.Sprintf(` LIMIT $%d OFFSET $%d`, len(args)-1, len(args))
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	out, err := scanPgArtifacts(rows)
	if err != nil {
		return nil, 0, err
	}
	if !windowed {
		total = len(out)
	}
	return out, total, nil
}

// CountByConversations counts each conversation's distinct object ids per
// kind. Every object id starts with its kind, so grouping by object_kind
// splits no artifact. The ids go through pgUUIDArray, as the deliverable
// store's batch reads do.
func (s *artifactStore) CountByConversations(ctx context.Context, orgID string, conversationIDs []string) (map[string]map[string]int, error) {
	counts := make(map[string]map[string]int, len(conversationIDs))
	if len(conversationIDs) == 0 {
		return counts, nil
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT ea.conversation_id::text, ea.object_kind, COUNT(DISTINCT `+pgActionObjectIDSQL+`)
		  FROM external_actions ea
		 WHERE ea.org_id = $1 AND ea.conversation_id = ANY($2)`+pgArtifactRowFilter+`
		 GROUP BY ea.conversation_id, ea.object_kind
	`, orgID, pgUUIDArray(conversationIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var conversationID, kind string
		var n int
		if err := rows.Scan(&conversationID, &kind, &n); err != nil {
			return nil, err
		}
		if counts[conversationID] == nil {
			counts[conversationID] = map[string]int{}
		}
		counts[conversationID][kind] = n
	}
	return counts, rows.Err()
}

func (s *artifactStore) ExistsForConversationSystem(ctx context.Context, orgID, conversationID string) (bool, error) {
	var exists bool
	err := s.admin.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM external_actions ea
			 WHERE ea.org_id = $1 AND ea.conversation_id = $2::uuid`+pgArtifactRowFilter+`
		)`, orgID, conversationID).Scan(&exists)
	return exists, err
}

func (s *artifactStore) ListForConversationsSystem(ctx context.Context, orgID string, conversationIDs []string) ([]domain.Artifact, error) {
	if len(conversationIDs) == 0 {
		return nil, nil
	}
	rows, err := s.admin.QueryContext(ctx,
		pgArtifactSelect(`ea.org_id = $1 AND ea.conversation_id = ANY($2)`)+
			` ORDER BY g.first_at, `+pgArtifactObjectIDOrder,
		orgID, pgUUIDArray(conversationIDs))
	if err != nil {
		return nil, err
	}
	return scanPgArtifacts(rows)
}

func (s *artifactStore) ListByTeam(ctx context.Context, orgID, teamID string, opts db.ArtifactListOpts) ([]domain.ObjectActivity, int, error) {
	return listPgObjectActivity(ctx, s.q, `ea.org_id = $1 AND ea.team_id = $2::uuid`, []any{orgID, teamID}, opts)
}

func (s *artifactStore) ListByOrgSystem(ctx context.Context, orgID string, opts db.ArtifactListOpts) ([]domain.ObjectActivity, int, error) {
	return listPgObjectActivity(ctx, s.admin, `ea.org_id = $1`, []any{orgID}, opts)
}

// listPgObjectActivity is both scopes' read: base and args select the scope's
// rows, opts' filters narrow them, and the page is of objects.
func listPgObjectActivity(ctx context.Context, q queryer, base string, args []any, opts db.ArtifactListOpts) ([]domain.ObjectActivity, int, error) {
	if err := db.RequireArtifactOffsetPage(opts.ListOpts); err != nil {
		return nil, 0, err
	}
	where, args := pgArtifactListWhere(base, args, opts)
	if opts.CountOnly {
		total, err := countPgObjectActivity(ctx, q, where, args)
		return []domain.ObjectActivity{}, total, err
	}
	query := pgObjectActivitySelect(where) + ` ORDER BY g.occurred_at DESC, ` + pgArtifactObjectIDOrder
	pageArgs := args
	if opts.Limit > 0 {
		pageArgs = append(slices.Clip(args), opts.Limit, opts.Offset)
		query += fmt.Sprintf(` LIMIT $%d OFFSET $%d`, len(pageArgs)-1, len(pageArgs))
	}
	out, total, err := scanPgObjectActivity(ctx, q, query, pageArgs)
	if err != nil {
		return nil, 0, err
	}
	// A page past the last object has no row to carry the total.
	if len(out) == 0 && opts.Offset > 0 {
		if total, err = countPgObjectActivity(ctx, q, where, args); err != nil {
			return nil, 0, err
		}
	}
	return out, total, nil
}

// scanPgObjectActivity runs a pgObjectActivitySelect page and reads its items
// and the total the rows carry.
func scanPgObjectActivity(ctx context.Context, q queryer, query string, args []any) ([]domain.ObjectActivity, int, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []domain.ObjectActivity{}
	var total int
	for rows.Next() {
		var (
			a       domain.ObjectActivity
			teamIDs string
		)
		if err := rows.Scan(
			&a.ObjectID, &a.ObjectKind, &a.Provider, &a.Scope,
			&a.EntityID, &a.RepositoryID, &a.ObjectKey,
			&a.Target, &a.URL,
			&a.LastAction, &a.ActionCount, &a.FirstAt, &a.LastAt,
			&a.ConversationCount, &teamIDs, &total,
		); err != nil {
			return nil, 0, err
		}
		a.TeamIDs = db.SplitArtifactTeamIDs(teamIDs)
		out = append(out, a)
	}
	return out, total, rows.Err()
}

// countPgObjectActivity counts the objects the rows where selects name.
func countPgObjectActivity(ctx context.Context, q queryer, where string, args []any) (int, error) {
	var total int
	err := q.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT `+pgActionObjectIDSQL+`)
		  FROM external_actions ea
		 WHERE `+where+pgArtifactRowFilter, args...).Scan(&total)
	return total, err
}

// pgArtifactListWhere appends opts' filters to base, numbering each bind
// after args.
func pgArtifactListWhere(base string, args []any, opts db.ArtifactListOpts) (string, []any) {
	where := base
	add := func(predicate string, v any) {
		args = append(args, v)
		where += fmt.Sprintf(predicate, len(args))
	}
	if opts.Provider != "" {
		add(` AND ea.provider = $%d`, opts.Provider)
	}
	if opts.ObjectKind != "" {
		add(` AND ea.object_kind = $%d`, opts.ObjectKind)
	}
	if !opts.Since.IsZero() {
		add(` AND ea.occurred_at >= $%d`, opts.Since)
	}
	if !opts.Until.IsZero() {
		add(` AND ea.occurred_at < $%d`, opts.Until)
	}
	return where, args
}

func scanPgArtifacts(rows *sql.Rows) ([]domain.Artifact, error) {
	defer rows.Close()
	out := []domain.Artifact{}
	for rows.Next() {
		var a domain.Artifact
		if err := rows.Scan(
			&a.ObjectID, &a.ObjectKind, &a.Provider, &a.Scope,
			&a.EntityID, &a.RepositoryID, &a.ObjectKey,
			&a.Target, &a.URL,
			&a.LastAction, &a.ActionCount, &a.FirstAt, &a.LastAt,
		); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
