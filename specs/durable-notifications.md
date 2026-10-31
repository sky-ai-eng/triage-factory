# Durable notifications — design

Read this before adding a notification kind, raising one from a producer, or touching `internal/notify`, the `notifications` / `notification_marks` tables, or the notifications page. It records the design worked out for TFAC-878. Everything under **Decisions** is settled; the remaining questions are listed under **Open questions**.

---

## The gap

TF has two ways to tell a person something, and neither fits a condition that is quietly and persistently wrong.

- **Toasts** are ephemeral and connection-scoped. A condition that recurs every poll cycle would fire a toast every cycle, and a person who was not looking never learns it happened.
- **The event bus and websocket** are internal pub/sub and UI invalidation. They are not a policy-aware feed.

So nothing answers: _this team's configuration has been broken for three weeks — who is told, and where do they see it?_ The known cases are the `TODO(TFAC-878)` markers in the code and the consumers listed in the ticket and its comments (see **First consumers**).

## Principles

1. **A notification is an occurrence of a condition.** It is open while the condition is true, the system resolves it when the condition stops being true, and a recurrence while it is open coalesces into the same row (first seen, last seen, count). A **notice** — something that happened once and leaves no state behind — is the one-shot case: nothing resolves it, it expires.
2. **Kinds are defined in code.** A kind is a Go value registered next to its definition. Nothing in V1 reads or writes `events`, `events_catalog`, `event_handlers`, or the event queue, and nothing is added to the events catalog for a notification's sake.
3. **The kind definition carries everything of importance.** Audience, priority, dismissibility, copy, links, actions, coalescing, websocket delivery and failure handling are declared once on the kind or implemented once in `internal/notify`. A producer integrates with one or two lines.
4. **Audience is resolved at read time from the row and re-resolved on every write.** No per-recipient rows are written. A new admin sees the org's open admin notifications; a removed member stops seeing a team's.
5. **The system resolves; people read and dismiss.** Resolution is never a user action. Read and dismissed are per-person marks no matter what the scope of the notification is.

## Why not event handlers (V1)

Making notifications a third `event_handlers` kind (beside rules and triggers) was considered and rejected for V1. It would put every condition into the events catalog — the GitHub App becoming unusable, a Linear team becoming unreadable, a Jira mirror failing — although none of them is something an automation should run on, the set cannot be predicted ahead of time, and the catalog would change every time a new consumer appears. It also meets constraints in the router: entity-less events are dropped (`routableEntity` → `DispositionTasklessUnroutable`), `system:*` types never reach the durable queue, handlers are team-only (`event_handlers.team_id NOT NULL`), handlers are forward-only, and each producer would have to detect its own transitions to avoid writing an event per poll cycle.

Event-driven notifications remain the plan for user-configured rules on events that already exist for automation ("notify me when CI fails on my PR"). They write into the same tables with `origin = 'event'`; see **Event notifications**.

## Vocabulary

| Term           | Meaning                                                                                                                              |
| -------------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| **Kind**       | A registered notification type, e.g. `linear.team_unreadable`. Its name is stored on rows and never renamed.                         |
| **Key**        | The occurrence's identity within its kind, built from ids (never names) — e.g. the Linear team id.                                   |
| **Occurrence** | One row: one kind + key, for one audience, from opening to resolution. A condition that resolves and comes back is a new occurrence. |
| **Audience**   | Who a row is for: `org` / `team` / `user` scope, with role `members` or `admins` for org and team.                                   |
| **Lifecycle**  | `condition` (the system resolves it) or `notice` (it expires).                                                                       |
| **Origin**     | `system` (a code-defined kind) or `event` (a notify rule on an event; see **Event notifications**).                                  |
| **Marks**      | One person's `read_at` / `dismissed_at` on a row.                                                                                    |

## Decisions

| #   | Decision                                                                                                                                                                                                                                                                                                                        | Reason                                                                                                                                                                                                                                                 |
| --- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| 1   | Dismissal is **per person**.                                                                                                                                                                                                                                                                                                    | A shared dismiss can only hide a condition that is still true — the condition resolves itself when fixed — so one person's "later" would hide it from everyone.                                                                                        |
| 2   | **Dismissibility is a property of the kind.** A kind about something TF requires (GitHub; the background-jobs model) is not dismissible and stays until the system resolves it. A kind about something an org may legitimately stop caring about (Jira, Linear, mirrors, drift) is dismissible. Notices are always dismissible. | "We are moving off Jira" is a legitimate answer; "we are not using GitHub" is not. Mirrors the line `eventsource` draws between disableable and required sources.                                                                                      |
| 3   | **Turning a source off or disconnecting its credential resolves that source's open conditions.**                                                                                                                                                                                                                                | A turned-off source does not poll, so nothing else would ever resolve them, and they no longer apply.                                                                                                                                                  |
| 4   | **Org admins do not inherit team-scoped notifications.** They can read them (an "All teams" view), but they are not sent them. A producer that needs org admins told names them as an audience.                                                                                                                                 | Inheriting every team's notifications in a large org trains admins to clear the page unread, including the org-level urgent ones only they can fix. Every team has at least one admin (`guard_team_admins`), so no team notification is undeliverable. |
| 5   | **Notices are in scope**, under three rules: a notice is only for something that leaves no state behind (a suspended prompt is a condition, not a notice); a notice must carry an action; notices default to `low` and expire after 14 days.                                                                                    | TFAC-518 needs them; the rules keep the page from becoming a feed.                                                                                                                                                                                     |
| 6   | **Three priorities, each defined by what it does** (see **Priority**).                                                                                                                                                                                                                                                          | A level with no distinct behavior is not a level.                                                                                                                                                                                                      |
| 7   | **A full page**, reached from a rail row; no drawer in V1.                                                                                                                                                                                                                                                                      | A drawer cannot hold the filters, history and actions the page needs.                                                                                                                                                                                  |
| 8   | **Copy is rendered by the server at write time** and refreshed on every re-raise.                                                                                                                                                                                                                                               | Headless clients and later out-of-app delivery need the text; a re-raised condition keeps its names current.                                                                                                                                           |
| 9   | **`origin` ships in V1** although V1 only writes `system`.                                                                                                                                                                                                                                                                      | The Postgres baseline is still editable; adding the column now is cheaper than later.                                                                                                                                                                  |

## Priority

| Priority | Badge (rail `alert`) | Shell banner                      | Meaning                                                                                              |
| -------- | -------------------- | --------------------------------- | ---------------------------------------------------------------------------------------------------- |
| `urgent` | yes                  | yes, while open and not dismissed | Work has stopped for this audience: polling stopped, every task blocked.                             |
| `high`   | yes                  | no                                | Something is degraded and nothing else says so: a project produces no work, a mirror does not write. |
| `low`    | no                   | no                                | Information; nothing is broken.                                                                      |

A kind's priority is a function of its params, so one kind can be `urgent` for one cause and `high` for another (e.g. memory generation failing because no model is configured vs. a provider backing off). A raise that increases an open row's priority sets `escalated_at`, which makes the row unread again and brings it back for anyone who dismissed it. A decrease does neither.

## The interface

This is the part that has to stay small. Every consumer uses one of three verbs on a kind value, chosen by how the producer learns the truth:

| Verb                                      | Use when                                            | Behavior                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| ----------------------------------------- | --------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `Set(ctx, orgID, p, active bool)`         | The producer evaluates a yes/no each time it looks. | `true` opens or bumps the occurrence for `p`'s key; `false` resolves it. Idempotent — **call it on every evaluation, not only on a change**, so a failed write corrects itself on the next one.                                                                                                                                                                                                                                            |
| `Sync(ctx, orgID, active []P, keep ...P)` | The producer evaluates a whole set in one pass.     | Opens or bumps every member of `active`; leaves the open occurrences of `keep` as they are; resolves every other open occurrence of the kind in the org. `keep` is the members the pass has no answer for (a request rate-limited or never answered, or a failure still short of the producer's debounce), so they neither raise nor resolve. **Only on a complete pass** — a pass that could not reach the upstream at all calls nothing. |
| `Post(ctx, orgID, p)`                     | Notices.                                            | Opens the notice, or bumps an unexpired one with the same key and extends its expiry.                                                                                                                                                                                                                                                                                                                                                      |

`notify.Condition` returns a `*ConditionKind[P]`, which has `Set` and `Sync`; `notify.Notice` returns a `*NoticeKind[P]`, which has `Post`. The compiler rejects the wrong verb. None of them returns a value: a failure is logged and counted in `tf_notify_write_failures_total{kind}` inside `notify`, and never fails the work that raised it.

A `*ConditionKind[P]` also has one read, `OpenOrgs(ctx) ([]string, error)`: the orgs, across the deployment, holding an open occurrence of the kind. A producer evaluates only the orgs it has a reason to look at, and an occurrence stays open until that producer evaluates its org again. So a producer whose reason to look can disappear while an occurrence is open (the memory provisioner looks at orgs owing a memory) re-evaluates every org `OpenOrgs` names on its own cadence. Unlike the verbs it returns its error, because only the caller knows what a missing answer costs. Before `Install` it returns nothing.

### Call sites

```go
// internal/poller — every reconcile, not only on a change; zero or one member,
// so the occurrence of an App the org no longer holds (replaced, or given up
// for the deployment App) resolves
kinds.GitHubAppUnusable.Sync(ctx, orgID, unusable)

// internal/tracker/linear.go — a complete visibility pass over every armed team
kinds.LinearTeamUnreadable.Sync(ctx, orgID, unreadable)

// internal/tracker/tracker.go — projects past the debounce raise; one Jira did
// not answer, or still short of the debounce, is kept rather than resolved
kinds.JiraDiscoveryFailing.Sync(ctx, orgID, failing, keep...)

// internal/delegate/jira_mirror.go — true where the canonical is no longer one of
// the project's statuses, false after a successful move; a transient error, or a
// ticket that can't reach a status the project still has, does neither
kinds.JiraMirrorFailing.Set(ctx, orgID, kinds.JiraMirrorFor(rule, issueKey), true)

// internal/memoryprovision/failing.go — every 30 seconds, in each org an attempt
// has closed in since and each org MemoryGenerationFailing.OpenOrgs names; the
// store read applies the debounce and the window
kinds.MemoryGenerationFailing.Sync(ctx, orgID, kinds.MemoryFailuresFor(failing))

// internal/routing — the conflict TFAC-518 surfaces
kinds.AutomationConflict.Post(ctx, orgID, kinds.Conflict{TaskID: task.ID, Winner: winner, Losers: losers})

// the source off-switch and each credential disconnect — once each, for every kind of that source
notify.ClearSource(ctx, orgID, "jira")
```

A call site never builds text, an audience, a link, or a key. A review that finds one of those at a call site sends it back to the kind.

### A kind definition

```go
// internal/notify/kinds/linear.go
type LinearTeam struct {
	LinearTeamID string `json:"linear_team_id"`
	TeamKey      string `json:"team_key"` // display only
}

var LinearTeamUnreadable = notify.Condition(notify.ConditionSpec[LinearTeam]{
	Name:        "linear.team_unreadable",
	Source:      "linear",
	Priority:    notify.Always(notify.High),
	Dismissible: true,
	Key:         func(t LinearTeam) string { return t.LinearTeamID },
	Audience: notify.Audiences(
		notify.OrgAdmins(),
		notify.TeamsTrackingLinearTeam(func(t LinearTeam) string { return t.LinearTeamID }, notify.Admins),
	),
	Render: func(t LinearTeam, to notify.Recipient) notify.Content {
		c := notify.Content{Title: "Linear team " + t.TeamKey + " can't be read", Body: "…"}
		settings := notify.Link("Open Linear settings", notify.Routes.OrgSettings())
		if to.TeamID == "" {
			c.Actions = []notify.Action{settings}
			return c
		}
		// A team admin can untrack, and the settings page is an org admin's,
		// so the invoke is a team row's primary action.
		c.Actions = []notify.Action{
			notify.Invoke("untrack", "Stop tracking "+t.TeamKey, notify.RequiresTeamAdmin, notify.Confirm("…")),
			settings,
		}
		return c
	},
	Example: LinearTeam{LinearTeamID: "00000000-0000-0000-0000-000000000000", TeamKey: "ENG"},
})
```

| Spec field    | Condition | Notice   | Notes                                                                                                                             |
| ------------- | --------- | -------- | --------------------------------------------------------------------------------------------------------------------------------- |
| `Name`        | ✓         | ✓        | Stable; stored on rows.                                                                                                           |
| `Source`      | optional  | optional | An event-source kind. `ClearSource` resolves a condition by it.                                                                   |
| `Priority`    | ✓         | ✓        | `notify.Always(p)` for a constant; `notify.PriorityOf(func(P) Priority)` when it depends on the params. Notices default to `Low`. |
| `Dismissible` | ✓         | —        | Notices are always dismissible.                                                                                                   |
| `TTL`         | —         | ✓        | Required: `notify.DefaultNoticeTTL` (14 days) unless the kind has a reason for another.                                           |
| `Key`         | ✓         | ✓        | Ids only.                                                                                                                         |
| `Audience`    | ✓         | ✓        | Composed from the constructors below.                                                                                             |
| `Render`      | ✓         | ✓        | `func(P, Recipient) Content`. Receives the audience being rendered, so a team row can carry an action an org row does not.        |
| `Example`     | ✓         | ✓        | A representative `P`, used by the kinds conformance test and a dev page that renders every kind.                                  |

**Where kinds live.** `internal/notify/kinds`, one file per source, so every audience and every line of copy is reviewed in one place. `kinds` imports `notify` and `domain`, never a producer package. `ee/` defines its kinds in its own packages with the same constructors; core never imports them.

**Audience constructors** (V1): `OrgMembers()`, `OrgAdmins()`, `TeamMembers(fn)`, `TeamAdmins(fn)`, `Teams(fn, role)` (every team a list in the params names, one audience per team), `User(fn)`, `TeamsTrackingLinearTeam(fn, role)`, `TeamsArmingJiraProject(fn, role)`, combined with `Audiences(...)`. The tracking resolvers read the tracked sets (for Jira, the rules that arm a project) through stores handed to `notify.Install`, skip archived teams, and are re-run on every write. A resolution failure skips the write (logged, counted) and never falls back to a wider audience; `Set` and `Sync` retry it on the next evaluation.

### What `notify` does so a call site does not

- Upserts the open occurrence under the partial unique index (coalescing), bumping `last_seen_at` and `seen_count` and refreshing title, body, actions and params.
- Resolves the audience; inserts rows for audiences newly named and resolves open rows for audiences no longer named (`resolved_reason = 'audience_removed'`) — so a team that stops tracking a subject stops being told about it.
- Sets `escalated_at` on a priority increase.
- Stores `P` as the row's `params` and hands it back typed to action handlers.
- Sends the websocket ping on open, escalation and resolution only.
- Does all of a call's writes in one transaction.

### Wiring

`app.New` calls `notify.Install(backend)` once — the store, the audience stores, the hub — the same shape as `entitlements.RegisterProvider`. Nothing is threaded through constructors. Before `Install`, every verb is a no-op: that is the correct behavior for the host-cli and jail-cli identities, and a boot test asserts the server installs it.

**Invoke handlers** are the one integration that is more than a line. The kind declares the action (id, label, required permission, optional confirmation text); the effect is registered once at boot, beside the code that owns the operation:

```go
kinds.LinearTeamUnreadable.HandleAction("untrack", s.untrackUnreadableLinearTeam)
```

`untrackUnreadableLinearTeam` writes through the functions the tracked-set route uses (its gate, `authz.Checker.TeamAdminWrite`, and its write, `saveTeamLinearTeams`), so validation and authorization exist once. A refusal from them is an `*httpx.Fault`, which the invoke route answers with as the tracked-set route would. It receives the request context carrying the caller's claims and a typed `notify.ActionCall[P]` (the row's org, the caller, the row's recipient, the decoded params) and returns whether the effect resolved the condition; if so, `notify` resolves the row the action was invoked on immediately, and the occurrence's other audiences' rows wait for the producer's next evaluation. The dispatcher the route calls is `notify.Caller`: `Actions` for the read, `RunAction` for the invoke, one permission check behind both. Handlers are registered from `internal/server` (in `server.New`) or `internal/app` (in `buildNotify`) because `kinds` cannot import them. A boot test fails if any `Invoke` id a kind's `Example` renders has no handler.

### Tests

```go
rec := notifytest.Record(t)
// … run the producer …
rec.Active(t, orgID, kinds.LinearTeamUnreadable)
```

The recorder is installed for the test and its assertions filter by the test's own org id, so parallel tests do not see each other. It answers `OpenOrgs` from every installed recorder, so a producer test that re-evaluates those orgs can reach another test's. A conformance test in `kinds` renders every kind's `Example` for every audience it can produce and checks: a non-empty title, at most two actions, every notice has an action, every `Link` route resolves, and no two kinds share a name.

## Data model

Postgres changes go into the baseline; SQLite gets a forward migration. Both behind one `db.NotificationStore` with `dbtest` conformance cases, following the store standards (writes return the stored row via `RETURNING`; system writes are `...System` methods on the admin pool; reads run under the request's claims).

### `notifications`

| Column                          | Type             | Notes                                                                               |
| ------------------------------- | ---------------- | ----------------------------------------------------------------------------------- |
| `id`                            | uuid             |                                                                                     |
| `org_id`                        | uuid             |                                                                                     |
| `origin`                        | text             | `system` \| `event`.                                                                |
| `lifecycle`                     | text             | `condition` \| `notice`. CHECK: `notice` ⇔ `expires_at IS NOT NULL`.                |
| `kind`                          | text             | No FK; the registry is code.                                                        |
| `key`                           | text             |                                                                                     |
| `audience_scope`                | text             | `org` \| `team` \| `user`                                                           |
| `audience_role`                 | text NULL        | `members` \| `admins` for org/team; NULL for user. CHECK on the shape.              |
| `team_id`                       | uuid NULL        | Set iff scope is `team`. FK `teams`.                                                |
| `user_id`                       | uuid NULL        | Set iff scope is `user`. FK `users`.                                                |
| `source`                        | text NULL        | Event-source kind, for `ClearSource`.                                               |
| `priority`                      | text             | `urgent` \| `high` \| `low`                                                         |
| `dismissible`                   | boolean          | Copied from the kind, so the read side and the marks PATCH never need the registry. |
| `title`, `body`                 | text             | Plain text. Never rendered as markup — it can contain externally authored names.    |
| `actions`                       | jsonb            | At most two; the first is primary.                                                  |
| `params`                        | jsonb            | The kind's `P`: ids and display values. Never shown.                                |
| `first_seen_at`, `last_seen_at` | timestamptz      |                                                                                     |
| `seen_count`                    | integer          |                                                                                     |
| `escalated_at`                  | timestamptz      | `first_seen_at` until the priority first increases.                                 |
| `resolved_at`                   | timestamptz NULL |                                                                                     |
| `resolved_reason`               | text NULL        | `cleared` \| `source_off` \| `audience_removed`                                     |
| `expires_at`                    | timestamptz NULL | Notices only.                                                                       |
| `entity_id`                     | uuid NULL        | Set iff `origin = 'event'`: the entity the event is about. FK `entities`.           |

Indexes: a partial unique index on the open occurrence — `(org_id, origin, kind, key, audience_scope, coalesce(audience_role,''), coalesce(team_id, <nil uuid>), coalesce(user_id, <nil uuid>)) WHERE resolved_at IS NULL`, a list index on `(org_id, resolved_at, priority, escalated_at, id)`, and, in Postgres, a partial index on `(kind, origin, org_id) WHERE lifecycle = 'condition' AND resolved_at IS NULL` for `OpenOrgs`, which reads across orgs.

### `notification_marks`

| Column            | Type             | Notes                                                          |
| ----------------- | ---------------- | -------------------------------------------------------------- |
| `notification_id` | uuid             | FK, `ON DELETE CASCADE`                                        |
| `user_id`         | uuid             |                                                                |
| `org_id`          | uuid             | For RLS.                                                       |
| `read_at`         | timestamptz NULL |                                                                |
| `dismissed_at`    | timestamptz NULL | Refused (422 `INVALID_FIELD`) when the row is not dismissible. |

Primary key `(notification_id, user_id)`. Rows exist only for notifications someone has acted on.

- **Unread:** no `read_at`, or `read_at < escalated_at`.
- **Dismissed:** `dismissed_at` set and `dismissed_at > escalated_at`.

### Two predicates: who may read, who is sent

The spec states both because they differ (Decision 4).

- **May read** (RLS on `notifications`, both roles via the existing `tf.*` helpers): org-scope `members` rows for any org member; org-scope `admins` rows for org admins; team-scope rows for that team's members (`members` role) or admins (`admins` role), **and for org admins**; user-scope rows for that user. The app role cannot insert or update `notifications`; every write is a system write. `notification_marks` is readable and writable only by its own `user_id`, and only for a notification the user may read.
- **Is sent** (the inbox query): the same, without the org-admin read-through on team rows, excluding archived teams and expired notices.

### Retention

A brain-side GC deletes rows 30 days after resolution, and notices once expired. Marks go with them.

## HTTP API

The inbox is viewer-relative, so it lives under `/api/me` and answers for the org the credential carries.

| Route                                                 | Purpose                                                                                                                                                                                                                                         |
| ----------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `POST /api/me/notifications/list`                     | The caller's inbox. Filters: `status` (`open` \| `resolved`), `dismissed` (bool), `unread` (bool), `priority_in`, `scope_in` (`org` \| `team` \| `user`). An absent filter does not filter. Standard paging; `page_size: 0` is the badge count. |
| `GET /api/me/notifications/{id}`                      | One notification the caller may read, with the caller's marks. 404 otherwise.                                                                                                                                                                   |
| `PATCH /api/me/notifications/{id}`                    | The caller's marks: `{"read": bool, "dismissed": bool}`. Absent keeps a mark; `false` (or `null`) clears it. Answers the item as the caller now sees it.                                                                                         |
| `POST /api/me/notifications/mark-read`                | `{"ids": [...]}` — the rows the client actually rendered, so a row that arrived after rendering is not marked. Multi-row transition, hence a verb route. Answers `{"ids": [...]}`, the ids it marked; one the caller cannot read is left out.   |
| `POST /api/me/notifications/{id}/actions/{action_id}` | Run a declared invoke action. No body. 403 when the caller lacks the action's permission; 404 for an unknown action id or an unreadable row; 409 once the condition has resolved; a handler's own refusal (`*httpx.Fault`) with its status. Answers the item as the caller now sees it, or 204 when the row has since left what they may read.  |
| `POST /api/orgs/{org_id}/notifications/list`          | Org admins: every notification in the org, team rows included (the "All teams" view). Same filters plus `team_id_in`. Items carry the caller's marks.                                                                                           |

The badge is `status=open, dismissed=false, unread=true, priority_in=[urgent, high], page_size=0`. The banner is `status=open, dismissed=false, priority_in=[urgent]`.

An item:

```json
{
  "id": "…",
  "kind": "linear.team_unreadable",
  "lifecycle": "condition",
  "priority": "high",
  "audience": { "scope": "team", "role": "admins", "team_id": "…", "user_id": null },
  "title": "Linear team ENG can't be read",
  "body": "…",
  "dismissible": true,
  "first_seen_at": "…",
  "last_seen_at": "…",
  "seen_count": 412,
  "resolved_at": null,
  "resolved_reason": null,
  "expires_at": null,
  "actions": [
    {
      "id": "org_settings",
      "type": "link",
      "label": "Open Linear settings",
      "href": "/orgs/…/org?tab=settings"
    },
    {
      "id": "untrack",
      "type": "invoke",
      "label": "Stop tracking ENG",
      "confirm": "…",
      "allowed": true
    }
  ],
  "read": false,
  "dismissed": false
}
```

### Actions

- **`link`** — `href` is an in-app path built by `notify.Routes` (one Go table of the SPA's routes, rendered with the org prefixed, so API and later email clients get a working address) or an external URL from `notify.External`. External URLs are accepted only on the org's configured hosts (`github_base_url`, `jira_base_url`, Linear while a workspace is connected), with no dot segments in the path, and checked when the row is written. A rendering that is refused fails a `Set` or `Post`; in a `Sync` it skips only that member, whose key is kept so its open row is neither bumped nor resolved. A link's id is its route's name, or `external`. A test holds the route table to `frontend/src/main.tsx`.
- **`invoke`** — runs a registered handler (see **Wiring**). Only a condition carries one; a notice's actions are links. The declared permission (`RequiresOrgAdmin`, or `RequiresTeamAdmin`, which only a team row's action may declare) is stored on the row. `allowed` is computed per viewer at read time from it, and is true only while the condition is open, the viewer holds the permission and the process has the handler, so a client never offers a button that answers 403, 404 or 409. A team member can see "team unreadable" without being allowed to untrack the team.

## Websocket

One invalidation ping, `notifications_updated`, with no payload. It is sent when a row opens, escalates or resolves, and to the acting user when their own marks change (other tabs). It is **not** sent when `last_seen_at` is bumped — that happens on every evaluation and is the write rate the websocket rules exist to keep off the hub.

The audience is resolved at emit time, as in `internal/knowledgeevent`: an `org`/`members` row is one org-scoped envelope; every other row fans out one envelope per user id. A resolution failure drops the envelope (the client refetches on its next ping or navigation) rather than widening it. The client refetches the badge count, the banner query, and the page's list if it is open.

Every producer can send the ping from the process it runs in. `notify.Install` takes the app's hub, and in multi mode every pod builds the websocket backplane (`internal/app/backplane.go`), so a broadcast on any pod reaches sockets on every control pod. The mirrors run where delegations are minted: the router's firing path on the brain and the task-actions handler, both on control pods. `announceTaskPlacement` already broadcasts `task_updated` through the spawner's hub just before it starts them. The claim guards run in the API handler.

## UI requirements

The visual design is TFAC-1239, under the UI V1 epic. It starts from scratch; the existing toast UI is not a reference. What the surfaces must hold:

- **Rail row** "Notifications" in the work section, in both modes, using the rail's `alert` mark for the badge count. (The `alert` field exists for exactly this: "a thing that happened and needs somebody".) Its position within the section is TFAC-1239's call.
- **Page** at `/orgs/:org_id/notifications` (local: `/notifications`). Default view: open and not dismissed, sorted by priority, then `escalated_at` newest first. Filters: status, dismissed, priority, scope ("Org", "My teams", "You"; org admins also "All teams"). Each item shows its priority, its scope ("Org", "Team Infra", "You"), title and body, how long the condition has been true (from `first_seen_at`), when it was last seen, its actions, and dismiss when dismissible. Resolved items show when and why they resolved. Items are addressable by URL so the banner, the arrival pop and later emails can link to one.
- **Read**: the page marks the items it renders as read (`mark-read` with their ids).
- **Urgent banner** across the shell for open, undismissed urgent rows, linking to the item (or to the page when there are several).
- **Arrival pop**: a short, transient display when a ping brings a new open `urgent` or `high` row while the person is in the app.

## Local mode

The same calls, tables (SQLite) and page. Local is N=1: the one user is the recipient of every audience, there is no RLS, and the audience constructors resolve to that user: a kind with several audiences writes one user-scoped row.

## First consumers

| Kind                          | Source | Audience                                    | Priority                                            | Dismissible | Verb and site                                                                                                                     | Actions                                                                       |
| ----------------------------- | ------ | ------------------------------------------- | --------------------------------------------------- | ----------- | --------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------- |
| `github.app_unusable`         | github | org admins                                  | urgent                                              | no          | `Sync` on every reconcile (`internal/poller/manager.go`, beside `logAppUsabilityChange`); an active App only                      | Link: GitHub access settings                                                  |
| `linear.team_unreadable`      | linear | org admins + admins of TF teams tracking it | high                                                | yes         | `Sync` on a complete visibility pass over every armed team (`checkLinearTeamVisibility`; never on outcome `failed`)               | Link: Linear settings; invoke "Stop tracking" on team rows                    |
| `jira.discovery_failing`      | jira   | admins of teams whose rules arm the project | high                                                | yes         | `Sync` from `discoverJira` after 3 rejected cycles in a row; unanswered projects kept                                             | Link: the team's Jira status rules                                            |
| `jira.mirror_failing`         | jira   | team admins                                 | high                                                | yes         | `Set` in `runJiraMirror`, `applyJiraClaim` and the stock deck's claim; a rules save resolves it                                   | Link: the team's Jira status rules                                            |
| `linear.mirror_failing`       | linear | team admins                                 | high                                                | yes         | `Set` in `runLinearMirror` and `applyLinearClaim`; a rules save resolves it                                                       | Link: the team's Linear rules                                                 |
| `memory.generation_failing`   | —      | org admins                                  | urgent for `no_model`, high otherwise               | no          | `Sync` in the provisioner: once failures since a generated memory span 2 min, `error_kind`s within 5 min of the newest            | Link: background-jobs model setting; invoke "Retry now" (rings `memory_owed`) |
| `jira.status_drift`           | jira   | team admins                                 | high when a rule stops working, low otherwise       | yes         | `Sync` from the hourly drift check in the Jira cycle; `Set` from a rules save that reads the workflow                             | Link: the team's Jira status rules                                            |
| `jira.predicate_drift`        | jira   | team admins                                 | high when the filter matches nothing, low otherwise | yes         | `Sync` from the hourly drift check; `Set` from a task rule or trigger save that changes its filter or enabled flag, and on delete | Link: the task rule (`/triage?rule=`) or trigger (`/prompts?trigger=`)        |
| `routing.automation_conflict` | —      | admins of the losing teams                  | low                                                 | (notice)    | `Post` in `fireMatchedTriggers` for a conflict decided at event time; in `processReDerive` after the re-derive commits            | Link: the losing team's trigger                                               |

Debounce is each producer's responsibility, using evidence it already has (the attempts ledger, consecutive cycles). `notify` coalesces; it does not wait. Each consumer's ticket sets its producer's thresholds.

### Jira drift

Jira configuration names values Jira can take away. Detection is `internal/jiradrift`; the two kinds above are its output.

- **What drifts.** A team's status rules (`jira_project_status_rules`) name statuses of a project's workflow; a status deleted in Jira or retired from the workflow leaves a rule naming nothing. A Jira task rule's or trigger's filter names statuses, issue types and priorities by id, checked only when a save adds them; a value deleted in Jira, dropped from a workflow, or left on a project the team no longer tracks leaves a filter that never matches it.
- **Two kinds**, because the keys and the fixes differ: `jira.status_drift` is keyed by the rule row (team + project) and fixed on the team's Jira source; `jira.predicate_drift` is keyed by the handler and fixed in its editor. One occurrence lists every gone value of its row or handler.
- **Matching.** Nothing is matched on a name a person typed. A rule's refs compare as `JiraStatusRef.SameStatus` does (by id; by name only for a legacy ref with no id), and a filter's values by id, so a rename in Jira drifts nothing. A value is gone when the catalog it was chosen from no longer has it — the same catalogs the save checks: the project's workflow for a rule; the union of the statuses or issue types of every project the handler's team tracks; the instance's priorities. As in the save, a tracked project Jira answers 404 for offers nothing to that union; its own rule is left as it is, since the project is `jira.discovery_failing`'s to report. A disabled handler, or one whose team tracks no project, names nothing gone.
- **Priority.** High when the drift stops something: a rule TF acts on (pickup, in progress, done) is left with none of its statuses or loses its write target, or a filter field names nothing still offered, so the handler matches no event. Low when every rule or field still has values that work. In-review drift is never high: nothing reads or writes it.
- **When.** The poller's Jira cycle runs the check once an hour per org (`jiraDriftInterval`), after the registry refresh, with the cycle's client. Each catalog is read once per pass, issue types and priorities only when a filter names them. A read that fails keeps what depends on it; a failure that is about Jira rather than the request (a rate limit, an outage, a refused credential) ends the pass's reads. An org with no tracked project resolves both kinds on every cycle, which needs no Jira call. A save decides what it can from reads it makes anyway, without waiting for the pass: a rules save decides status drift for each project whose workflow the gate read and resolves it for each project it dropped; a handler save that changes the filter, the kind or the enabled flag re-checks that handler before it answers, for at most five seconds; a delete resolves it. A tracked-set change that alters what a team's filters are judged against, and a rules save that reads no workflow, reach the notifications at the next pass. The discovery salvage's dropped statuses are rule drift, which the check reports from its own read of the workflow.
- **Where the value is identifiable.** The team's Jira source marks a project's missing statuses; the predicate editor shows a gone id as an unknown chip with a remove control.

## Phases

1. **Infrastructure and two consumers.** Tables, store and conformance, `notify` and `notifytest`, `kinds`, the API, the ping, the rail row, page and banner, and `github.app_unusable` + `linear.team_unreadable`. Together they exercise org and team audiences, a multi-audience key, `Set` and `Sync`, both action types, and the urgent banner.
2. **Remaining condition consumers and notices.** `jira.discovery_failing`, both mirrors, `memory.generation_failing`, `routing.automation_conflict` (TFAC-518), and the arrival pop. Each removes its `TODO(TFAC-878)` marker.
3. **`jira.status_drift` and `jira.predicate_drift`.** Detection in `internal/jiradrift` (see **Jira drift**).
4. **Event notifications.** Notify rules on catalog events (see **Event notifications**).

## Event notifications

People configure notifications on events that already exist for automation: "notify me when CI fails on my PR", "tell the team when a review requests changes". A **notify rule** is a third kind of `event_handlers`, beside rules and triggers, and it writes into the same `notifications` tables with `origin = 'event'`. Nothing is added to the events catalog for it, and TF's own health conditions stay the code-defined kinds above.

### The rule

| Field             | Notes                                                                                                         |
| ----------------- | ------------------------------------------------------------------------------------------------------------- |
| `kind`            | `notify`.                                                                                                     |
| `name`            | Required. Named in the notification's body, so a person can tell which rule told them.                        |
| `event_type`      | A GitHub, Jira or Linear event. Another source is refused (422 `INVALID_FIELD`); see **Matching**.            |
| `scope_predicate` | The same predicate schema, editor and checks as a task rule's.                                                |
| `audience`        | `creator` (a personal rule, the default) or `team`. Stored as `event_handlers.notify_audience`.               |
| `enabled`         | Defaults to true.                                                                                             |
| `team_id`         | The owning team, as for every handler.                                                                        |

A notify rule has no priority, sort order, blueprint or `applies_to_unowned`, and TF ships none: a shipped notify rule would badge every member of every team on install. Creation is `POST /api/event-handlers/notify`; reads, `PATCH` and `DELETE` are the existing handler routes, with a notify body schema (`name`, `enabled`, `scope_predicate`, `audience`). `scope_predicate: null` clears the filter; the other three always hold a value, so a `null` for one is a 400 naming it.

### Audience

- **`creator`** is one user-scope row for the rule's creator, written only while they are a member of the rule's team. A personal rule is a team handler, so the team's tracked set already limits what it can match, and a person who leaves the team stops being told.
- **`team`** is one team-scope row for the team's members.
- **One person, one row.** When one event matches a team rule and a personal rule, the personal row is not written if its creator is a member of the team whose rule matched: the team row already reaches them. Two rules for the same audience are one row (see **Coalescing**), so a person in two personal rules and a team rule on one event sees one notification.
- **Local mode** writes one row for the one user, whichever rules matched.

The membership read uses the inbox's own "is sent" predicate (`RecipientsSystem`), so an archived team's rules reach nobody. A failed read writes nothing for the event.

### Who may write

| Write                                                          | Gate                               |
| -------------------------------------------------------------- | ---------------------------------- |
| Create a personal rule                                         | Team write, as a task rule         |
| Edit or delete a personal rule                                 | Its creator, or a team admin       |
| Create, edit or delete a team rule, or move a rule to or from `team` | Team admin                   |

A rule whose audience is the whole team badges every member, so it is a team admin's decision. Team members can read each other's personal rules through the API, as they can every handler of their team; the editor lists only the caller's own and the team's.

### Matching

A notify rule matches as a rule does: an active entity, the event's current view against the predicate, the event type's entitlement. Its predicate is matched in the same pass as rules and triggers, after the close phase, whether or not any task results. It raises once the event has routed: an error later in routing replays the event, and raising before it would tell people once per attempt. An entity that closed under the task mint raises nothing, the answer the lifecycle gate gives a straggler.

**The tracked-set gate fails closed.** The router's team-scope gate answers "tracks" on a read error, malformed metadata or an unwired store, which is acceptable for tasks because it only narrows a set someone else computed. For a notification, a wrong "tracks" sends a person news about an entity their team does not track, which is a disclosure. The gate keeps both answers per team for one event, read from one lookup: notify rules take the strict one, where any answer the gate cannot prove is "does not track". GitHub, Jira and Linear have core's gates; an extension's source gate (`SourceHooks.TracksScope`) answers a bool that narrows and may fail open, so notify rules are refused on those event types until a source can answer strictly. A routing test holds the gated sources to the ones `domain.NotifyRuleSource` admits.

A notification never fails routing. The event's routing obligation is its tasks. A failed write, or a failed read of who a rule tells, is logged and counted (`tf_notify_write_failures_total{kind="event"}`), and the event is not replayed for either. There is no next evaluation to correct it, which is the cost of not replaying a whole event for a notification.

### Occurrences and coalescing

- **Kind** is the event type and **key** is `<entity_id>/<dedup_key>`: one open occurrence per (event type, entity, dedup key) per audience, the same triple a task dedups on. The rule is not part of the key, because two rules that match one event describe one thing to be told. The first matching rule for the audience is named in the copy.
- **Lifecycle.** An event type that a typed close relation closes (`closeRelations` in `internal/routing`, the non-terminating entries) opens a **condition**: CI check failed, review requested, changes requested, and Jira and Linear assigned and available. Every other type posts a **notice**, which expires `DefaultNoticeTTL` after its last event.
- **A recurrence** while a condition is open bumps it quietly: the condition is still true. A recurrence of a notice bumps it and **resurfaces** it (`escalated_at` moves, so it is unread and undismissed again, and the ping is sent): a second comment on a pull request is news the person asked for.
- **Priority** is `high` and every row is dismissible. `urgent` means work has stopped for the audience, which no catalog event says; a rule someone wrote in order to be told is what the badge is for.
- **A raise never resolves an audience.** Unlike `Set`, an event names only the rules its own metadata matched, so a second event on the same key that a team rule's predicate excludes does not resolve that team's row.

### Columns

`notifications.entity_id` (FK `entities`, `ON DELETE CASCADE`) is set iff `origin = 'event'` (CHECK), with a partial index on `(org_id, entity_id, kind)` over open rows for the close phase's read. The event id, the rule's handler id and the dedup key ride in `params`, which nothing selects on. There is no `handler_id` column: several rules can raise one occurrence, and deleting or disabling a rule leaves the rows it raised to resolve or expire on their own, as untracking leaves a team's tasks.

### Resolution

- **Typed close relations resolve conditions on the same entity.** The relation's `prepare` gate and `keep` filter run once for both kinds of target. `keep` reads a target's event type, dedup key and owning team; a notification has no owning team, so a reassignment resolves the old assignee's "assigned" notification, and the same event raises a new one when a rule matches the new assignment.
- **A terminating close** (merged, closed, completed, unreachable, the poll's close obligation) resolves every open event condition on the entity in the relation's close set, after the entity's transaction commits.
- **`ClearSource`** resolves a source's event conditions with its system ones.
- **An entity closed by a path that routes no event** (resolved from the stock deck, its repository retired) has no close relation to run. A brain-gated sweep (`notify.RunClosedEntitySweep`, every `ClosedEntitySweepInterval`) resolves every org's open event conditions about entities that are not active, and the stock deck's resolve runs the same pass for its org at once.
- Resolution is best effort, as a raise is: a failed write is logged and counted, a failed read is logged, and neither is part of the close phase's routing obligation. A relation that would only resolve notifications never fails routing either: a `prepare` error with no task to close leaves the notifications open. A condition a failed resolve leaves open is dismissible and resolves when its entity closes.

### Copy

One rendering for every event type (`kinds.Event`):

- **Title:** the catalog label and the entity's key, e.g. `CI Check Failed: acme/widgets#18`.
- **Body:** the entity's title, then `From your rule "…"` or `From the team rule "…"`.
- **Actions:** a link to the entity on its source (an external link, so the host check applies), and a link to the rule: "Edit your rule" on a personal row, which is how a person stops being told until per-person preferences exist, and "Open the team rule" on a team row. The entity's link is data rather than code, so an event row whose link is refused (no stored URL, a host the org no longer configures) is written with its other actions instead of failing the raise.

No type gets copy of its own in this phase. Where two open rows on one entity share a title (one per requested reviewer, one per label), the entity is where the reader acts on either, and a type that needs more than the label gets a renderer of its own when someone asks for it.

### Forward-only

A rule says nothing about an event that already happened. Creating, enabling or editing a notify rule applies to events from then on, and the editor says so under the event picker. Disabling or deleting one leaves what it already raised.

### The editor

Notify rules live on the triage page beside task rules: a **Notify rules** panel lists the caller's personal rules and the team's rules, each with an enabled switch, and opens an editor with the event picker and predicate editor task rules use, plus the audience (offered as "Just me" or "Everyone on the team"; the second only to a team admin). `Routes.NotifyRule(id)` is `/triage?notify_rule=<id>`, which opens that rule.

### Not in scope

Org-scoped handlers; out-of-app delivery, which is a later channel on the same handler; a per-type renderer; muting a rule per person.

## Later

### Also deferred

- Out-of-app delivery (email, Slack, webhooks — adjacent to TFAC-79).
- Per-person preferences: muting a kind, choosing delivery channels.
- A shared "acknowledged by …" mark that annotates a row without hiding it.
- A cross-org count on the org switcher.
- Snooze.

## Open questions

Both are UI design questions, tracked in TFAC-1239:

1. **The urgent banner for a non-dismissible condition.** Does it stay at full size after the person has read it, or collapse?
2. **Rail row position** within the work section.
