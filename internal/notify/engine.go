package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/notify/internal/sink"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

var notifyLog = logging.Component("notify")

const origin = domain.NotificationOriginSystem

// clearSourceLabel is the kind ClearSource's failures are counted under: no
// one kind failed, and the source is in the log line rather than the metric.
const clearSourceLabel = "clear_source"

// errUnrendered marks a member whose actions could not be rendered: one the
// write refuses, or one whose host check could not read the org. It is that
// member's params at fault rather than the call's, so Sync skips the member
// and writes the rest.
var errUnrendered = errors.New("actions not rendered")

// engine is the sink Install builds: each verb resolves its audiences, writes
// in one transaction, and pings the rows that opened, escalated or resolved.
type engine struct{ b Backend }

var _ sink.Sink = (*engine)(nil)

func (e *engine) Set(ctx context.Context, sk sink.Kind, orgID string, p any, active bool) {
	k := sk.(*kind)
	defer e.recoverPanic(k.name, orgID)
	if !active {
		e.commit(ctx, k.name, orgID, func(tx db.NotificationStore) ([]domain.Notification, error) {
			return tx.ResolveSystem(ctx, orgID, origin, k.name, k.key(p))
		})
		return
	}
	occ, err := e.prepare(ctx, k, e.resolution(orgID), p, nil)
	if err != nil {
		e.fail(k.name, orgID, err)
		return
	}
	e.commit(ctx, k.name, orgID, occ.writeCondition(ctx, orgID))
}

func (e *engine) Sync(ctx context.Context, sk sink.Kind, orgID string, active []any) {
	k := sk.(*kind)
	defer e.recoverPanic(k.name, orgID)
	r := e.resolution(orgID)
	var occs []occurrence
	at := map[string]int{}
	var unrendered []string
	for _, p := range active {
		occ, err := e.prepare(ctx, k, r, p, nil)
		if errors.Is(err, errUnrendered) {
			// One member's rows are at fault, not the pass: the rest still
			// write, and its key is kept so its open row is left as it was.
			e.fail(k.name, orgID, err)
			unrendered = append(unrendered, k.key(p))
			continue
		}
		if err != nil {
			e.fail(k.name, orgID, err)
			return
		}
		// Two members with one key are one occurrence, and the later one's
		// params win, audience included, as two Sets in a row would.
		if i, ok := at[occ.key]; ok {
			occs[i] = occ
			continue
		}
		at[occ.key] = len(occs)
		occs = append(occs, occ)
	}
	keys := make([]string, 0, len(occs)+len(unrendered))
	for _, occ := range occs {
		keys = append(keys, occ.key)
	}
	keys = append(keys, unrendered...)
	e.commit(ctx, k.name, orgID, func(tx db.NotificationStore) ([]domain.Notification, error) {
		var changed []domain.Notification
		for _, occ := range occs {
			got, err := occ.writeCondition(ctx, orgID)(tx)
			if err != nil {
				return nil, err
			}
			changed = append(changed, got...)
		}
		resolved, err := tx.ResolveKeysNotInSystem(ctx, orgID, origin, k.name, keys)
		return append(changed, resolved...), err
	})
}

func (e *engine) Post(ctx context.Context, sk sink.Kind, orgID string, p any) {
	k := sk.(*kind)
	defer e.recoverPanic(k.name, orgID)
	expires := time.Now().Add(k.ttl)
	occ, err := e.prepare(ctx, k, e.resolution(orgID), p, &expires)
	if err != nil {
		e.fail(k.name, orgID, err)
		return
	}
	e.commit(ctx, k.name, orgID, occ.upsert(ctx))
}

func (e *engine) ClearSource(ctx context.Context, orgID, source string) {
	defer e.recoverPanic(clearSourceLabel, orgID)
	e.commit(ctx, clearSourceLabel, orgID, func(tx db.NotificationStore) ([]domain.Notification, error) {
		resolved, err := tx.ResolveSourceSystem(ctx, orgID, source)
		if err != nil {
			return nil, fmt.Errorf("source %s: %w", source, err)
		}
		return resolved, nil
	})
}

func (e *engine) resolution(orgID string) *resolution {
	return &resolution{b: &e.b, orgID: orgID}
}

// occurrence is one key's rows, one per recipient, ready to write.
type occurrence struct {
	kind, key  string
	recipients []Recipient
	rows       []db.NotificationOccurrence
}

// prepare resolves p's audience and renders a row for each recipient. A
// resolution failure fails it: the call writes nothing rather than resolve
// the rows of recipients it could not read.
func (e *engine) prepare(ctx context.Context, k *kind, r *resolution, p any, expiresAt *time.Time) (occurrence, error) {
	occ := occurrence{kind: k.name, key: k.key(p)}
	recipients, err := k.audience.resolve(ctx, r, p)
	if err != nil {
		return occurrence{}, fmt.Errorf("resolve audience: %w", err)
	}
	if e.b.Local {
		if recipients, err = e.localRecipients(ctx, r.orgID, recipients); err != nil {
			return occurrence{}, err
		}
	}
	params, err := json.Marshal(p)
	if err != nil {
		return occurrence{}, fmt.Errorf("marshal params: %w", err)
	}
	priority := k.priority.at(p)
	occ.recipients = recipients
	for _, to := range recipients {
		c := k.render(p, to)
		stored, err := renderActions(k.lifecycle, to, c.Actions, r.orgID, e.b.Local, r.checkExternal(ctx))
		if err != nil {
			return occurrence{}, fmt.Errorf("%w: actions for a %s row: %w", errUnrendered, to.Scope, err)
		}
		var actions json.RawMessage
		if len(stored) > 0 {
			if actions, err = json.Marshal(stored); err != nil {
				return occurrence{}, fmt.Errorf("marshal actions: %w", err)
			}
		}
		occ.rows = append(occ.rows, db.NotificationOccurrence{
			OrgID: r.orgID, Origin: origin, Lifecycle: k.lifecycle, Kind: occ.kind, Key: occ.key,
			Audience: to, Source: k.source, Priority: priority, Dismissible: k.dismissible,
			Title: c.Title, Body: c.Body, Actions: actions, Params: params, ExpiresAt: expiresAt,
		})
	}
	return occ, nil
}

// localRecipients is N=1's audience: one row for the one user when any of
// rs is sent to them, so a kind with several audiences shows once. What is
// sent to whom is the store's answer, as it is for the ping, so an archived
// team's audience reaches nobody here either.
func (e *engine) localRecipients(ctx context.Context, orgID string, rs []Recipient) ([]Recipient, error) {
	for _, to := range rs {
		users, err := e.b.Notifications.RecipientsSystem(ctx, orgID, to)
		if err != nil {
			return nil, fmt.Errorf("resolve local recipient: %w", err)
		}
		if len(users) > 0 {
			return []Recipient{{Scope: domain.NotificationScopeUser, UserID: runmode.LocalDefaultUserID}}, nil
		}
	}
	return nil, nil
}

// upsert writes every row, returning those that opened or escalated: a bump
// of last_seen_at alone pings nobody.
func (o occurrence) upsert(ctx context.Context) func(db.NotificationStore) ([]domain.Notification, error) {
	return func(tx db.NotificationStore) ([]domain.Notification, error) {
		var changed []domain.Notification
		for _, row := range o.rows {
			u, err := tx.UpsertOpenSystem(ctx, row)
			if err != nil {
				return nil, err
			}
			if u.Opened || u.Escalated {
				changed = append(changed, u.Notification)
			}
		}
		return changed, nil
	}
}

// writeCondition upserts the rows, then resolves the key's open rows for the
// audiences no longer named.
func (o occurrence) writeCondition(ctx context.Context, orgID string) func(db.NotificationStore) ([]domain.Notification, error) {
	return func(tx db.NotificationStore) ([]domain.Notification, error) {
		changed, err := o.upsert(ctx)(tx)
		if err != nil {
			return nil, err
		}
		removed, err := tx.ResolveAudiencesNotInSystem(ctx, orgID, origin, o.kind, o.key, o.recipients)
		return append(changed, removed...), err
	}
}

// commit runs one call's writes in one transaction and pings what they
// changed once it commits.
func (e *engine) commit(ctx context.Context, label, orgID string, write func(db.NotificationStore) ([]domain.Notification, error)) {
	var changed []domain.Notification
	err := e.b.Tx.SystemWithTx(ctx, orgID, func(tx db.SystemTxStores) error {
		var err error
		changed, err = write(tx.Notifications)
		return err
	})
	if err != nil {
		e.fail(label, orgID, fmt.Errorf("write: %w", err))
		return
	}
	e.ping(ctx, orgID, changed)
}

// ping sends notifications_updated to everyone a changed row is sent to. An
// org members row is one org envelope, which reaches every other recipient
// too; any other row fans out one envelope per user, resolved now. A
// resolution failure drops that audience's envelopes rather than widening
// them.
func (e *engine) ping(ctx context.Context, orgID string, changed []domain.Notification) {
	if len(changed) == 0 || e.b.Hub == nil {
		return
	}
	orgWide := false
	for _, n := range changed {
		if n.Audience.Scope == domain.NotificationScopeOrg && n.Audience.Role == Members {
			orgWide = true
		}
	}
	if orgWide {
		e.b.Hub.Broadcast(websocket.Event{Type: EventType, OrgID: orgID})
		return
	}
	var audiences []Recipient
	for _, n := range changed {
		audiences = appendNew(audiences, n.Audience)
	}
	sent := map[string]bool{}
	for _, a := range audiences {
		users, err := e.b.Notifications.RecipientsSystem(ctx, orgID, a)
		if err != nil {
			notifyLog.Error("resolve notification recipients failed; dropping their ping", "org", orgID, "scope", a.Scope, "team", a.TeamID, "error", err)
			continue
		}
		for _, uid := range users {
			// An empty id would widen the envelope to the whole org.
			if uid == "" || sent[uid] {
				continue
			}
			sent[uid] = true
			e.b.Hub.Broadcast(websocket.Event{Type: EventType, OrgID: orgID, UserID: uid})
		}
	}
}

func (e *engine) fail(label, orgID string, err error) {
	notifyLog.Warn("notification write failed", "kind", label, "org", orgID, "error", err)
	recordWriteFailure(label)
}

// recoverPanic keeps a kind's own bug — a Key or Render that panics on params
// its Example did not cover — from failing the producer that raised it.
func (e *engine) recoverPanic(label, orgID string) {
	if r := recover(); r != nil {
		notifyLog.Error("notification kind panicked", "kind", label, "org", orgID, "panic", fmt.Sprint(r))
		recordWriteFailure(label)
	}
}
