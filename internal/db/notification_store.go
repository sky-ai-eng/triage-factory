package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

var (
	// ErrNoSuchNotification: no row with that id the viewer may read
	// (expired notices included).
	ErrNoSuchNotification = errors.New("db: no such notification")

	// ErrNotificationNotDismissible is SetMarks' refusal to dismiss a
	// non-dismissible row.
	ErrNotificationNotDismissible = errors.New("db: notification is not dismissible")

	ErrInvalidNotification         = errors.New("db: invalid notification")
	ErrInvalidNotificationFilter   = errors.New("db: invalid notification filter")
	ErrEmptyNotificationMarksPatch = errors.New("db: a marks patch sets read, dismissed, or both")
)

// NotificationResolvedRetention is how long DeleteRetiredSystem keeps a
// resolved row.
const NotificationResolvedRetention = 30 * 24 * time.Hour

const notificationMaxActions = 2

// NotificationStore owns notifications and notification_marks.
//
// Postgres: every write to notifications is a ...System method on the admin
// pool (tf_app holds SELECT only); the reads and marks writes run on the app
// pool under the viewer's claims, where RLS is the "may read" predicate.
// SQLite is N=1: the local user is the recipient of every audience.
//
// Every read and marks write treats an expired notice as gone.
type NotificationStore interface {
	// UpsertOpenSystem inserts the occurrence, or bumps the open row with the
	// same (org, origin, kind, key, audience): seen_count+1, last_seen_at,
	// fresh priority/dismissible/source/copy/actions/params/expiry, and
	// escalated_at on a priority increase or when the occurrence resurfaces.
	// An expired notice is replaced by a new row rather than bumped.
	UpsertOpenSystem(ctx context.Context, o NotificationOccurrence) (NotificationUpsert, error)

	// ResolveSystem resolves (cleared) the open conditions of (kind, key)
	// for every audience.
	ResolveSystem(ctx context.Context, orgID string, origin domain.NotificationOrigin, kind, key string) ([]domain.Notification, error)

	// ResolveKeysNotInSystem resolves (cleared) the kind's open conditions
	// whose key is not in keep.
	ResolveKeysNotInSystem(ctx context.Context, orgID string, origin domain.NotificationOrigin, kind string, keep []string) ([]domain.Notification, error)

	// ResolveAudiencesNotInSystem resolves (audience_removed) the open
	// conditions of (kind, key) whose audience is not in keep.
	ResolveAudiencesNotInSystem(ctx context.Context, orgID string, origin domain.NotificationOrigin, kind, key string, keep []domain.NotificationAudience) ([]domain.Notification, error)

	// ResolveSourceSystem resolves (source_off) the org's open conditions
	// with that source.
	ResolveSourceSystem(ctx context.Context, orgID, source string) ([]domain.Notification, error)

	// ResolveIDSystem resolves (cleared) the one row id names, leaving the
	// other audiences' rows of its key open, and returns it; nil when it is
	// not an open condition in orgID.
	ResolveIDSystem(ctx context.Context, orgID, id string) (*domain.Notification, error)

	// OpenEventKeysSystem returns the distinct (kind, key) of the open event
	// conditions about entityID whose kind is in kinds, ordered: what an
	// event's close phase may resolve.
	OpenEventKeysSystem(ctx context.Context, orgID, entityID string, kinds []string) ([]NotificationKindKey, error)

	// OpenConditionOrgsSystem returns the distinct, sorted ids of the orgs,
	// across the deployment, holding an open condition of (origin, kind): the
	// orgs a producer whose evaluation set can lose an org re-evaluates so its
	// rows still resolve. Admin pool.
	OpenConditionOrgsSystem(ctx context.Context, origin domain.NotificationOrigin, kind string) ([]string, error)

	// RecipientsSystem returns the distinct, sorted ids of the users a row
	// with audience a in orgID is sent to: ListForViewer's "is sent"
	// predicate read the other way round, so a websocket ping reaches exactly
	// the inboxes the row appears in. An archived team's audience is nobody.
	// Admin pool, read fresh per emission.
	RecipientsSystem(ctx context.Context, orgID string, a domain.NotificationAudience) ([]string, error)

	// DeleteRetiredSystem deletes, across orgs, rows resolved more than
	// NotificationResolvedRetention before now and notices expired at now.
	DeleteRetiredSystem(ctx context.Context, now time.Time) (int64, error)

	// ListForViewer is the inbox: rows the viewer is sent ("is sent" — no
	// org-admin read-through on team rows, no archived teams, no expired
	// notices). Ordered by priority, escalated_at desc, id. Offset paging only.
	ListForViewer(ctx context.Context, orgID, userID string, f NotificationListFilter, opts ListOpts) ([]domain.NotificationView, int, error)

	// ListForOrg is the org admin's view: every row the viewer may read.
	ListForOrg(ctx context.Context, orgID, userID string, f NotificationOrgListFilter, opts ListOpts) ([]domain.NotificationView, int, error)

	// GetForViewer returns a row the viewer may read, or (nil, nil).
	GetForViewer(ctx context.Context, orgID, userID, id string) (*domain.NotificationView, error)

	// SetMarks stamps (true), clears (false) or keeps (nil) each mark.
	SetMarks(ctx context.Context, orgID, userID, id string, p NotificationMarksPatch) (domain.NotificationMarks, error)

	// MarkRead stamps read_at on the ids the viewer may read and returns the
	// marks written; unreadable ids are skipped.
	MarkRead(ctx context.Context, orgID, userID string, ids []string) ([]domain.NotificationMarks, error)
}

// NotificationOccurrence is what UpsertOpenSystem writes. The store mints the
// id and every timestamp but ExpiresAt.
type NotificationOccurrence struct {
	OrgID       string
	Origin      domain.NotificationOrigin
	Lifecycle   domain.NotificationLifecycle
	Kind        string
	Key         string
	Audience    domain.NotificationAudience
	Source      string
	Priority    domain.NotificationPriority
	Dismissible bool
	Title       string
	Body        string
	Actions     json.RawMessage // array, at most two; nil stores []
	Params      json.RawMessage // object; nil stores {}
	ExpiresAt   *time.Time      // notices only
	// EntityID is the entity an event row is about: required when Origin is
	// event, and empty otherwise.
	EntityID string
	// Resurface makes a bump of an open row unread and undismissed again, as
	// an escalation does: an event notice whose event recurred.
	Resurface bool
}

// NotificationKindKey is one occurrence's identity within an org and origin.
type NotificationKindKey struct {
	Kind string
	Key  string
}

type NotificationUpsert struct {
	Notification domain.Notification
	Opened       bool
	Escalated    bool
}

// NotificationUpsertOf derives what an upsert did from the row it returned:
// an insert has seen_count 1, and an escalating bump stamps escalated_at and
// last_seen_at with the same instant.
func NotificationUpsertOf(n domain.Notification) NotificationUpsert {
	opened := n.SeenCount == 1
	return NotificationUpsert{
		Notification: n,
		Opened:       opened,
		Escalated:    !opened && n.EscalatedAt.Equal(n.LastSeenAt),
	}
}

// ValidateNotificationOccurrence is UpsertOpenSystem's door in both dialects.
// It returns o with actions and params defaulted and compacted.
func ValidateNotificationOccurrence(o NotificationOccurrence) (NotificationOccurrence, error) {
	fail := func(format string, args ...any) (NotificationOccurrence, error) {
		return NotificationOccurrence{}, fmt.Errorf("%w: %s", ErrInvalidNotification, fmt.Sprintf(format, args...))
	}
	if o.OrgID == "" {
		return fail("org_id is required")
	}
	if o.Origin != domain.NotificationOriginSystem && o.Origin != domain.NotificationOriginEvent {
		return fail("origin %q is not %q or %q", o.Origin, domain.NotificationOriginSystem, domain.NotificationOriginEvent)
	}
	if o.Kind == "" {
		return fail("kind is required")
	}
	if (o.Origin == domain.NotificationOriginEvent) != (o.EntityID != "") {
		return fail("an entity is named by an event row, and only by one")
	}
	if err := o.Audience.Validate(); err != nil {
		return fail("%v", err)
	}
	if o.Priority.Rank() == 0 {
		return fail("priority %q is not one of %v", o.Priority, domain.AllNotificationPriorities())
	}
	switch o.Lifecycle {
	case domain.NotificationCondition:
		if o.ExpiresAt != nil {
			return fail("a condition does not expire")
		}
	case domain.NotificationNotice:
		if o.ExpiresAt == nil {
			return fail("a notice needs an expiry")
		}
		if !o.Dismissible {
			return fail("a notice is always dismissible")
		}
	default:
		return fail("lifecycle %q is not %q or %q", o.Lifecycle, domain.NotificationCondition, domain.NotificationNotice)
	}
	if strings.TrimSpace(o.Title) == "" {
		return fail("title is required")
	}

	actions, err := compactJSON(o.Actions, "[]")
	if err != nil {
		return fail("actions: %v", err)
	}
	var list []json.RawMessage
	if err := json.Unmarshal(actions, &list); err != nil || list == nil {
		return fail("actions must be a JSON array")
	}
	if len(list) > notificationMaxActions {
		return fail("at most %d actions, got %d", notificationMaxActions, len(list))
	}
	params, err := compactJSON(o.Params, "{}")
	if err != nil {
		return fail("params: %v", err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(params, &obj); err != nil || obj == nil {
		return fail("params must be a JSON object")
	}

	o.Actions, o.Params = actions, params
	if o.ExpiresAt != nil {
		at := o.ExpiresAt.UTC()
		o.ExpiresAt = &at
	}
	return o, nil
}

func compactJSON(raw json.RawMessage, empty string) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(empty), nil
	}
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return nil, err
	}
	return json.RawMessage(b.Bytes()), nil
}

// NotificationListFilter: a zero field, including an empty slice, does not
// filter.
type NotificationListFilter struct {
	Status     domain.NotificationStatus
	Dismissed  *bool
	Unread     *bool
	PriorityIn []domain.NotificationPriority
	ScopeIn    []domain.NotificationScope
}

func (f NotificationListFilter) Validate() error {
	if f.Status != "" && f.Status != domain.NotificationOpen && f.Status != domain.NotificationResolved {
		return fmt.Errorf("%w: status %q is not %q or %q", ErrInvalidNotificationFilter, f.Status, domain.NotificationOpen, domain.NotificationResolved)
	}
	for _, p := range f.PriorityIn {
		if p.Rank() == 0 {
			return fmt.Errorf("%w: priority %q is not one of %v", ErrInvalidNotificationFilter, p, domain.AllNotificationPriorities())
		}
	}
	for _, s := range f.ScopeIn {
		if !slices.Contains(domain.AllNotificationScopes(), s) {
			return fmt.Errorf("%w: scope %q is not one of %v", ErrInvalidNotificationFilter, s, domain.AllNotificationScopes())
		}
	}
	return nil
}

type NotificationOrgListFilter struct {
	NotificationListFilter
	TeamIDIn []string
}

type NotificationMarksPatch struct {
	Read      *bool
	Dismissed *bool
}

func ValidateNotificationMarksPatch(p NotificationMarksPatch) error {
	if p.Read == nil && p.Dismissed == nil {
		return ErrEmptyNotificationMarksPatch
	}
	return nil
}

// RefuseNotificationKeyset: the notification lists page by offset only.
func RefuseNotificationKeyset(opts ListOpts) error {
	if len(opts.After) > 0 {
		return errors.New("db: the notification lists page by offset, not by keyset")
	}
	return nil
}

// Shared list-read fragments; notifications is aliased n, the viewer's marks m.
const (
	NotificationReadSQL      = `(m.read_at IS NOT NULL AND m.read_at >= n.escalated_at)`
	NotificationDismissedSQL = `(n.dismissible AND m.dismissed_at IS NOT NULL AND m.dismissed_at > n.escalated_at)`
)

// NotificationStateClauses renders f's placeholder-free predicates: status,
// dismissed and unread.
func NotificationStateClauses(f NotificationListFilter) []string {
	var out []string
	switch f.Status {
	case domain.NotificationOpen:
		out = append(out, `n.resolved_at IS NULL`)
	case domain.NotificationResolved:
		out = append(out, `n.resolved_at IS NOT NULL`)
	}
	if f.Dismissed != nil {
		out = append(out, notOrSQL(*f.Dismissed, NotificationDismissedSQL))
	}
	if f.Unread != nil {
		out = append(out, notOrSQL(!*f.Unread, NotificationReadSQL))
	}
	return out
}

func notOrSQL(want bool, expr string) string {
	if want {
		return expr
	}
	return `NOT ` + expr
}

func NotificationOrderSQL() string {
	return ` ORDER BY ` + NotificationPriorityRankSQL("n.priority") + ` DESC, n.escalated_at DESC, n.id DESC`
}

// NotificationPriorityRankSQL renders NotificationPriority.Rank over expr.
func NotificationPriorityRankSQL(expr string) string {
	var b strings.Builder
	b.WriteString("CASE " + expr)
	for _, p := range domain.AllNotificationPriorities() {
		fmt.Fprintf(&b, " WHEN '%s' THEN %d", p, p.Rank())
	}
	b.WriteString(" ELSE 0 END")
	return b.String()
}

// NotificationAudiencesJSON renders audiences as [{scope, role, team_id,
// user_id}] with unset fields null, for ResolveAudiencesNotInSystem.
func NotificationAudiencesJSON(audiences []domain.NotificationAudience) (string, error) {
	type row struct {
		Scope  string  `json:"scope"`
		Role   *string `json:"role"`
		TeamID *string `json:"team_id"`
		UserID *string `json:"user_id"`
	}
	ptr := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	rows := make([]row, 0, len(audiences))
	for _, a := range audiences {
		if err := a.Validate(); err != nil {
			return "", fmt.Errorf("%w: %v", ErrInvalidNotification, err)
		}
		rows = append(rows, row{Scope: string(a.Scope), Role: ptr(string(a.Role)), TeamID: ptr(a.TeamID), UserID: ptr(a.UserID)})
	}
	b, err := json.Marshal(rows)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// notificationRow holds the 25-column projection both dialects emit, with
// nullable text COALESCEd to the empty string, while it is scanned.
type notificationRow struct {
	n                   domain.Notification
	actions, params     []byte
	resolvedAt, expires sql.NullTime
}

func (r *notificationRow) dest() []any {
	n := &r.n
	return []any{&n.ID, &n.OrgID, &n.Origin, &n.Lifecycle, &n.Kind, &n.Key,
		&n.Audience.Scope, &n.Audience.Role, &n.Audience.TeamID, &n.Audience.UserID,
		&n.Source, &n.Priority, &n.Dismissible, &n.Title, &n.Body, &r.actions, &r.params,
		&n.FirstSeenAt, &n.LastSeenAt, &n.SeenCount, &n.EscalatedAt,
		&r.resolvedAt, &n.ResolvedReason, &r.expires, &n.EntityID}
}

func (r *notificationRow) finish() domain.Notification {
	n := r.n
	n.Actions = json.RawMessage(r.actions)
	n.Params = json.RawMessage(r.params)
	n.FirstSeenAt = n.FirstSeenAt.UTC()
	n.LastSeenAt = n.LastSeenAt.UTC()
	n.EscalatedAt = n.EscalatedAt.UTC()
	n.ResolvedAt = utcTimePtr(r.resolvedAt)
	n.ExpiresAt = utcTimePtr(r.expires)
	return n
}

func ScanNotification(scan func(...any) error) (domain.Notification, error) {
	var r notificationRow
	if err := scan(r.dest()...); err != nil {
		return domain.Notification{}, err
	}
	return r.finish(), nil
}

// ScanNotificationView reads the notification projection, the viewer's marks
// row (all NULL when absent), then the read and dismissed booleans.
func ScanNotificationView(scan func(...any) error) (domain.NotificationView, error) {
	var (
		r                         notificationRow
		v                         domain.NotificationView
		markID, markUser, markOrg sql.NullString
		readAt, dismissedAt       sql.NullTime
	)
	err := scan(append(r.dest(), &markID, &markUser, &markOrg, &readAt, &dismissedAt, &v.Read, &v.Dismissed)...)
	if err != nil {
		return domain.NotificationView{}, err
	}
	v.Notification = r.finish()
	if markID.Valid {
		v.Marks = &domain.NotificationMarks{
			NotificationID: markID.String,
			UserID:         markUser.String,
			OrgID:          markOrg.String,
			ReadAt:         utcTimePtr(readAt),
			DismissedAt:    utcTimePtr(dismissedAt),
		}
	}
	return v, nil
}

func ScanNotificationMarks(scan func(...any) error) (domain.NotificationMarks, error) {
	var (
		m                   domain.NotificationMarks
		readAt, dismissedAt sql.NullTime
	)
	if err := scan(&m.NotificationID, &m.UserID, &m.OrgID, &readAt, &dismissedAt); err != nil {
		return domain.NotificationMarks{}, err
	}
	m.ReadAt, m.DismissedAt = utcTimePtr(readAt), utcTimePtr(dismissedAt)
	return m, nil
}
