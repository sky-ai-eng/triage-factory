// Package eventsource answers one question, per org: which event sources can
// actually reach this organization, and when one cannot, why not.
//
// The answer is DERIVED on every read and never stored. Every way a source can
// be off already exists and is meaningful somewhere else — the credential is
// unbound, the workspace was disconnected, the licence lapsed, the source has
// not shipped — so a stored flag would be one more way to be off, free to
// disagree with the other four. Nothing here has a setter.
//
// Its readers want different slices of the same derivation: the org
// availability read hands the whole vocabulary to the UI, an event-handler
// list maps a page of handlers onto it, and a create asks about the single
// source the new handler binds to. All three go through the declarations
// below, so a source added once is answered consistently everywhere.
//
// # Availability is ORG-level
//
// A source is available when the org can produce its events at all — a
// credential resolves, a workspace is connected. What a given TEAM has brought
// into scope (tracked repos, tracked Jira projects, tracked channels) is
// DEMAND, not availability: a team tracking nothing must still see Jira as
// available, or there is no way in to track something. Nothing here reads a
// tracked set.
//
// # The registration seam
//
// Core ships github and jira (probed here) plus the sources TF has declared
// but not built. Everything else registers: core never imports ee/, so an
// out-of-tree source plugs its own probe in at startup the same way it plugs
// into routing and entitlements.
package eventsource

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The source kinds core itself declares. A registered source's kind is its own
// string (the segment before the first ':' in its event types, e.g. "slack" in
// "slack:message") and is never named here.
const (
	KindGitHub   = "github"
	KindJira     = "jira"
	KindLinear   = "linear"
	KindSchedule = "schedule"
)

// KindOf returns the source kind an event type belongs to — the segment
// before the first ':' ("jira" for "jira:issue:assigned"). A type with no
// colon is its own kind, which keeps a malformed value out of another source's
// answer instead of silently joining it.
func KindOf(eventType string) string {
	if i := strings.IndexByte(eventType, ':'); i >= 0 {
		return eventType[:i]
	}
	return eventType
}

// State is a source's answer for one org — a closed vocabulary of four values,
// each naming a different thing the reader can do about it.
type State string

const (
	// StateAvailable — the org can produce events from this source.
	StateAvailable State = "available"
	// StateUnconfigured — built and permitted, but org-level setup is
	// missing. The one state a reader can fix themselves, or ask an org admin
	// to.
	StateUnconfigured State = "unconfigured"
	// StateUnlicensed — the entitlement is absent. Not the reader's to fix.
	StateUnlicensed State = "unlicensed"
	// StateWIP — TF has not shipped this source. Nobody's to fix.
	StateWIP State = "wip"
)

// Source is one source's answer, and the wire shape of an element of the org
// availability read.
type Source struct {
	Kind  string `json:"kind"`
	State State  `json:"state"`
}

// Availability is one org's whole resolved vocabulary, in wire order.
type Availability []Source

// State returns kind's resolved state, ok=false when kind is outside this
// org's vocabulary — a source this deployment does not carry, one omitted by
// run mode, or a prefix that names no source at all (system:).
func (a Availability) State(kind string) (State, bool) {
	if i := slices.IndexFunc(a, func(s Source) bool { return s.Kind == kind }); i >= 0 {
		return a[i].State, true
	}
	return "", false
}

// CanProduce reports whether an event handler bound to an event type of this
// source could ever fire for the org.
//
// A kind outside the vocabulary reports TRUE. Availability takes no view on
// system: (bus-only, never routed) or on a source this build does not carry,
// and the flag exists to explain a handler that cannot fire for a reason the
// reader can act on — never to mark one this read cannot speak to.
func (a Availability) CanProduce(kind string) bool {
	st, known := a.State(kind)
	return !known || st == StateAvailable
}

// Probe resolves one registered source's state for an org. It is handed the
// caller's transaction, so it reads under the requesting user's claims like
// every other request-path read, and it returns an error rather than a state
// when it cannot answer: a failed read is not "unconfigured", and answering
// one as the other is what makes a status read impossible to trust.
type Probe func(ctx context.Context, tx db.TxStores, orgID string) (State, error)

// Registration is what a source outside core's tree declares about itself.
type Registration struct {
	// Probe resolves the source's state. Required.
	Probe Probe
	// MultiOnly omits the source from local-mode reads entirely, for a source
	// whose backing store or transport cannot exist there. Omission is the
	// honest answer in that case: every other state is a claim about this
	// deployment's setup or its licensing, and neither question is being asked
	// in a mode where the source structurally cannot run.
	MultiOnly bool
}

// probeFn is a declaration's resolution step. It takes the pass's resolver
// rather than the transaction, because core's own two probes are cut from one
// credential bundle and must share the read that loads it; a registered
// source's Probe is adapted to this shape at declaration time.
type probeFn func(ctx context.Context, r *resolver) (State, error)

// declaration is one source's entry in the resolved vocabulary — core's
// built-ins and every registered source alike. A registration's MultiOnly is
// resolved away before a declaration is built: local mode omits such a source
// rather than carrying it here with a flag every reader would have to honour.
type declaration struct {
	kind string
	// probe is nil for a source TF has declared but not built: its state is
	// the constant StateWIP and nothing is read.
	probe probeFn
}

// builtins are the sources core ships or has declared. github and jira resolve
// through this package's own probes; linear and schedule are declared and not
// built, which is a fact the UI needs to render rather than an omission.
var builtins = []declaration{
	{kind: KindGitHub, probe: githubState},
	{kind: KindJira, probe: jiraState},
	{kind: KindLinear},
	{kind: KindSchedule},
}

// registry holds registrations from outside core, keyed by kind. Written once
// during single-threaded startup (an ee package's init / install closure, both
// of which run before delivery and the listener start), read from request
// handlers thereafter — the same no-mutex contract as routing's source
// registry.
var registry = map[string]Registration{}

// Register declares a source core does not ship. Panics on an empty or
// already-taken kind, on a kind core declares itself, or on a nil probe: every
// one of those is a wiring bug, and a boot failure beats a source that
// silently answers for another.
func Register(kind string, reg Registration) {
	if kind == "" {
		panic("eventsource.Register: kind must not be empty")
	}
	if slices.ContainsFunc(builtins, func(d declaration) bool { return d.kind == kind }) {
		panic("eventsource.Register: " + kind + " is declared by core and cannot be re-registered")
	}
	if reg.Probe == nil {
		panic("eventsource.Register: " + kind + " must supply a Probe")
	}
	if _, exists := registry[kind]; exists {
		panic("eventsource.Register: " + kind + " is already registered")
	}
	registry[kind] = reg
}

// Reset clears every registration made outside core (tests only).
func Reset() { registry = map[string]Registration{} }

// declarations returns every source this deployment can report on, in wire
// order: the sources TF has built first, then the ones it has only declared,
// each group by kind. A caller iterating the result renders a stable list
// whose head is the part of the vocabulary that can actually change.
func declarations() []declaration {
	local := runmode.Current() == runmode.ModeLocal
	out := make([]declaration, 0, len(builtins)+len(registry))
	out = append(out, builtins...)
	for kind, reg := range registry {
		if local && reg.MultiOnly {
			continue
		}
		out = append(out, declaration{kind: kind, probe: adapt(reg.Probe)})
	}
	slices.SortFunc(out, func(a, b declaration) int {
		if (a.probe == nil) != (b.probe == nil) {
			if a.probe == nil {
				return 1
			}
			return -1
		}
		return strings.Compare(a.kind, b.kind)
	})
	return out
}

// adapt wraps a registered source's Probe in the internal shape.
func adapt(p Probe) probeFn {
	return func(ctx context.Context, r *resolver) (State, error) { return p(ctx, r.tx, r.orgID) }
}

// Resolve answers for every source this deployment can report on for orgID.
// One resolver spans the pass, so the org's credentials are loaded once no
// matter how many sources are cut from them.
func Resolve(ctx context.Context, tx db.TxStores, orgID string) (Availability, error) {
	r := &resolver{tx: tx, orgID: orgID}
	decls := declarations()
	out := make(Availability, 0, len(decls))
	for _, d := range decls {
		st, err := r.state(ctx, d)
		if err != nil {
			return nil, err
		}
		out = append(out, Source{Kind: d.kind, State: st})
	}
	return out, nil
}

// StateFor answers for a single source, ok=false when kind is outside this
// deployment's vocabulary. It exists for the create-time gate, which is about
// exactly one source and has no reason to read the others — a source the
// caller is not asking about must not be able to fail their write.
func StateFor(ctx context.Context, tx db.TxStores, orgID, kind string) (State, bool, error) {
	decls := declarations()
	i := slices.IndexFunc(decls, func(d declaration) bool { return d.kind == kind })
	if i < 0 {
		return "", false, nil
	}
	st, err := (&resolver{tx: tx, orgID: orgID}).state(ctx, decls[i])
	if err != nil {
		return "", false, err
	}
	return st, true, nil
}

// resolver carries one pass's shared reads. The github and jira answers are
// both cut from the same credential bundle, so it is loaded lazily and at most
// once — a pass that resolves neither never touches the secret store.
type resolver struct {
	tx    db.TxStores
	orgID string

	creds       auth.Credentials
	credsLoaded bool
}

func (r *resolver) state(ctx context.Context, d declaration) (State, error) {
	if d.probe == nil {
		return StateWIP, nil
	}
	st, err := d.probe(ctx, r)
	if err != nil {
		return "", fmt.Errorf("resolve event source %q: %w", d.kind, err)
	}
	return st, nil
}

// credentials loads the org's integration bundle once per pass.
func (r *resolver) credentials(ctx context.Context) (auth.Credentials, error) {
	if r.credsLoaded {
		return r.creds, nil
	}
	creds, err := integrations.Load(ctx, r.tx.Secrets, r.orgID)
	if err != nil {
		return auth.Credentials{}, err
	}
	r.creds, r.credsLoaded = creds, true
	return creds, nil
}

// githubState is available exactly when a GitHub credential resolves — a
// stored PAT (the local env overlay folded in by the loader) or a registered,
// active App. It is the same derivation the setup gate reads, so the two
// cannot drift into disagreeing about whether GitHub is connected.
func githubState(ctx context.Context, r *resolver) (State, error) {
	creds, err := r.credentials(ctx)
	if err != nil {
		return "", err
	}
	ready, err := integrations.GitHubReady(ctx, r.tx.Orgs, r.tx.GitHubApps, r.orgID, creds)
	if err != nil {
		return "", err
	}
	if ready {
		return StateAvailable, nil
	}
	return StateUnconfigured, nil
}

// jiraState is available exactly when the org's stored auth-method marker
// resolves to a usable service credential. That reads the marker rather than
// key presence, which is why a Cloud org — which has no PAT at all — correctly
// reports available.
func jiraState(ctx context.Context, r *resolver) (State, error) {
	creds, err := r.credentials(ctx)
	if err != nil {
		return "", err
	}
	if _, ok := integrations.JiraSystemConfig(creds); ok {
		return StateAvailable, nil
	}
	return StateUnconfigured, nil
}
