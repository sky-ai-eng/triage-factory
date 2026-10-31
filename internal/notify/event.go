package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify/internal/sink"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// Event notifications (specs/durable-notifications.md, "Event notifications")
// are the rows notify rules raise: origin 'event', kind the event type, key
// the entity and the event's dedup key. The router decides which rules an
// event matched and whether its type is a condition; notify resolves who the
// rules tell, renders and writes the rows, and resolves them when the close
// phase says so.

// exampleUserID stands in for the creator a personal rule's Example tells.
const exampleUserID = "00000000-0000-0000-0000-000000000001"

// eventLabel is what an event row's failures are counted under: one label
// for every event type, which is in the log line rather than the metric.
const eventLabel = "event"

// EventRule is one notify rule an event matched.
type EventRule struct {
	HandlerID string
	TeamID    string
	Name      string
	// Audience is domain.NotifyAudienceCreator or domain.NotifyAudienceTeam.
	Audience      string
	CreatorUserID string
}

// EventRaise is one event's notification: what happened, the entity it
// happened to, and the rules it matched, in the order they are named.
type EventRaise struct {
	EventID   string
	EventType string
	// Source is the event source kind ClearSource resolves the row by.
	Source   string
	DedupKey string
	Entity   domain.Entity
	// Condition is whether a typed close relation closes the event type, so
	// the rows are resolved by the system rather than expiring.
	Condition bool
	Rules     []EventRule
}

// EventParams is an event row's params, and what its rendering reads. The ids
// are the occurrence's provenance; the rest is display only.
type EventParams struct {
	EventID   string `json:"event_id"`
	EventType string `json:"event_type"`
	EntityID  string `json:"entity_id"`
	DedupKey  string `json:"dedup_key"`
	// HandlerID is the rule the row names: the first the event matched for
	// the row's audience.
	HandlerID string `json:"handler_id"`
	// TeamRule is whether that rule tells its team rather than its creator.
	TeamRule     bool   `json:"team_rule"`
	RuleName     string `json:"rule_name"`
	EntitySource string `json:"entity_source"`
	EntityKey    string `json:"entity_key"`
	EntityTitle  string `json:"entity_title"`
	EntityURL    string `json:"entity_url"`
}

// EventSpec declares how event rows render.
type EventSpec struct {
	// Render is called once per row, for the recipient it is written for.
	Render func(EventParams, Recipient) Content
	// Example is a representative row for the conformance test.
	Example EventParams
}

// EventKind renders and writes event rows. Raise is its verb.
type EventKind struct {
	render  func(EventParams, Recipient) Content
	example EventParams
}

// Event declares the event rendering. Call it from a package-level var.
func Event(s EventSpec) *EventKind {
	if s.Render == nil {
		panic("notify: an event kind needs Render")
	}
	return &EventKind{render: s.Render, example: s.Example}
}

// Raise writes the event's rows: one for each matched team rule's team, and
// one for each matched personal rule's creator who is still a member of the
// rule's team and is not a member of a team whose rule matched. A failure is
// logged and counted, and writes nothing.
func (k *EventKind) Raise(ctx context.Context, orgID string, r EventRaise) {
	if len(r.Rules) == 0 {
		return
	}
	if s := sink.Load(); s != nil {
		s.RaiseEvent(ctx, k, orgID, r)
	}
}

// Examples renders the Example for a team row, a personal row, and local
// mode's one user, under the conformance checks Examples applies to a kind.
func (k *EventKind) Examples() []Example {
	var out []Example
	render := func(p EventParams, to Recipient, local bool) {
		ex := Example{Recipient: to, Local: local, Priority: High}
		defer func() {
			if r := recover(); r != nil {
				ex.Content, ex.Err = Content{}, fmt.Errorf("render panicked: %v", r)
			}
			out = append(out, ex)
		}()
		ex.Content = k.render(p, to)
		_, ex.Err = renderActions(domain.NotificationNotice, to, ex.Content.Actions, exampleOrgID, local, func(*url.URL) error { return nil })
	}
	team, personal := k.example, k.example
	team.TeamRule, personal.TeamRule = true, false
	render(team, Recipient{Scope: domain.NotificationScopeTeam, Role: Members, TeamID: exampleTeamID}, false)
	render(personal, Recipient{Scope: domain.NotificationScopeUser, UserID: exampleUserID}, false)
	render(personal, Recipient{Scope: domain.NotificationScopeUser, UserID: runmode.LocalDefaultUserID}, true)
	return out
}

// EventKey is an event occurrence's key: the entity, then the event's dedup
// key. An entity id is a UUID, so the first slash ends it.
func EventKey(entityID, dedupKey string) string {
	return entityID + "/" + dedupKey
}

// EventTarget is one open event condition about an entity.
type EventTarget struct {
	EventType string
	DedupKey  string
}

// OpenEventConditions returns the open event conditions about entityID whose
// event type is in eventTypes: what a close relation may resolve. Unlike the
// verbs it returns its error, because the close phase decides what a missing
// answer costs. Before Install it returns nothing.
func OpenEventConditions(ctx context.Context, orgID, entityID string, eventTypes []string) ([]EventTarget, error) {
	s := sink.Load()
	if s == nil || len(eventTypes) == 0 {
		return nil, nil
	}
	keys, err := s.OpenEvents(ctx, orgID, entityID, eventTypes)
	if err != nil {
		return nil, err
	}
	prefix := EventKey(entityID, "")
	out := make([]EventTarget, 0, len(keys))
	for _, k := range keys {
		out = append(out, EventTarget{EventType: k.Kind, DedupKey: strings.TrimPrefix(k.Key, prefix)})
	}
	return out, nil
}

// ResolveEvents resolves, for every audience, the open event conditions about
// entityID that targets name. A failure is logged and counted.
func ResolveEvents(ctx context.Context, orgID, entityID string, targets []EventTarget) {
	s := sink.Load()
	if s == nil || len(targets) == 0 {
		return
	}
	keys := make([]sink.EventKey, len(targets))
	for i, t := range targets {
		keys[i] = sink.EventKey{Kind: t.EventType, Key: EventKey(entityID, t.DedupKey)}
	}
	s.ResolveEvents(ctx, orgID, keys)
}

// ResolveClosedEntities resolves, for every audience, orgID's open event
// conditions about entities that are no longer active. A close relation
// resolves them when the event that closes the entity routes; an entity
// closed by a path that routes none (resolved from the stock deck, its
// repository retired) leaves them to this. A failure is logged and counted.
func ResolveClosedEntities(ctx context.Context, orgID string) {
	if s := sink.Load(); s != nil {
		s.ResolveClosedEntities(ctx, orgID)
	}
}

// ClosedEntitySweepInterval is how often RunClosedEntitySweep runs: the
// longest an event condition outlives an entity closed outside the router.
const ClosedEntitySweepInterval = 10 * time.Minute

// RunClosedEntitySweep runs ResolveClosedEntities for every org holding an
// event condition about a closed entity when it starts, then once every
// interval until ctx is cancelled. One deployment-wide read finds those orgs,
// so an org with nothing to resolve costs no transaction. Brain-gated by its
// caller: one process sweeps at a time. A nil store makes this a logged no-op.
func RunClosedEntitySweep(ctx context.Context, store db.NotificationStore, interval time.Duration) {
	if store == nil {
		notifyLog.Warn("closed-entity notification sweep not started: no NotificationStore wired")
		return
	}
	if interval <= 0 {
		interval = ClosedEntitySweepInterval
	}
	sweep := func() {
		orgIDs, err := store.ClosedEntityEventOrgsSystem(ctx)
		if err != nil {
			notifyLog.Warn("closed-entity notification sweep: list orgs failed; retrying next tick", "error", err)
			return
		}
		for _, orgID := range orgIDs {
			if ctx.Err() != nil {
				return
			}
			ResolveClosedEntities(ctx, orgID)
		}
	}
	sweep()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

// eventRow is one row an event raises: its audience and the rule it names.
type eventRow struct {
	to   Recipient
	rule EventRule
}

func (e *engine) RaiseEvent(ctx context.Context, sk any, orgID string, raw any) {
	defer e.recoverPanic(eventLabel, orgID)
	k, kindOK := sk.(*EventKind)
	r, raiseOK := raw.(EventRaise)
	if !kindOK || !raiseOK {
		e.fail(eventLabel, orgID, fmt.Errorf("an event raise takes an *EventKind and an EventRaise, not %T and %T", sk, raw))
		return
	}
	if r.Entity.ID == "" || r.EventType == "" {
		e.fail(eventLabel, orgID, errors.New("an event notification needs an event type and an entity"))
		return
	}
	rows, err := e.eventRows(ctx, orgID, r.Rules)
	if err != nil {
		e.fail(eventLabel, orgID, fmt.Errorf("%s: resolve audience: %w", r.EventType, err))
		return
	}
	if len(rows) == 0 {
		return
	}
	if e.b.Local {
		// N=1: the one user, named by the first rule.
		rows = []eventRow{{to: Recipient{Scope: domain.NotificationScopeUser, UserID: runmode.LocalDefaultUserID}, rule: rows[0].rule}}
	}
	lifecycle := domain.NotificationNotice
	var expiresAt *time.Time
	if r.Condition {
		lifecycle = domain.NotificationCondition
	} else {
		at := time.Now().Add(DefaultNoticeTTL)
		expiresAt = &at
	}
	res := e.resolution(orgID)
	key := EventKey(r.Entity.ID, r.DedupKey)
	occs := make([]db.NotificationOccurrence, 0, len(rows))
	for _, row := range rows {
		p := EventParams{
			EventID: r.EventID, EventType: r.EventType, EntityID: r.Entity.ID, DedupKey: r.DedupKey,
			HandlerID: row.rule.HandlerID, TeamRule: row.rule.Audience == domain.NotifyAudienceTeam, RuleName: row.rule.Name,
			EntitySource: r.Entity.Source, EntityKey: r.Entity.SourceID, EntityTitle: r.Entity.Title, EntityURL: r.Entity.URL,
		}
		c := k.render(p, row.to)
		stored, err := e.eventActions(ctx, lifecycle, row.to, c.Actions, orgID, res)
		if err != nil {
			e.fail(eventLabel, orgID, fmt.Errorf("%s: actions for a %s row: %w", r.EventType, row.to.Scope, err))
			return
		}
		var actions, params json.RawMessage
		if len(stored) > 0 {
			if actions, err = json.Marshal(stored); err != nil {
				e.fail(eventLabel, orgID, fmt.Errorf("marshal actions: %w", err))
				return
			}
		}
		if params, err = json.Marshal(p); err != nil {
			e.fail(eventLabel, orgID, fmt.Errorf("marshal params: %w", err))
			return
		}
		occs = append(occs, db.NotificationOccurrence{
			OrgID: orgID, Origin: domain.NotificationOriginEvent, Lifecycle: lifecycle, Kind: r.EventType, Key: key,
			Audience: row.to, Source: r.Source, Priority: High, Dismissible: true,
			Title: c.Title, Body: c.Body, Actions: actions, Params: params, ExpiresAt: expiresAt,
			EntityID: r.Entity.ID, Resurface: lifecycle == domain.NotificationNotice,
		})
	}
	e.commit(ctx, eventLabel, orgID, func(tx db.NotificationStore) ([]domain.Notification, error) {
		var changed []domain.Notification
		for _, o := range occs {
			u, err := tx.UpsertOpenSystem(ctx, o)
			if err != nil {
				return nil, err
			}
			if u.Opened || u.Escalated {
				changed = append(changed, u.Notification)
			}
		}
		return changed, nil
	})
}

// eventActions renders an event row's actions. An event row's links come
// from the entity it is about, which is data rather than code: an entity
// whose stored link is empty, or on a host the org no longer configures,
// fails one action, and the row keeps the others rather than telling nobody.
// A fault that is not one action's (too many, a notice left with none) still
// refuses the row, and so does an unreadable org setting: the host check
// could not run, and a row written without the link would keep that gap for
// as long as it stays open.
func (e *engine) eventActions(ctx context.Context, lc domain.NotificationLifecycle, to Recipient, as []Action, orgID string, res *resolution) ([]storedAction, error) {
	if _, err := res.orgSettings(ctx); err != nil {
		return nil, err
	}
	check := res.checkExternal(ctx)
	stored, err := renderActions(lc, to, as, orgID, e.b.Local, check)
	if err == nil {
		return stored, nil
	}
	kept := make([]Action, 0, len(as))
	for _, a := range as {
		if _, aerr := a.render(lc, to, orgID, e.b.Local, check); aerr != nil {
			notifyLog.WarnContext(ctx, "event notification action dropped", "org", orgID, "error", aerr)
			continue
		}
		kept = append(kept, a)
	}
	if len(kept) == len(as) {
		return nil, err
	}
	return renderActions(lc, to, kept, orgID, e.b.Local, check)
}

// eventRows is who the matched rules tell. A team rule tells its team's
// members; a personal rule tells its creator while they are a member of the
// rule's team, unless a matched team rule's team already reaches them. A
// team's members are the inbox's own "is sent" answer, so an archived team
// reaches nobody. Any failed read fails the whole answer: a partial one would
// tell a person what the read could not show they may hear.
func (e *engine) eventRows(ctx context.Context, orgID string, rules []EventRule) ([]eventRow, error) {
	members := map[string][]string{}
	membersOf := func(teamID string) ([]string, error) {
		if got, ok := members[teamID]; ok {
			return got, nil
		}
		got, err := e.b.Notifications.RecipientsSystem(ctx, orgID, Recipient{Scope: domain.NotificationScopeTeam, Role: Members, TeamID: teamID})
		if err != nil {
			return nil, err
		}
		members[teamID] = got
		return got, nil
	}
	var rows []eventRow
	has := func(to Recipient) bool {
		return slices.ContainsFunc(rows, func(r eventRow) bool { return r.to == to })
	}
	var told []string // teams a team row reaches
	for _, rule := range rules {
		if rule.Audience != domain.NotifyAudienceTeam || rule.TeamID == "" {
			continue
		}
		ms, err := membersOf(rule.TeamID)
		if err != nil {
			return nil, err
		}
		to := Recipient{Scope: domain.NotificationScopeTeam, Role: Members, TeamID: rule.TeamID}
		if len(ms) == 0 || has(to) {
			continue
		}
		rows = append(rows, eventRow{to: to, rule: rule})
		told = append(told, rule.TeamID)
	}
	for _, rule := range rules {
		if rule.Audience != domain.NotifyAudienceCreator || rule.CreatorUserID == "" || rule.TeamID == "" {
			continue
		}
		ms, err := membersOf(rule.TeamID)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(ms, rule.CreatorUserID) {
			continue
		}
		covered := false
		for _, team := range told {
			if ms, err := membersOf(team); err != nil {
				return nil, err
			} else if slices.Contains(ms, rule.CreatorUserID) {
				covered = true
				break
			}
		}
		to := Recipient{Scope: domain.NotificationScopeUser, UserID: rule.CreatorUserID}
		if covered || has(to) {
			continue
		}
		rows = append(rows, eventRow{to: to, rule: rule})
	}
	return rows, nil
}

func (e *engine) OpenEvents(ctx context.Context, orgID, entityID string, kinds []string) ([]sink.EventKey, error) {
	got, err := e.b.Notifications.OpenEventKeysSystem(ctx, orgID, entityID, kinds)
	if err != nil {
		return nil, fmt.Errorf("notify: open event conditions on %s: %w", entityID, err)
	}
	out := make([]sink.EventKey, len(got))
	for i, k := range got {
		out[i] = sink.EventKey{Kind: k.Kind, Key: k.Key}
	}
	return out, nil
}

func (e *engine) ResolveClosedEntities(ctx context.Context, orgID string) {
	defer e.recoverPanic(eventLabel, orgID)
	e.commit(ctx, eventLabel, orgID, func(tx db.NotificationStore) ([]domain.Notification, error) {
		return tx.ResolveClosedEntityEventsSystem(ctx, orgID)
	})
}

func (e *engine) ResolveEvents(ctx context.Context, orgID string, keys []sink.EventKey) {
	defer e.recoverPanic(eventLabel, orgID)
	e.commit(ctx, eventLabel, orgID, func(tx db.NotificationStore) ([]domain.Notification, error) {
		var resolved []domain.Notification
		for _, k := range keys {
			got, err := tx.ResolveSystem(ctx, orgID, domain.NotificationOriginEvent, k.Kind, k.Key)
			if err != nil {
				return nil, fmt.Errorf("resolve %s %s: %w", k.Kind, k.Key, err)
			}
			resolved = append(resolved, got...)
		}
		return resolved, nil
	})
}
