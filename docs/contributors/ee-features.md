# Building an Enterprise Edition feature

Where code for an `ee/` feature goes, which core seams it registers into, and
the rules every EE feature follows.

## Placement

Core (`internal/*`, `cmd/*`) never imports `ee/`; only `package main` does.
`scripts/lint.sh` enforces this.

- **EE calling core needs nothing.** An EE package imports `internal/*` like
  any other consumer.
- **Core calling EE needs a seam.** If core would have to import your code, it
  is in the wrong place: register an implementation into a core-owned registry
  instead.

`ee/<feature>/` holds the feature itself: API clients, ingest handlers,
enforcement logic, its stores and domain behavior. Core may hold only:

1. **Generic seams**: registries any source could use. A seam names no EE type
   and does nothing until something registers.
2. **Inert declarations**: event type ids, metadata and predicate shapes, SQL
   migrations and RLS policies. Schema is the same for every install; gating
   happens at access time, never in DDL.
3. **Thin agent verbs**: a `triagefactory exec` subcommand that parses
   arguments and makes one host-side call.

## Seams

| Seam | Registration | What plugs in |
| --- | --- | --- |
| Entitlements provider | `entitlements.RegisterProvider` (once, from `ee.Install()`) | The license-backed answer behind `entitlements.For(orgID)` |
| Server routes | `server.RegisterExtension(name, install)` | HTTP routes behind the same session and CSRF handling as core routes |
| Transaction-bound stores | `db.RegisterStoreExtension(dialect, key, factory)` | Per-dialect store bundles available inside core transactions |
| Login hooks | `ExtensionAPI.SetLoginExtension` | Decisions inside the OAuth login path (SSO enforcement, just-in-time provisioning) |
| Event types | `events.Register(EventSchema{...})` | Metadata and predicate shapes, matcher, ownership |
| Routing | `routing.RegisterSource(prefix, SourceHooks{...})` | Which team owns an event, and whether a team tracks its scope |
| Entitlement gate | `entitlements.GateEventSource(prefix, feature)` | Dormancy for every event type of the source |
| Event publish | `SystemWithTx` → `ingest.Enqueue` → `PublishPreEnqueued` | Writing the event in the ingest's own transaction, then fanning it out |
| Event subscribe | `ExtensionAPI.Bus()` | Live-update consumers |
| Background workers | `ExtensionAPI.OnReady` | A long-running worker, started on the pod holding the background-brain lease |
| Agent verbs | `exec.RegisterSubcommand` + `agenthost.RegisterExtension` | An `exec` subcommand and its host-side handler |

The first four apply to any feature, the next five to a feature with its own
event source, and the last to a feature that gives agents verbs. `ee/slack`
is the reference for an event source with agent verbs; `ee/sso` for login
hooks.

## Rules

Registries panic at boot on a bad registration (duplicate event type,
duplicate or reserved routing prefix, missing hook); they never degrade at
dispatch. Beyond that:

- **Gate every handler on the entitlement.** Routes always mount; each handler
  checks `entitlements.For(orgID).Has(feature)` per request. A background
  worker registered with `OnReady` runs for every org and must check per org
  too.
- **`ResolveOwner` propagates failures.** Return `routing.OwnedBy(team)`,
  `routing.Unowned()` (looked, nothing owns it), or an error (could not look).
  The router requeues on an error and refuses a zero `OwnerResolution` with no
  error as an unreported failure.
- **`TracksScope` fails open** on a read failure (permissive answer plus a
  log): a briefly wide gate is corrected by the next event; a dropped event is
  lost.
- **Write the event in the transaction that consumes its input.** Use
  `api.Tx().SystemWithTx` to record the delivery, write the feature's rows and
  `ingest.Enqueue` the event together, then call `PublishPreEnqueued` after the
  commit. A failure rolls everything back, so the upstream's redelivery runs
  fresh.
- **`PublishPreEnqueued` and `Bus()` are nil until wiring finishes.** Use them
  in request handlers and workers, never in the install closure.

## Agent-facing CLI verbs

Delegated agents act through `triagefactory exec` subcommands, never by
calling external APIs themselves. An EE feature adds a verb family with two
registrations from its `init()`:

```go
exec.RegisterSubcommand("chat", exec.Subcommand{
	Run:        cli.Run,        // parses flags, makes one host call
	HelpText:   cli.HelpText,
	ValueFlags: cli.ValueFlags, // flags that take a value, so `--body "--help"` stays a payload
	SourceKind: "chat",         // the source the help index filters on
})
agenthost.RegisterExtension("chat", entitlements.FeatureChat, hostHandler)
```

The verb parses arguments and calls `host.CallExtension("chat", "post",
argsJSON)`. The host-side handler does the work and the audit writes, and the
entitlement check happens at that dispatch, so a verb cannot skip it. EE verbs
refuse when run from a plain CLI, which has no entitlement provider. Requests
and responses are one JSON frame each, within the IPC frame cap.

## Dormancy

`GateEventSource` makes an org without the feature (never had it, or lapsed)
see none of the source's event types, handlers, or blueprints whose every
trigger is gated, and refuses creating a handler on a gated type. The router
still records gated events but creates no tasks and fires no triggers.
Nothing is deleted: regaining the entitlement restores everything as it was.
On the EE side, stop ingesting when the entitlement is absent; the router's
gate covers races.

## Frontend

There is no `frontend/ee`. EE UI lands inside shared pages, rendered behind
`loaded && has(Feature...)` from the `useEntitlements` hook (a constant, never
a string) and absent otherwise: no upsell stubs or disabled placeholders.
Server-side filtering does the real hiding, so most features need no frontend
gating beyond their own settings surface.

## Checklist

1. Code in `ee/<feature>/`; dialect-split stores under
   `ee/<feature>/store/{lite,pg}`, registered with `db.RegisterStoreExtension`.
2. Feature constant in `internal/entitlements` (and `allFeatures`), mirrored in
   the frontend's `useEntitlements`. `feature_parity_test.go` fails if either is
   missing.
3. Migrations in the shared trees (`internal/db/migrations-*`).
4. With events: `events.Register`, `routing.RegisterSource`,
   `entitlements.GateEventSource`; enqueue in the consuming transaction and
   publish with `PublishPreEnqueued`.
5. Routes through `server.RegisterExtension`, each gated on the entitlement.
   Pre-auth webhook ingest uses `Raw` plus `PreAuthRateLimit` and verifies its
   own signature, or `Raw` plus `SignedWebhookRateLimit` when the sender signs
   every request and can exceed 1 request per second (the Slack Events API
   receiver).
6. With agent verbs: `exec.RegisterSubcommand` + `agenthost.RegisterExtension`.
7. A blank import in `package main`, next to the other EE imports.
