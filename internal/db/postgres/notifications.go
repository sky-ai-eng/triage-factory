package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// notificationStore: q is the app pool (reads, marks under claims); admin
// carries every write to notifications.
type notificationStore struct {
	q     queryer
	admin queryer
}

func newNotificationStore(q, admin queryer) db.NotificationStore {
	return &notificationStore{q: q, admin: admin}
}

var _ db.NotificationStore = (*notificationStore)(nil)

// pgNotificationColsTmpl is db.ScanNotification's projection; {p} is the
// table qualifier.
const pgNotificationColsTmpl = `{p}id::text, {p}org_id::text, {p}origin, {p}lifecycle, {p}kind, {p}key,
	{p}audience_scope, COALESCE({p}audience_role, ''), COALESCE({p}team_id::text, ''), COALESCE({p}user_id::text, ''),
	COALESCE({p}source, ''), {p}priority, {p}dismissible, {p}title, {p}body, {p}actions, {p}params,
	{p}first_seen_at, {p}last_seen_at, {p}seen_count, {p}escalated_at,
	{p}resolved_at, COALESCE({p}resolved_reason, ''), {p}expires_at, COALESCE({p}entity_id::text, '')`

var (
	pgNotificationCols = strings.ReplaceAll(pgNotificationColsTmpl, "{p}", "")

	pgNotificationViewCols = strings.ReplaceAll(pgNotificationColsTmpl, "{p}", "n.") + `,
	m.notification_id::text, m.user_id::text, m.org_id::text, m.read_at, m.dismissed_at,
	` + db.NotificationReadSQL + `, ` + db.NotificationDismissedSQL
)

const pgNotificationMarksCols = `notification_id::text, user_id::text, org_id::text, read_at, dismissed_at`

const pgNotificationNotExpired = `(n.expires_at IS NULL OR n.expires_at > now())`

func (s *notificationStore) UpsertOpenSystem(ctx context.Context, o db.NotificationOccurrence) (db.NotificationUpsert, error) {
	o, err := db.ValidateNotificationOccurrence(o)
	if err != nil {
		return db.NotificationUpsert{}, err
	}
	a := o.Audience
	if o.Lifecycle == domain.NotificationNotice {
		// An expired notice still holds the open-occurrence index; delete it
		// so the insert opens a new notice instead of reviving it. Statement
		// time, not now(): inside a transaction now() is its start.
		if _, err := s.admin.ExecContext(ctx, `
			DELETE FROM notifications
			WHERE org_id = $1 AND origin = $2 AND kind = $3 AND key = $4 AND audience_scope = $5
			  AND audience_role IS NOT DISTINCT FROM $6::text
			  AND team_id IS NOT DISTINCT FROM $7::uuid
			  AND user_id IS NOT DISTINCT FROM $8::uuid
			  AND resolved_at IS NULL AND expires_at <= clock_timestamp()`,
			o.OrgID, o.Origin, o.Kind, o.Key, a.Scope,
			nullIfEmpty(string(a.Role)), nullIfEmpty(a.TeamID), nullIfEmpty(a.UserID)); err != nil {
			return db.NotificationUpsert{}, err
		}
	}
	var expiresAt any
	if o.ExpiresAt != nil {
		expiresAt = *o.ExpiresAt
	}
	// One clock read per statement: every stamp the write sets is the same
	// instant (NotificationUpsertOf relies on it), and two raises in one
	// transaction get different instants, which now() would not.
	n, err := db.ScanNotification(s.admin.QueryRowContext(ctx, `
		WITH clock AS (SELECT clock_timestamp() AS at)
		INSERT INTO notifications AS n (
			org_id, origin, lifecycle, kind, key,
			audience_scope, audience_role, team_id, user_id,
			source, priority, dismissible, title, body, actions, params,
			first_seen_at, last_seen_at, seen_count, escalated_at, expires_at, entity_id)
		SELECT $1::uuid, $2::text, $3::text, $4::text, $5::text, $6::text, $7::text, $8::uuid, $9::uuid,
			$10::text, $11::text, $12::boolean, $13::text, $14::text, $15::jsonb, $16::jsonb,
			clock.at, clock.at, 1, clock.at, $17::timestamptz, $18::uuid
		FROM clock
		ON CONFLICT (org_id, origin, kind, key, audience_scope,
			COALESCE(audience_role, ''),
			COALESCE(team_id, '00000000-0000-0000-0000-000000000000'::uuid),
			COALESCE(user_id, '00000000-0000-0000-0000-000000000000'::uuid))
			WHERE resolved_at IS NULL
		DO UPDATE SET
			last_seen_at = EXCLUDED.last_seen_at,
			seen_count   = n.seen_count + 1,
			escalated_at = CASE WHEN `+db.NotificationPriorityRankSQL("EXCLUDED.priority")+` > `+db.NotificationPriorityRankSQL("n.priority")+` OR $19::boolean
			                    THEN EXCLUDED.last_seen_at ELSE n.escalated_at END,
			priority     = EXCLUDED.priority,
			dismissible  = EXCLUDED.dismissible,
			source       = EXCLUDED.source,
			title        = EXCLUDED.title,
			body         = EXCLUDED.body,
			actions      = EXCLUDED.actions,
			params       = EXCLUDED.params,
			expires_at   = EXCLUDED.expires_at
		RETURNING `+pgNotificationCols,
		o.OrgID, o.Origin, o.Lifecycle, o.Kind, o.Key,
		a.Scope, nullIfEmpty(string(a.Role)), nullIfEmpty(a.TeamID), nullIfEmpty(a.UserID),
		nullIfEmpty(o.Source), o.Priority, o.Dismissible, o.Title, o.Body, string(o.Actions), string(o.Params),
		expiresAt, nullIfEmpty(o.EntityID), o.Resurface).Scan)
	if err != nil {
		return db.NotificationUpsert{}, err
	}
	return db.NotificationUpsertOf(n), nil
}

func (s *notificationStore) ResolveSystem(ctx context.Context, orgID string, origin domain.NotificationOrigin, kind, key string) ([]domain.Notification, error) {
	return s.resolve(ctx, domain.NotificationResolvedCleared,
		`org_id = $2 AND origin = $3 AND kind = $4 AND key = $5`,
		orgID, origin, kind, key)
}

func (s *notificationStore) ResolveKeysNotInSystem(ctx context.Context, orgID string, origin domain.NotificationOrigin, kind string, keep []string) ([]domain.Notification, error) {
	if keep == nil {
		// nil binds as NULL, which would resolve nothing.
		keep = []string{}
	}
	return s.resolve(ctx, domain.NotificationResolvedCleared,
		`org_id = $2 AND origin = $3 AND kind = $4 AND NOT (key = ANY($5::text[]))`,
		orgID, origin, kind, keep)
}

func (s *notificationStore) ResolveAudiencesNotInSystem(ctx context.Context, orgID string, origin domain.NotificationOrigin, kind, key string, keep []domain.NotificationAudience) ([]domain.Notification, error) {
	audiences, err := db.NotificationAudiencesJSON(keep)
	if err != nil {
		return nil, err
	}
	return s.resolve(ctx, domain.NotificationResolvedAudienceRemoved, `
		org_id = $2 AND origin = $3 AND kind = $4 AND key = $5
		AND NOT EXISTS (
			SELECT 1 FROM jsonb_to_recordset($6::jsonb) AS k(scope text, role text, team_id uuid, user_id uuid)
			WHERE k.scope = notifications.audience_scope
			  AND k.role IS NOT DISTINCT FROM notifications.audience_role
			  AND k.team_id IS NOT DISTINCT FROM notifications.team_id
			  AND k.user_id IS NOT DISTINCT FROM notifications.user_id)`,
		orgID, origin, kind, key, audiences)
}

func (s *notificationStore) ResolveSourceSystem(ctx context.Context, orgID, source string) ([]domain.Notification, error) {
	return s.resolve(ctx, domain.NotificationResolvedSourceOff, `org_id = $2 AND source = $3`, orgID, source)
}

func (s *notificationStore) ResolveClosedEntityEventsSystem(ctx context.Context, orgID string) ([]domain.Notification, error) {
	return s.resolve(ctx, domain.NotificationResolvedCleared, `
		org_id = $2 AND origin = 'event' AND entity_id IS NOT NULL
		AND EXISTS (
			SELECT 1 FROM entities e
			WHERE e.id = notifications.entity_id AND e.org_id = notifications.org_id AND e.state <> 'active')`,
		orgID)
}

func (s *notificationStore) ResolveIDSystem(ctx context.Context, orgID, id string) (*domain.Notification, error) {
	if !isValidUUID(id) {
		return nil, nil
	}
	got, err := s.resolve(ctx, domain.NotificationResolvedCleared, `org_id = $2 AND id = $3::uuid`, orgID, id)
	if err != nil || len(got) == 0 {
		return nil, err
	}
	return &got[0], nil
}

// resolve resolves the open conditions where selects; $1 is the reason.
func (s *notificationStore) resolve(ctx context.Context, reason domain.NotificationResolvedReason, where string, args ...any) ([]domain.Notification, error) {
	rows, err := s.admin.QueryContext(ctx, `
		UPDATE notifications SET resolved_at = now(), resolved_reason = $1
		WHERE `+where+` AND lifecycle = 'condition' AND resolved_at IS NULL
		RETURNING `+pgNotificationCols,
		append([]any{reason}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Notification{}
	for rows.Next() {
		n, err := db.ScanNotification(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// OpenConditionOrgsSystem reads notifications_open_kind_idx, which holds open
// conditions alone, so its cost is the kind's open rows rather than every
// open row in the deployment.
func (s *notificationStore) OpenConditionOrgsSystem(ctx context.Context, origin domain.NotificationOrigin, kind string) ([]string, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT DISTINCT org_id::text FROM notifications
		WHERE kind = $1 AND origin = $2 AND lifecycle = 'condition' AND resolved_at IS NULL
		ORDER BY 1`, kind, origin)
	if err != nil {
		return nil, fmt.Errorf("list orgs with an open condition: %w", err)
	}
	return scanIDs(rows, "org with an open condition")
}

// OpenEventKeysSystem reads notifications_open_entity_idx.
func (s *notificationStore) OpenEventKeysSystem(ctx context.Context, orgID, entityID string, kinds []string) ([]db.NotificationKindKey, error) {
	out := []db.NotificationKindKey{}
	if len(kinds) == 0 {
		return out, nil
	}
	rows, err := s.admin.QueryContext(ctx, `
		SELECT DISTINCT kind, key FROM notifications
		WHERE org_id = $1 AND entity_id = $2::uuid AND kind = ANY($3::text[])
		  AND origin = 'event' AND lifecycle = 'condition' AND resolved_at IS NULL
		ORDER BY kind, key`, orgID, entityID, kinds)
	if err != nil {
		return nil, fmt.Errorf("list open event notifications: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k db.NotificationKindKey
		if err := rows.Scan(&k.Kind, &k.Key); err != nil {
			return nil, fmt.Errorf("scan open event notification: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RecipientsSystem reads pgNotificationSent the other way round: the
// memberships it tests, listed.
func (s *notificationStore) RecipientsSystem(ctx context.Context, orgID string, a domain.NotificationAudience) ([]string, error) {
	if err := a.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", db.ErrInvalidNotification, err)
	}
	var (
		query string
		args  []any
	)
	switch a.Scope {
	case domain.NotificationScopeOrg:
		query = `SELECT DISTINCT om.user_id::text FROM org_memberships om
			WHERE om.org_id = $1 AND ($2 = 'members' OR om.role IN ('owner', 'admin'))
			ORDER BY 1`
		args = []any{orgID, a.Role}
	case domain.NotificationScopeTeam:
		query = `SELECT DISTINCT tm.user_id::text FROM memberships tm JOIN teams t ON t.id = tm.team_id
			WHERE t.org_id = $1 AND tm.team_id = $2::uuid AND t.deleted_at IS NULL
			  AND ($3 = 'members' OR tm.role = 'admin')
			ORDER BY 1`
		args = []any{orgID, a.TeamID, a.Role}
	default:
		return []string{a.UserID}, nil
	}
	rows, err := s.admin.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list notification recipients: %w", err)
	}
	return scanIDs(rows, "notification recipient")
}

func (s *notificationStore) DeleteRetiredSystem(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.admin.ExecContext(ctx, `
		DELETE FROM notifications
		WHERE (resolved_at IS NOT NULL AND resolved_at < $1)
		   OR (expires_at IS NOT NULL AND expires_at <= $2)`,
		now.UTC().Add(-db.NotificationResolvedRetention), now.UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// pgNotificationSent is "is sent" for the viewer at $2. Narrower than
// notifications_select: org admins get no read-through on team rows, and
// archived teams' rows go to nobody.
const pgNotificationSent = `CASE n.audience_scope
	WHEN 'org' THEN EXISTS (
		SELECT 1 FROM org_memberships om
		WHERE om.org_id = n.org_id AND om.user_id = $2::uuid
		  AND (n.audience_role = 'members' OR om.role IN ('owner', 'admin')))
	WHEN 'team' THEN EXISTS (
		SELECT 1 FROM memberships tm JOIN teams t ON t.id = tm.team_id
		WHERE tm.team_id = n.team_id AND tm.user_id = $2::uuid AND t.deleted_at IS NULL
		  AND (n.audience_role = 'members' OR tm.role = 'admin'))
	ELSE n.user_id = $2::uuid
END`

// pgNotificationReadable narrows notifications_select to the org at $1,
// unexpired, and no other user's row.
const pgNotificationReadable = `n.org_id = $1 AND ` + pgNotificationNotExpired + `
	AND (n.audience_scope <> 'user' OR n.user_id = $2::uuid)`

func (s *notificationStore) ListForViewer(ctx context.Context, orgID, userID string, f db.NotificationListFilter, opts db.ListOpts) ([]domain.NotificationView, int, error) {
	if err := f.Validate(); err != nil {
		return nil, 0, err
	}
	where, args := pgNotificationFilters(`n.org_id = $1 AND `+pgNotificationNotExpired+` AND `+pgNotificationSent,
		[]any{orgID, userID}, f)
	return s.list(ctx, where, args, opts)
}

func (s *notificationStore) ListForOrg(ctx context.Context, orgID, userID string, f db.NotificationOrgListFilter, opts db.ListOpts) ([]domain.NotificationView, int, error) {
	if err := f.Validate(); err != nil {
		return nil, 0, err
	}
	where, args := pgNotificationFilters(pgNotificationReadable, []any{orgID, userID}, f.NotificationListFilter)
	if len(f.TeamIDIn) > 0 {
		// Dropping a malformed id narrows the filter; it never widens it.
		args = append(args, pgUUIDArray(filterValidUUIDs(f.TeamIDIn)))
		where += fmt.Sprintf(` AND n.team_id = ANY($%d::uuid[])`, len(args))
	}
	return s.list(ctx, where, args, opts)
}

func pgNotificationFilters(base string, args []any, f db.NotificationListFilter) (string, []any) {
	clauses := append([]string{base}, db.NotificationStateClauses(f)...)
	if len(f.PriorityIn) > 0 {
		vals := make([]string, len(f.PriorityIn))
		for i, p := range f.PriorityIn {
			vals[i] = string(p)
		}
		args = append(args, vals)
		clauses = append(clauses, fmt.Sprintf(`n.priority = ANY($%d::text[])`, len(args)))
	}
	if len(f.ScopeIn) > 0 {
		vals := make([]string, len(f.ScopeIn))
		for i, sc := range f.ScopeIn {
			vals[i] = string(sc)
		}
		args = append(args, vals)
		clauses = append(clauses, fmt.Sprintf(`n.audience_scope = ANY($%d::text[])`, len(args)))
	}
	return strings.Join(clauses, " AND "), args
}

// list counts and pages where over the same join to the viewer's marks.
func (s *notificationStore) list(ctx context.Context, where string, args []any, opts db.ListOpts) ([]domain.NotificationView, int, error) {
	if err := db.RefuseNotificationKeyset(opts); err != nil {
		return nil, 0, err
	}
	const from = ` FROM notifications n
		LEFT JOIN notification_marks m ON m.notification_id = n.id AND m.user_id = $2::uuid
		WHERE `
	var total int
	if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*)`+from+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []domain.NotificationView{}, total, nil
	}
	query := `SELECT ` + pgNotificationViewCols + from + where + db.NotificationOrderSQL()
	if opts.Limit > 0 {
		args = append(args, opts.Limit, opts.Offset)
		query += fmt.Sprintf(` LIMIT $%d OFFSET $%d`, len(args)-1, len(args))
	}
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []domain.NotificationView{}
	for rows.Next() {
		v, err := db.ScanNotificationView(rows.Scan)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, v)
	}
	return out, total, rows.Err()
}

func (s *notificationStore) GetForViewer(ctx context.Context, orgID, userID, id string) (*domain.NotificationView, error) {
	if !isValidUUID(id) {
		return nil, nil
	}
	v, err := db.ScanNotificationView(s.q.QueryRowContext(ctx, `
		SELECT `+pgNotificationViewCols+`
		FROM notifications n
		LEFT JOIN notification_marks m ON m.notification_id = n.id AND m.user_id = $2::uuid
		WHERE `+pgNotificationReadable+` AND n.id = $3::uuid`,
		orgID, userID, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (s *notificationStore) SetMarks(ctx context.Context, orgID, userID, id string, p db.NotificationMarksPatch) (domain.NotificationMarks, error) {
	if err := db.ValidateNotificationMarksPatch(p); err != nil {
		return domain.NotificationMarks{}, err
	}
	if !isValidUUID(id) {
		return domain.NotificationMarks{}, db.ErrNoSuchNotification
	}
	readSet, readOn := p.Read != nil, p.Read != nil && *p.Read
	dismissSet, dismissOn := p.Dismissed != nil, p.Dismissed != nil && *p.Dismissed
	m, err := db.ScanNotificationMarks(s.q.QueryRowContext(ctx, `
		INSERT INTO notification_marks AS m (notification_id, user_id, org_id, read_at, dismissed_at)
		SELECT n.id, $2::uuid, n.org_id,
		       CASE WHEN $4::boolean THEN clock_timestamp() END,
		       CASE WHEN $6::boolean THEN clock_timestamp() END
		FROM notifications n
		WHERE `+pgNotificationReadable+` AND n.id = $3::uuid
		  AND (n.dismissible OR NOT $6::boolean)
		ON CONFLICT (notification_id, user_id) DO UPDATE SET
			read_at      = CASE WHEN $5::boolean THEN EXCLUDED.read_at ELSE m.read_at END,
			dismissed_at = CASE WHEN $7::boolean THEN EXCLUDED.dismissed_at ELSE m.dismissed_at END
		RETURNING `+pgNotificationMarksCols,
		orgID, userID, id, readOn, readSet, dismissOn, dismissSet).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.NotificationMarks{}, s.marksMiss(ctx, orgID, userID, id, dismissOn)
	}
	return m, err
}

func (s *notificationStore) marksMiss(ctx context.Context, orgID, userID, id string, dismissing bool) error {
	var dismissible bool
	err := s.q.QueryRowContext(ctx, `
		SELECT n.dismissible FROM notifications n
		WHERE `+pgNotificationReadable+` AND n.id = $3::uuid`,
		orgID, userID, id).Scan(&dismissible)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return db.ErrNoSuchNotification
	case err != nil:
		return err
	case dismissing && !dismissible:
		return db.ErrNotificationNotDismissible
	}
	return db.ErrNoSuchNotification
}

func (s *notificationStore) MarkRead(ctx context.Context, orgID, userID string, ids []string) ([]domain.NotificationMarks, error) {
	out := []domain.NotificationMarks{}
	ids = filterValidUUIDs(ids)
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.q.QueryContext(ctx, `
		INSERT INTO notification_marks AS m (notification_id, user_id, org_id, read_at)
		SELECT n.id, $2::uuid, n.org_id, clock_timestamp()
		FROM notifications n
		WHERE `+pgNotificationReadable+` AND n.id = ANY($3::uuid[])
		ON CONFLICT (notification_id, user_id) DO UPDATE SET read_at = EXCLUDED.read_at
		RETURNING `+pgNotificationMarksCols,
		orgID, userID, pgUUIDArray(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		m, err := db.ScanNotificationMarks(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
