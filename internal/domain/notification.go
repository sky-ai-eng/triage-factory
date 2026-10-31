package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type NotificationOrigin string

const (
	NotificationOriginSystem NotificationOrigin = "system"
	NotificationOriginEvent  NotificationOrigin = "event"
)

// NotificationLifecycle: a condition is resolved by the system; a notice
// expires and is never resolved.
type NotificationLifecycle string

const (
	NotificationCondition NotificationLifecycle = "condition"
	NotificationNotice    NotificationLifecycle = "notice"
)

type NotificationScope string

const (
	NotificationScopeOrg  NotificationScope = "org"
	NotificationScopeTeam NotificationScope = "team"
	NotificationScopeUser NotificationScope = "user"
)

func AllNotificationScopes() []NotificationScope {
	return []NotificationScope{NotificationScopeOrg, NotificationScopeTeam, NotificationScopeUser}
}

type NotificationRole string

const (
	NotificationRoleMembers NotificationRole = "members"
	NotificationRoleAdmins  NotificationRole = "admins"
)

type NotificationPriority string

const (
	NotificationUrgent NotificationPriority = "urgent"
	NotificationHigh   NotificationPriority = "high"
	NotificationLow    NotificationPriority = "low"
)

// AllNotificationPriorities returns the priorities, highest first.
func AllNotificationPriorities() []NotificationPriority {
	return []NotificationPriority{NotificationUrgent, NotificationHigh, NotificationLow}
}

// Rank orders priorities (urgent highest); 0 for a value outside the vocabulary.
func (p NotificationPriority) Rank() int {
	switch p {
	case NotificationUrgent:
		return 3
	case NotificationHigh:
		return 2
	case NotificationLow:
		return 1
	}
	return 0
}

type NotificationResolvedReason string

const (
	NotificationResolvedCleared         NotificationResolvedReason = "cleared"
	NotificationResolvedSourceOff       NotificationResolvedReason = "source_off"
	NotificationResolvedAudienceRemoved NotificationResolvedReason = "audience_removed"
)

// NotificationStatus is the list filter over resolution.
type NotificationStatus string

const (
	NotificationOpen     NotificationStatus = "open"
	NotificationResolved NotificationStatus = "resolved"
)

// NotificationAudience is who a row is for. Role is empty for a user
// audience; TeamID is set only for team, UserID only for user.
type NotificationAudience struct {
	Scope  NotificationScope
	Role   NotificationRole
	TeamID string
	UserID string
}

var ErrInvalidNotificationAudience = errors.New("domain: invalid notification audience")

func (a NotificationAudience) Validate() error {
	switch a.Scope {
	case NotificationScopeOrg, NotificationScopeTeam:
		if a.Role != NotificationRoleMembers && a.Role != NotificationRoleAdmins {
			return fmt.Errorf("%w: a %s audience needs role %q or %q, got %q",
				ErrInvalidNotificationAudience, a.Scope, NotificationRoleMembers, NotificationRoleAdmins, a.Role)
		}
		if a.UserID != "" {
			return fmt.Errorf("%w: a %s audience names no user", ErrInvalidNotificationAudience, a.Scope)
		}
		if a.Scope == NotificationScopeOrg && a.TeamID != "" {
			return fmt.Errorf("%w: an org audience names no team", ErrInvalidNotificationAudience)
		}
		if a.Scope == NotificationScopeTeam && a.TeamID == "" {
			return fmt.Errorf("%w: a team audience needs a team id", ErrInvalidNotificationAudience)
		}
	case NotificationScopeUser:
		if a.Role != "" || a.TeamID != "" {
			return fmt.Errorf("%w: a user audience has no role or team", ErrInvalidNotificationAudience)
		}
		if a.UserID == "" {
			return fmt.Errorf("%w: a user audience needs a user id", ErrInvalidNotificationAudience)
		}
	default:
		return fmt.Errorf("%w: scope %q is not one of %v", ErrInvalidNotificationAudience, a.Scope, AllNotificationScopes())
	}
	return nil
}

// Notification is one notifications row. Empty strings stand for NULL.
type Notification struct {
	ID          string
	OrgID       string
	Origin      NotificationOrigin
	Lifecycle   NotificationLifecycle
	Kind        string
	Key         string
	Audience    NotificationAudience
	Source      string
	Priority    NotificationPriority
	Dismissible bool
	Title       string
	Body        string
	Actions     json.RawMessage
	Params      json.RawMessage
	FirstSeenAt time.Time
	LastSeenAt  time.Time
	SeenCount   int
	// EscalatedAt is FirstSeenAt until the priority first increases, or an
	// event notice recurs; marks older than it no longer count.
	EscalatedAt    time.Time
	ResolvedAt     *time.Time
	ResolvedReason NotificationResolvedReason
	ExpiresAt      *time.Time
	// EntityID is the entity an event row is about; set exactly when Origin
	// is NotificationOriginEvent.
	EntityID string
}

type NotificationMarks struct {
	NotificationID string
	UserID         string
	OrgID          string
	ReadAt         *time.Time
	DismissedAt    *time.Time
}

// NotificationView is a notification with one viewer's marks (nil when they
// have none) and the read/dismissed state derived from them.
type NotificationView struct {
	Notification
	Marks     *NotificationMarks
	Read      bool
	Dismissed bool
}
