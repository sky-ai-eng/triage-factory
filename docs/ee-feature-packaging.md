# How to build a feature that lives in `ee/` (Enterprise Edition)

Where code goes, which seams core exposes, and the behavioral contract
(entitlement gating, dormancy) every EE feature follows.

## The placement rubric

**Core (`internal/*`, `cmd/*`) never imports `ee/`; only `package main`
does**. Dependencies point inward, so:

- **`ee/` calling core is free.** An EE package imports `internal/*` like any
  other consumer: stores, the event bus, domain types, `ExtensionAPI`. No seam
  needed in that direction.
- **Core calling into EE logic needs a seam.** If core
  would have to import your code, you've put it in the wrong place — register
  an implementation into a core-owned registry instead.

What belongs in **`ee/<feature>/`**: the feature's actual substance — API
clients, ingest handlers, resolvers, enforcement logic, its own store
implementations, domain behavior.

What's admissible in **core** (exactly three kinds of things):

1. **Generic seams** — registries any source could plug into (the table
   below). A seam names no enterprise type and does nothing until something
   registers.
2. **Inert declarations** — event-type ID constants, predicate/metadata
   schema shapes, SQL migrations and RLS policies. Schema is shared and
   present for every install (`events.event_type` has an FK into
   `events_catalog`, so catalog rows must exist universally regardless of
   entitlement); gating happens at access time, never in DDL.
3. **Thin proxies** — agent-facing CLI verbs (`triagefactory exec ...`): a
   registered subcommand whose body is arg parsing around one host-side call.
   Feature logic never runs in the agent's process — see "Agent-facing CLI
   verbs" below.

## The seams

| Seam (core)                      | Registration call                                                        | What plugs in                                                                                                                                                          |
| -------------------------------- | ------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `internal/entitlements` provider | `entitlements.RegisterProvider` (done once by `ee.Install()`)            | the license-backed per-org provider behind `entitlements.For(orgID)`                                                                                                   |
| Server routes                    | `server.RegisterExtension(name, install)`                                | HTTP routes mounted through the same session/CSRF wrap as core routes; `install` receives `ExtensionAPI`                                                               |
| Tx-bound stores                  | `db.RegisterStoreExtension(dialect, key, factory)`                       | per-dialect store bundles built inside core transactions, retrieved by a typed accessor on the EE side                                                                 |
| Login hooks                      | `ExtensionAPI.SetLoginExtension`                                         | decision hooks inside the core OAuth login path (SSO enforcement / JIT)                                                                                                |
| Event-type schemas               | `events.Register(EventSchema{...})` in `internal/domain/events`          | the feature's event types: metadata + predicate shape, matcher, and the per-type `Ownership` declaration                                                               |
| Routing behavior                 | `routing.RegisterSource(prefix, SourceHooks{ResolveOwner, TracksScope})` | how an _Owned_ event of this source resolves its owning team, and whether a team tracks the event's scope. Registration also marks the prefix router-bound (see below) |
| Entitlement gate                 | `entitlements.GateEventSource(prefix, feature)`                          | the dormancy contract for every event type of this source                                                                                                              |
| Event publish                    | `ExtensionAPI.PublishEvent(evt)`                                         | ingest → durable outbox → router, without touching the bus directly                                                                                                    |
| Event subscribe                  | `ExtensionAPI.Bus()`                                                     | bus subscriptions (live-update consumers)                                                                                                                              |
| Background workers               | `ExtensionAPI.OnReady`                                                   | a long-lived worker goroutine (connection manager, poller) started post-wiring with a shutdown-cancelling context                                                      |
| Agent CLI verbs                  | `exec.RegisterSubcommand` + `agenthost.RegisterExtension`                | an exec subcommand (arg parsing, ee-side) and its host-side logic, entitlement-gated at the `CallExtension` dispatch                                                   |

The first four apply to any feature. The next five are for a **feature with
its own event source** — one that ingests external signals and mints
entities/events/tasks. The last is for a feature that gives delegated agents
CLI verbs.

## Anatomy of a feature with events

Everything registers from one install path: an `init()` in the EE package
(reached by a blank import in `package main`) plus the `RegisterExtension`
install closure. Using a hypothetical `chat` source:

```go
package chat // ee/chat

func init() {
    // 1. Event types — classification is DATA, declared per type. Build the
    //    schema with events.NewSchema[Meta, Pred](eventType, ownership) —
    //    see internal/domain/events for the Metadata/Predicate shapes.
    events.Register(events.NewSchema[ChatMentionMetadata, ChatMentionPredicate](
        "chat:mention", events.OwnershipOwned))

    // 2. Routing behavior — resolution is BEHAVIOR, registered per source.
    //    Also marks "chat:" router-bound: internal/ingest enqueues its events
    //    into the durable outbox instead of leaving them bus-only.
    routing.RegisterSource("chat", routing.SourceHooks{
        ResolveOwner: resolveChannelOwner, // e.g. a channel→team lookup
        TracksScope:  teamTracksChannel,   // the stage-1 team↔resource gate
    })

    // 3. Dormancy — every chat:* type now requires the feature, org by org.
    entitlements.GateEventSource("chat", entitlements.FeatureChat)

    // 4. Routes — webhook ingest + settings API.
    server.RegisterExtension("chat", install)
}

func install(api server.ExtensionAPI) {
    h := &handler{tx: api.Tx(), az: api.Authz(), publish: api.PublishEvent}
    // Signed-webhook ingest is pre-auth: Raw + PreAuthRateLimit, handler
    // verifies the signature itself, then gates on the entitlement before
    // doing anything: if !entitlements.For(orgID).Has(FeatureChat) { drop }.
    api.Raw("POST /api/chat/events", api.PreAuthRateLimit(http.HandlerFunc(h.ingest)))
    // Session-gated surfaces go through API/APIMutating like any core route.
    api.API("GET /api/chat/channels", h.channelsList)
}
```

Rules the registries enforce (all panic at boot, never degrade at dispatch):

- `events.Register` — no duplicate types; `OwnershipRequestedParty` is
  github-only (its resolver is native to the GitHub review-request path).
- `routing.RegisterSource` — no empty/duplicate prefix; both hooks required
  (a pool-only source supplies a `routing.Unowned()` stub for `ResolveOwner`); the
  built-in prefixes `github`, `jira`, `system`, `webhook` are reserved and
  cannot be shadowed.

Ordering is structural, not fragile: a source's events can only originate from
its own registered ingest route, so its hooks are always in place before its
first event exists. Registries are written during single-threaded startup and
read from steady-state goroutines — same contract as `registeredExtensions`.

What a registered _Owned_ source inherits for free from the router: the full
owner-ladder semantics — visibility unions, `applies_to_unowned` watch
handlers, the member-over-watcher no-steal invariant, deterministic firing
order. `SourceHooks.ResolveOwner` only answers "who owns this occurrence";
everything downstream is shared machinery. Do not reimplement it.

The two hooks answer a **store failure in opposite ways**:

- `ResolveOwner` **decides** the owner, so a failed read **propagates**:
  `routing.Unowned()` means _resolved, nothing owns this_, and a non-nil error
  means _could not find out_ — the router requeues the event instead of
  routing it on a guess. Degrading a failure to the empty owner is
  indistinguishable from the real no-owner answer, and an ee source has no
  snapshot-diff behind it to re-emit the occurrence: it is lost, or it is
  handed to an `applies_to_unowned` watcher whose fire then consolidates
  ownership onto that watcher.
- `TracksScope` only **narrows** a set someone else computed, so it **fails
  open** (permissive result + log), mirroring core's `teamTracksEventRepo`. A
  briefly-too-wide gate that the next event corrects beats dropping legitimate
  work on a transient DB blip.

Data states a retry cannot change — metadata the source can't read, a
resource no team has claimed — are the resolved answer in both hooks, never
the error.

The `ResolveOwner` half is **enforced, not just documented**. Returning the
zero `OwnerResolution{}` with no error — the shape of `if err != nil { log;
return }` — is refused by the router as an unreported failure, so the event
replays and parks naming your source instead of being read as "nobody owns
this". Say which answer you mean:

```go
return routing.OwnedBy(team), nil   // this team owns it
return routing.Unowned(), nil       // I looked; nothing owns it
return routing.OwnerResolution{}, err  // I couldn't look
```

`ExtensionAPI.PublishEvent` and `Bus()` are read-through and nil until app
wiring completes: use them at request time (any mounted handler is safe),
never inside the install closure itself.

A feature that needs a long-lived background worker (a connection manager, a
poller) registers it via `ExtensionAPI.OnReady` during install; core fires the
hook every time this pod's background-brain lease starts — at
`TF_ROLE=all` / local mode that's once, at boot, indistinguishable from
before this ticket; under the control/standby split it's gated to exactly one
pod at a time, and fires again on a fresh ctx if this pod later re-acquires
the lease after a demotion. The ctx cancels on EITHER process shutdown OR
lease demotion — by the time the hook runs, `Bus()` and `PublishEvent` are
safe to use. The worker is not exempt from the dormancy contract: it must
gate its per-org work on `entitlements.For(orgID).Has(feature)` just like any
handler, since `OnReady` fires unconditionally (across every entitled and
unentitled org alike) for every install that currently holds the brain.

A worker that is genuinely safe to run replicated across every pod
(idempotent, no external side effects that would duplicate) opts into
`ExtensionAPI.OnReadyReplicaSafe` instead — fired once, unconditionally, at
boot, regardless of brain-lease state. This is an explicit opt-in with zero
callers today; default to `OnReady`.

## Entitlement gating: two shapes, one contract

**Out-of-core module (the default).** Routes always mount; each handler
gates per-request on `entitlements.For(orgID).Has(feature)` at its own
org-resolution seam. There is no boot-time feature gate — the same binary
serves entitled and unentitled orgs, and a lapsed org's requests fail closed
at the handler.

**In-core gated code (the exception).** Small amounts of core code may
check `For(orgID).Has(feature)` directly when the functionality can't cleanly
extract (cost caps inside the delegation path, audit surfaces inside core
handlers). Use sparingly; prefer the module shape for anything substantial.

**The dormancy contract** (what `GateEventSource` buys, enforced centrally —
a gated feature does not implement this itself):

- An org without the feature — never had it, or had it and lapsed; the two are
  deliberately identical — sees none of the source's event types in
  `/api/event-types` or `/api/event-schemas`, none of its handlers in the
  handler lists, and no blueprint whose _every_ trigger is gated (a blueprint
  with at least one ungated trigger stays visible).
- Creating a handler on a gated-off type is rejected; toggling/promoting an
  existing one is not (the firing gate makes it inert anyway).
- The router records gated events (the append-only log stays honest) but
  derives nothing: no close phase, no tasks, no trigger fires — including the
  post-scoring re-derive pass.
- **Nothing is deleted or rewritten.** Rows persist untouched; regaining the
  entitlement restores every surface exactly as configured. Existing tasks
  from a previously-entitled period are durable work and are never purged.

The in-`ee/` half of dormancy is just: gate ingest. When the entitlement is
absent the source stops producing events at all; the router-side freeze is
belt-and-suspenders for lapse races and stragglers.

## Agent-facing CLI verbs

Delegated agents act on TF state through `triagefactory exec ...` subcommands
rather than calling external APIs directly. Every verb routes through the
`cmd/exec/agenthost` seam: one `Client` interface with a host-local
implementation (local mode — the exec process is already on the host) and a
sandbox implementation that RPCs over a per-run unix socket to a daemon in
the server process. The socket is simultaneously the transport, the
credential (one socket per run, fs permissions), and the identity (the daemon
maps socket → run). The jail never holds a token; credentials, state access,
and audit writes all happen host-side.

An EE feature adds verbs with two registrations from its `init()`:

```go
// The verb family: arg parsing lives in the ee package. Core's dispatch is a
// map lookup after the built-in switch — the only things core learns from the
// registration are the docs it needs to serve --help: the family's help
// section, its value-taking flags (so `--body "--help"` stays a payload, not
// a help request), and the source kind the help index filters on.
exec.RegisterSubcommand("chat", exec.Subcommand{
	Run:        cli.Run,
	HelpText:   cli.HelpText,
	ValueFlags: cli.ValueFlags,
	SourceKind: "chat",
})

// The logic: runs host-side, keyed by namespace, gated by feature.
agenthost.RegisterExtension("chat", entitlements.FeatureChat, hostHandler)
```

The runner parses its flags and makes one call —
`host.CallExtension("chat", "post", argsJSON)`. Both transports funnel into a
single dispatch point that resolves the namespace, checks
`entitlements.For(<run's org>).Has(feature)`, and only then invokes the
handler. The gate lives in the seam, not in the verb, so a verb author
cannot forget it.

Two consequences worth knowing:

- **EE verbs are structurally inert in a locally-invoked CLI, server running
  or not.** CLI dispatch short-circuits before `ee.Install()` runs, so a CLI
  process never has an entitlement provider — every gated verb refuses
  there. Entitled execution happens only where identity and entitlements are
  real: the daemon inside the server process.
- **`exec --help` documents registered verbs, filtered by availability where
  an answer is in scope.** The registration's `HelpText`/`ValueFlags` are what
  the dispatcher serves `--help` from, at every depth and with a nil host —
  exactly the built-ins' contract, so help never needs run identity. The
  top-level index additionally resolves the org's available source kinds
  through the run's agenthost (the claim's stamped tools manifest in multi, a
  live resolve in local — the same answer the run's `<tools>` prompt section
  derives from) and omits a registered family whose `SourceKind` is not in
  it; a failed resolve, and an operator's bare terminal, fall back to the
  full listing (over-inclusion is the safe direction). The filter is
  index-only: `<name> --help` always answers, and execution stays gated by
  the extension dispatch's entitlement + source-disabled checks.

Payloads are one JSON frame in, one out (no streaming), within the IPC frame
cap — size verb responses accordingly.

## Frontend

There is no `frontend/ee`, deliberately. The SPA is a single bundle embedded
in every binary (`//go:embed`), exactly as `ee/` Go code compiles into every
community binary: no shipped artifact excludes EE code, so a frontend
directory boundary would have nothing to enforce. The Go-side `ee/` split
earns its keep by keeping core structurally independent of EE code — the
frontend has no analogous invariant to protect, and UI features land as rows
inside shared pages (a settings card, a nav item, a status chip), which a
hard boundary would turn into a plugin-registry exercise for no payoff.

The invariant is the gate, not the location: **every EE surface renders
behind `loaded && has(FeatureSSO)`** (a `Feature*` constant from the
`useEntitlements` hook over `/api/entitlements`, never a bare string —
mirrors the backend's typed `Feature` enum so a typo fails at compile/lint
time instead of silently gating nothing) **and degrades to absence** — no
upsell stubs or disabled placeholders — matching the backend's 404-and-hide non-disclosure
posture. The `loaded` half keeps gated surfaces from flashing before the
probe resolves. Server-side list filtering (the dormancy contract above)
does the real hiding; the FE simply renders what the API returns, so gated
features generally need **no** FE gating logic beyond their own
settings/landing surfaces. Should a core-only frontend artifact ever become
a requirement, the gates mark every EE surface — extraction is a grep for
the feature constants, not an archaeology project.

## Checklist for a new EE feature

1. Code in `ee/<feature>/`; stores under `ee/<feature>/store/{lite,pg}` if
   dialect-split (register via `db.RegisterStoreExtension`).
2. Feature constant in `internal/entitlements` (+ `allFeatures`), mirrored in
   the frontend `useEntitlements` hook. Both edges are CI-guarded: the
   composition-root parity test (`feature_parity_test.go`) fails if a
   registered feature is missing from `allFeatures` or from the hook.
3. Migrations in the shared trees (`internal/db/migrations-*`) — schema is
   universal, access is gated.
4. If it has events: `events.Register` (with `Ownership`),
   `routing.RegisterSource`, `entitlements.GateEventSource`, publish via
   `ExtensionAPI.PublishEvent`.
5. Routes via `server.RegisterExtension`; per-request entitlement gating in
   every handler; pre-auth ingest through `Raw` + `PreAuthRateLimit` with its
   own signature verification — or `Raw` + `SignedWebhookRateLimit` when the
   sender authenticates every request itself and its legitimate delivery
   volume can exceed the human-login tier's 1 req/s budget (e.g. the Slack
   Events API receiver).
6. If it gives agents CLI verbs: `exec.RegisterSubcommand` +
   `agenthost.RegisterExtension` — logic and audit writes in the host-side
   handler, never in the verb.
7. Blank import in `package main` (alongside the existing EE package imports).
8. Nothing in `internal/*`/`cmd/*` imports the package — the lint boundary
   guard is the backstop, not the check.
