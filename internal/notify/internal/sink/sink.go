// Package sink is the seam between notify's verbs and what carries them out:
// the database engine notify.Install builds, or notifytest's recorder. It is
// internal so that nothing outside internal/notify can install a sink of its
// own.
package sink

import (
	"context"
	"sync/atomic"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Guard is a producer's own write, run first in the transaction that writes
// a notification: the notification is written only when it reports true, and
// a failed notification write rolls it back. notify.Guard documents it.
type Guard func(ctx context.Context, tx db.SystemTxStores) (bool, error)

// Kind is what a sink needs of a registered kind. notify's concrete kind is
// the only implementation.
type Kind interface {
	Name() string
	Source() string
	Lifecycle() domain.NotificationLifecycle
	Key(p any) string
}

// EventKey is one event occurrence's identity in an org: its kind, the event
// type, and its key.
type EventKey struct {
	Kind string
	Key  string
}

// Sink carries out the verbs and answers the reads, OpenOrgs and OpenEvents.
// p and the members of active and keep are the kind's params type. For the
// event verbs, k is notify's event kind and raise is a notify.EventRaise. A
// nil guard on Set or Post is none.
type Sink interface {
	Set(ctx context.Context, k Kind, orgID string, p any, active bool, guard Guard)
	Sync(ctx context.Context, k Kind, orgID string, active, keep []any)
	Post(ctx context.Context, k Kind, orgID string, p any, guard Guard)
	ClearSource(ctx context.Context, orgID, source string)
	OpenOrgs(ctx context.Context, k Kind) ([]string, error)

	RaiseEvent(ctx context.Context, k any, orgID string, raise any)
	OpenEvents(ctx context.Context, orgID, entityID string, kinds []string) ([]EventKey, error)
	// ResolveEvents resolves keys on the routing of an event of type closedBy.
	ResolveEvents(ctx context.Context, orgID, closedBy string, keys []EventKey)
	ResolveClosedEntities(ctx context.Context, orgID string)
}

type holder struct{ s Sink }

var current atomic.Pointer[holder]

// Load returns the installed sink, or nil before one is installed.
func Load() Sink {
	if h := current.Load(); h != nil {
		return h.s
	}
	return nil
}

// Swap installs s (nil uninstalls) and returns the sink it replaced.
func Swap(s Sink) Sink {
	var h *holder
	if s != nil {
		h = &holder{s: s}
	}
	if old := current.Swap(h); old != nil {
		return old.s
	}
	return nil
}
