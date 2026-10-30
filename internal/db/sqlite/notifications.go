package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// notificationStore: N=1, so org and team rows are sent to the local user
// without a membership. Timestamps are bound from Go in UTC so they compare
// as text.
type notificationStore struct{ q queryer }

func newNotificationStore(q queryer) db.NotificationStore {
	return &notificationStore{q: q}
}

var _ db.NotificationStore = (*notificationStore)(nil)

// sqliteNotificationColsTmpl is db.ScanNotification's projection; {p} is the
// table qualifier (RETURNING takes it unqualified).
const sqliteNotificationColsTmpl = `{p}id, {p}org_id, {p}origin, {p}lifecycle, {p}kind, {p}key,
	{p}audience_scope, COALESCE({p}audience_role, ''), COALESCE({p}team_id, ''), COALESCE({p}user_id, ''),
	COALESCE({p}source, ''), {p}priority, {p}dismissible, {p}title, {p}body, {p}actions, {p}params,
	{p}first_seen_at, {p}last_seen_at, {p}seen_count, {p}escalated_at,
	{p}resolved_at, COALESCE({p}resolved_reason, ''), {p}expires_at`

var (
	sqliteNotificationCols = strings.ReplaceAll(sqliteNotificationColsTmpl, "{p}", "")

	sqliteNotificationViewCols = strings.ReplaceAll(sqliteNotificationColsTmpl, "{p}", "n.") + `,
	m.notification_id, m.user_id, m.org_id, m.read_at, m.dismissed_at,
	` + db.NotificationReadSQL + `, ` + db.NotificationDismissedSQL
)

const sqliteNotificationMarksCols = `notification_id, user_id, org_id, read_at, dismissed_at`

// sqliteNotificationReadable binds org, now, viewer.
const sqliteNotificationReadable = `n.org_id = ? AND (n.expires_at IS NULL OR n.expires_at > ?)
	AND (n.audience_scope <> 'user' OR n.user_id = ?)`

func (s *notificationStore) UpsertOpenSystem(ctx context.Context, o db.NotificationOccurrence) (db.NotificationUpsert, error) {
	o, err := db.ValidateNotificationOccurrence(o)
	if err != nil {
		return db.NotificationUpsert{}, err
	}
	if err := assertLocalOrg(o.OrgID); err != nil {
		return db.NotificationUpsert{}, err
	}
	a := o.Audience
	now := time.Now().UTC()
	if o.Lifecycle == domain.NotificationNotice {
		// An expired notice still holds the open-occurrence index.
		if _, err := s.q.ExecContext(ctx, `
			DELETE FROM notifications
			WHERE org_id = ? AND origin = ? AND kind = ? AND key = ? AND audience_scope = ?
			  AND audience_role IS ? AND team_id IS ? AND user_id IS ?
			  AND resolved_at IS NULL AND expires_at <= ?`,
			o.OrgID, o.Origin, o.Kind, o.Key, a.Scope,
			nullIfEmpty(string(a.Role)), nullIfEmpty(a.TeamID), nullIfEmpty(a.UserID), now); err != nil {
			return db.NotificationUpsert{}, err
		}
	}
	var expiresAt any
	if o.ExpiresAt != nil {
		expiresAt = *o.ExpiresAt
	}
	n, err := db.ScanNotification(s.q.QueryRowContext(ctx, `
		INSERT INTO notifications AS n (
			id, org_id, origin, lifecycle, kind, key,
			audience_scope, audience_role, team_id, user_id,
			source, priority, dismissible, title, body, actions, params,
			first_seen_at, last_seen_at, seen_count, escalated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT (org_id, origin, kind, key, audience_scope,
			coalesce(audience_role, ''), coalesce(team_id, ''), coalesce(user_id, ''))
			WHERE resolved_at IS NULL
		DO UPDATE SET
			last_seen_at = excluded.last_seen_at,
			seen_count   = n.seen_count + 1,
			escalated_at = CASE WHEN `+db.NotificationPriorityRankSQL("excluded.priority")+` > `+db.NotificationPriorityRankSQL("n.priority")+`
			                    THEN excluded.last_seen_at ELSE n.escalated_at END,
			priority     = excluded.priority,
			dismissible  = excluded.dismissible,
			source       = excluded.source,
			title        = excluded.title,
			body         = excluded.body,
			actions      = excluded.actions,
			params       = excluded.params,
			expires_at   = excluded.expires_at
		RETURNING `+sqliteNotificationCols,
		uuid.New().String(), o.OrgID, o.Origin, o.Lifecycle, o.Kind, o.Key,
		a.Scope, nullIfEmpty(string(a.Role)), nullIfEmpty(a.TeamID), nullIfEmpty(a.UserID),
		nullIfEmpty(o.Source), o.Priority, o.Dismissible, o.Title, o.Body, string(o.Actions), string(o.Params),
		now, now, now, expiresAt).Scan)
	if err != nil {
		return db.NotificationUpsert{}, err
	}
	return db.NotificationUpsertOf(n), nil
}

func (s *notificationStore) ResolveSystem(ctx context.Context, orgID string, origin domain.NotificationOrigin, kind, key string) ([]domain.Notification, error) {
	return s.resolve(ctx, orgID, domain.NotificationResolvedCleared,
		`origin = ? AND kind = ? AND key = ?`, origin, kind, key)
}

func (s *notificationStore) ResolveKeysNotInSystem(ctx context.Context, orgID string, origin domain.NotificationOrigin, kind string, keep []string) ([]domain.Notification, error) {
	if keep == nil {
		keep = []string{}
	}
	keys, err := json.Marshal(keep)
	if err != nil {
		return nil, err
	}
	return s.resolve(ctx, orgID, domain.NotificationResolvedCleared,
		`origin = ? AND kind = ? AND key NOT IN (SELECT value FROM json_each(?))`, origin, kind, string(keys))
}

func (s *notificationStore) ResolveAudiencesNotInSystem(ctx context.Context, orgID string, origin domain.NotificationOrigin, kind, key string, keep []domain.NotificationAudience) ([]domain.Notification, error) {
	audiences, err := db.NotificationAudiencesJSON(keep)
	if err != nil {
		return nil, err
	}
	return s.resolve(ctx, orgID, domain.NotificationResolvedAudienceRemoved, `
		origin = ? AND kind = ? AND key = ?
		AND NOT EXISTS (
			SELECT 1 FROM json_each(?) k
			WHERE json_extract(k.value, '$.scope') = notifications.audience_scope
			  AND json_extract(k.value, '$.role') IS notifications.audience_role
			  AND json_extract(k.value, '$.team_id') IS notifications.team_id
			  AND json_extract(k.value, '$.user_id') IS notifications.user_id)`,
		origin, kind, key, audiences)
}

func (s *notificationStore) ResolveSourceSystem(ctx context.Context, orgID, source string) ([]domain.Notification, error) {
	return s.resolve(ctx, orgID, domain.NotificationResolvedSourceOff, `source = ?`, source)
}

func (s *notificationStore) resolve(ctx context.Context, orgID string, reason domain.NotificationResolvedReason, where string, args ...any) ([]domain.Notification, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		UPDATE notifications SET resolved_at = ?, resolved_reason = ?
		WHERE org_id = ? AND `+where+` AND lifecycle = 'condition' AND resolved_at IS NULL
		RETURNING `+sqliteNotificationCols,
		append([]any{time.Now().UTC(), reason, orgID}, args...)...)
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

func (s *notificationStore) DeleteRetiredSystem(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.q.ExecContext(ctx, `
		DELETE FROM notifications
		WHERE (resolved_at IS NOT NULL AND resolved_at < ?)
		   OR (expires_at IS NOT NULL AND expires_at <= ?)`,
		now.UTC().Add(-db.NotificationResolvedRetention), now.UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *notificationStore) ListForViewer(ctx context.Context, orgID, userID string, f db.NotificationListFilter, opts db.ListOpts) ([]domain.NotificationView, int, error) {
	if err := f.Validate(); err != nil {
		return nil, 0, err
	}
	// N=1: "is sent" is "may read" minus archived teams.
	where, args, err := sqliteNotificationFilters(sqliteNotificationReadable+`
		AND (n.audience_scope <> 'team'
		     OR EXISTS (SELECT 1 FROM teams t WHERE t.id = n.team_id AND t.deleted_at IS NULL))`,
		[]any{orgID, time.Now().UTC(), userID}, f)
	if err != nil {
		return nil, 0, err
	}
	return s.list(ctx, orgID, userID, where, args, opts)
}

func (s *notificationStore) ListForOrg(ctx context.Context, orgID, userID string, f db.NotificationOrgListFilter, opts db.ListOpts) ([]domain.NotificationView, int, error) {
	if err := f.Validate(); err != nil {
		return nil, 0, err
	}
	where, args, err := sqliteNotificationFilters(sqliteNotificationReadable,
		[]any{orgID, time.Now().UTC(), userID}, f.NotificationListFilter)
	if err != nil {
		return nil, 0, err
	}
	if len(f.TeamIDIn) > 0 {
		teams, err := json.Marshal(f.TeamIDIn)
		if err != nil {
			return nil, 0, err
		}
		where += ` AND n.team_id IN (SELECT value FROM json_each(?))`
		args = append(args, string(teams))
	}
	return s.list(ctx, orgID, userID, where, args, opts)
}

func sqliteNotificationFilters(base string, args []any, f db.NotificationListFilter) (string, []any, error) {
	clauses := []string{base}
	mark := func(want bool, expr string) string {
		if want {
			return expr
		}
		return `NOT ` + expr
	}
	switch f.Status {
	case domain.NotificationOpen:
		clauses = append(clauses, `n.resolved_at IS NULL`)
	case domain.NotificationResolved:
		clauses = append(clauses, `n.resolved_at IS NOT NULL`)
	}
	if f.Dismissed != nil {
		clauses = append(clauses, mark(*f.Dismissed, db.NotificationDismissedSQL))
	}
	if f.Unread != nil {
		clauses = append(clauses, mark(!*f.Unread, db.NotificationReadSQL))
	}
	if len(f.PriorityIn) > 0 {
		vals, err := json.Marshal(f.PriorityIn)
		if err != nil {
			return "", nil, err
		}
		clauses = append(clauses, `n.priority IN (SELECT value FROM json_each(?))`)
		args = append(args, string(vals))
	}
	if len(f.ScopeIn) > 0 {
		vals, err := json.Marshal(f.ScopeIn)
		if err != nil {
			return "", nil, err
		}
		clauses = append(clauses, `n.audience_scope IN (SELECT value FROM json_each(?))`)
		args = append(args, string(vals))
	}
	return strings.Join(clauses, " AND "), args, nil
}

// list counts and pages where over the same join to the viewer's marks,
// whose placeholder precedes where's.
func (s *notificationStore) list(ctx context.Context, orgID, userID, where string, args []any, opts db.ListOpts) ([]domain.NotificationView, int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	if err := db.RefuseNotificationKeyset(opts); err != nil {
		return nil, 0, err
	}
	const from = ` FROM notifications n
		LEFT JOIN notification_marks m ON m.notification_id = n.id AND m.user_id = ?
		WHERE `
	args = append([]any{userID}, args...)
	var total int
	if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*)`+from+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []domain.NotificationView{}, total, nil
	}
	query := `SELECT ` + sqliteNotificationViewCols + from + where + db.NotificationOrderSQL()
	if opts.Limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, opts.Limit, opts.Offset)
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
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	v, err := db.ScanNotificationView(s.q.QueryRowContext(ctx, `
		SELECT `+sqliteNotificationViewCols+`
		FROM notifications n
		LEFT JOIN notification_marks m ON m.notification_id = n.id AND m.user_id = ?
		WHERE `+sqliteNotificationReadable+` AND n.id = ?`,
		userID, orgID, time.Now().UTC(), userID, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (s *notificationStore) SetMarks(ctx context.Context, orgID, userID, id string, p db.NotificationMarksPatch) (domain.NotificationMarks, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.NotificationMarks{}, err
	}
	if err := db.ValidateNotificationMarksPatch(p); err != nil {
		return domain.NotificationMarks{}, err
	}
	now := time.Now().UTC()
	var readAt, dismissedAt any
	if p.Read != nil && *p.Read {
		readAt = now
	}
	dismissing := p.Dismissed != nil && *p.Dismissed
	if dismissing {
		dismissedAt = now
	}
	m, err := db.ScanNotificationMarks(s.q.QueryRowContext(ctx, `
		INSERT INTO notification_marks AS m (notification_id, user_id, org_id, read_at, dismissed_at)
		SELECT n.id, ?, n.org_id, ?, ?
		FROM notifications n
		WHERE `+sqliteNotificationReadable+` AND n.id = ?
		  AND (n.dismissible OR NOT ?)
		ON CONFLICT (notification_id, user_id) DO UPDATE SET
			read_at      = CASE WHEN ? THEN excluded.read_at ELSE m.read_at END,
			dismissed_at = CASE WHEN ? THEN excluded.dismissed_at ELSE m.dismissed_at END
		RETURNING `+sqliteNotificationMarksCols,
		userID, readAt, dismissedAt,
		orgID, now, userID, id,
		dismissing,
		p.Read != nil, p.Dismissed != nil).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.NotificationMarks{}, s.marksMiss(ctx, orgID, userID, id, now, dismissing)
	}
	return m, err
}

func (s *notificationStore) marksMiss(ctx context.Context, orgID, userID, id string, now time.Time, dismissing bool) error {
	var dismissible bool
	err := s.q.QueryRowContext(ctx, `
		SELECT n.dismissible FROM notifications n
		WHERE `+sqliteNotificationReadable+` AND n.id = ?`,
		orgID, now, userID, id).Scan(&dismissible)
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
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	out := []domain.NotificationMarks{}
	if len(ids) == 0 {
		return out, nil
	}
	idsJSON, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	rows, err := s.q.QueryContext(ctx, `
		INSERT INTO notification_marks AS m (notification_id, user_id, org_id, read_at)
		SELECT n.id, ?, n.org_id, ?
		FROM notifications n
		WHERE `+sqliteNotificationReadable+` AND n.id IN (SELECT value FROM json_each(?))
		ON CONFLICT (notification_id, user_id) DO UPDATE SET read_at = excluded.read_at
		RETURNING `+sqliteNotificationMarksCols,
		userID, now, orgID, now, userID, string(idsJSON))
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
