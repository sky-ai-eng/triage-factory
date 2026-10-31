package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify/internal/sink"
)

type Priority = domain.NotificationPriority

const (
	Urgent = domain.NotificationUrgent
	High   = domain.NotificationHigh
	Low    = domain.NotificationLow
)

// Role narrows an org or team audience to its members or its admins.
type Role = domain.NotificationRole

const (
	Members = domain.NotificationRoleMembers
	Admins  = domain.NotificationRoleAdmins
)

// DefaultNoticeTTL is the expiry a notice's kind names unless it has a reason
// for another.
const DefaultNoticeTTL = 14 * 24 * time.Hour

// PriorityRule is a kind's priority: Always for a constant, PriorityOf when it
// depends on the params.
type PriorityRule struct {
	fixed Priority
	of    func(any) Priority
	param reflect.Type
}

func Always(p Priority) PriorityRule {
	if p.Rank() == 0 {
		panic(fmt.Sprintf("notify: priority %q is not one of %v", p, domain.AllNotificationPriorities()))
	}
	return PriorityRule{fixed: p}
}

func PriorityOf[P any](fn func(P) Priority) PriorityRule {
	if fn == nil {
		panic("notify: PriorityOf needs a function")
	}
	return PriorityRule{of: func(p any) Priority { return fn(p.(P)) }, param: reflect.TypeFor[P]()}
}

func (r PriorityRule) set() bool { return r.fixed != "" || r.of != nil }

func (r PriorityRule) at(p any) Priority {
	if r.of != nil {
		return r.of(p)
	}
	return r.fixed
}

// ConditionSpec declares a condition: open while the producer says it is
// true, resolved by the system when it stops being true.
type ConditionSpec[P any] struct {
	// Name is stored on every row and never renamed.
	Name string
	// Source is the event-source kind ClearSource resolves the condition by;
	// empty for a condition no source owns.
	Source   string
	Priority PriorityRule
	// Dismissible is false for a condition about something TF requires.
	Dismissible bool
	// Key is the occurrence's identity within the kind, built from ids.
	Key      func(P) string
	Audience Audience
	// Render is called once per recipient the audience resolves to.
	Render func(P, Recipient) Content
	// Example is a representative P for the kinds conformance test.
	Example P
}

// NoticeSpec declares a notice: something that happened once and leaves no
// state behind. It expires TTL after its last Post; nothing resolves it, and
// it is always dismissible.
type NoticeSpec[P any] struct {
	Name   string
	Source string
	// Priority defaults to Low.
	Priority PriorityRule
	// TTL is required; DefaultNoticeTTL unless the kind has a reason for
	// another.
	TTL      time.Duration
	Key      func(P) string
	Audience Audience
	Render   func(P, Recipient) Content
	Example  P
}

// kind is a registered kind with its params type erased; ConditionKind and
// NoticeKind are the typed handles on it.
type kind struct {
	name        string
	source      string
	lifecycle   domain.NotificationLifecycle
	dismissible bool
	ttl         time.Duration
	priority    PriorityRule
	key         func(any) string
	audience    Audience
	render      func(any, Recipient) Content
	example     any

	handlersMu sync.RWMutex
	handlers   map[string]handler
}

func (k *kind) Name() string                            { return k.name }
func (k *kind) Source() string                          { return k.source }
func (k *kind) Lifecycle() domain.NotificationLifecycle { return k.lifecycle }
func (k *kind) Key(p any) string                        { return k.key(p) }

// Kind is a registered kind of either lifecycle.
type Kind interface {
	Name() string
	core() *kind
}

// ConditionKind is a registered condition. Set and Sync are its verbs.
type ConditionKind[P any] struct{ k *kind }

// NoticeKind is a registered notice. Post is its verb.
type NoticeKind[P any] struct{ k *kind }

func (c *ConditionKind[P]) Name() string { return c.k.name }
func (c *ConditionKind[P]) core() *kind  { return c.k }
func (n *NoticeKind[P]) Name() string    { return n.k.name }
func (n *NoticeKind[P]) core() *kind     { return n.k }

// Condition registers a condition kind. Call it from a package-level var: a
// spec it refuses panics at init.
func Condition[P any](s ConditionSpec[P]) *ConditionKind[P] {
	k := &kind{
		name: s.Name, source: s.Source, lifecycle: domain.NotificationCondition,
		dismissible: s.Dismissible, priority: s.Priority, audience: s.Audience, example: s.Example,
	}
	if !s.Priority.set() {
		refuse(s.Name, "a condition needs a Priority")
	}
	k.key, k.render = erase(s.Name, s.Key, s.Render)
	c := &ConditionKind[P]{k: k}
	register[P](c)
	return c
}

// Notice registers a notice kind. Call it from a package-level var: a spec it
// refuses panics at init.
func Notice[P any](s NoticeSpec[P]) *NoticeKind[P] {
	k := &kind{
		name: s.Name, source: s.Source, lifecycle: domain.NotificationNotice,
		dismissible: true, ttl: s.TTL, priority: s.Priority, audience: s.Audience, example: s.Example,
	}
	if !k.priority.set() {
		k.priority = Always(Low)
	}
	if s.TTL <= 0 {
		refuse(s.Name, "a notice needs a TTL")
	}
	k.key, k.render = erase(s.Name, s.Key, s.Render)
	n := &NoticeKind[P]{k: k}
	register[P](n)
	return n
}

func erase[P any](name string, key func(P) string, render func(P, Recipient) Content) (func(any) string, func(any, Recipient) Content) {
	if key == nil {
		refuse(name, "Key is required")
	}
	if render == nil {
		refuse(name, "Render is required")
	}
	return func(p any) string { return key(p.(P)) },
		func(p any, to Recipient) Content { return render(p.(P), to) }
}

func refuse(name, format string, args ...any) {
	panic(fmt.Sprintf("notify: kind %q: %s", name, fmt.Sprintf(format, args...)))
}

var registry = struct {
	mu     sync.Mutex
	byName map[string]Kind
}{byName: map[string]Kind{}}

// register checks what the type system cannot — that every params-reading
// part of the spec reads P, and that P stores as a JSON object — and claims
// the name.
func register[P any](kd Kind) {
	k := kd.core()
	if k.name == "" {
		refuse(k.name, "Name is required")
	}
	if len(k.audience.parts) == 0 {
		refuse(k.name, "Audience is required")
	}
	want := reflect.TypeFor[P]()
	if k.priority.param != nil && k.priority.param != want {
		refuse(k.name, "Priority reads %v, the kind's params are %v", k.priority.param, want)
	}
	for _, part := range k.audience.parts {
		if part.param != nil && part.param != want {
			refuse(k.name, "audience %s reads %v, the kind's params are %v", part.name, part.param, want)
		}
	}
	raw, err := json.Marshal(k.example)
	var obj map[string]json.RawMessage
	if err == nil {
		err = json.Unmarshal(raw, &obj)
	}
	if err != nil || obj == nil {
		refuse(k.name, "params %v must marshal to a JSON object", want)
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, dup := registry.byName[k.name]; dup {
		refuse(k.name, "the name is already registered")
	}
	registry.byName[k.name] = kd
}

// Kinds returns every registered kind, ordered by name.
func Kinds() []Kind {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	out := make([]Kind, 0, len(registry.byName))
	for _, k := range registry.byName {
		out = append(out, k)
	}
	slices.SortFunc(out, func(a, b Kind) int { return strings.Compare(a.Name(), b.Name()) })
	return out
}

// Set opens or bumps p's occurrence when active is true and resolves it when
// false. Call it on every evaluation, not only on a change, so a failed write
// corrects itself on the next one.
func (c *ConditionKind[P]) Set(ctx context.Context, orgID string, p P, active bool) {
	if s := sink.Load(); s != nil {
		s.Set(ctx, c.k, orgID, p, active)
	}
}

// Sync opens or bumps every member of active, leaves the open occurrences of
// keep as they are, and resolves every other open occurrence of the kind in
// the org. keep is the members the pass has no answer for, which neither
// raises nor resolves them. Call it only on a complete pass: a pass that could
// not reach the upstream at all calls nothing.
func (c *ConditionKind[P]) Sync(ctx context.Context, orgID string, active []P, keep ...P) {
	s := sink.Load()
	if s == nil {
		return
	}
	s.Sync(ctx, c.k, orgID, erased(active), erased(keep))
}

// OpenOrgs returns the orgs, across the deployment, holding an open occurrence
// of the kind, sorted. It is for a producer whose evaluation set can lose an
// org while an occurrence is still open there: re-evaluating each org it
// returns is what resolves that occurrence. Unlike the verbs it returns its
// error, because only the caller knows what a missing answer costs. Before
// Install it returns nothing.
func (c *ConditionKind[P]) OpenOrgs(ctx context.Context) ([]string, error) {
	s := sink.Load()
	if s == nil {
		return nil, nil
	}
	return s.OpenOrgs(ctx, c.k)
}

func erased[P any](ps []P) []any {
	out := make([]any, len(ps))
	for i, p := range ps {
		out[i] = p
	}
	return out
}

// Post opens the notice, or bumps an unexpired one with the same key and
// extends its expiry.
func (n *NoticeKind[P]) Post(ctx context.Context, orgID string, p P) {
	if s := sink.Load(); s != nil {
		s.Post(ctx, n.k, orgID, p)
	}
}

// ClearSource resolves the org's open conditions of every kind whose Source is
// source: the source was turned off or its credential disconnected, so
// nothing will poll it to resolve them.
func ClearSource(ctx context.Context, orgID, source string) {
	if s := sink.Load(); s != nil {
		s.ClearSource(ctx, orgID, source)
	}
}
