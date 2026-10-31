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

Event-driven notifications remain the plan for user-configured rules on events that already exist for automation ("notify me when CI fails on my PR"). They write into the same tables with `origin = 'event'`; see **Later: event notifications**.

## Vocabulary

| Term           | Meaning                                                                                                                              |
| -------------- | ------------------------------------------------------------------------------------------------------------------------------------ |
| **Kind**       | A registered notification type, e.g. `linear.team_unreadable`. Its name is stored on rows and never renamed.                         |
| **Key**        | The occurrence's identity within its kind, built from ids (never names) — e.g. the Linear team id.                                   |
| **Occurrence** | One row: one kind + key, for one audience, from opening to resolution. A condition that resolves and comes back is a new occurrence. |
| **Audience**   | Who a row is for: `org` / `team` / `user` scope, with role `members` or `admins` for org and team.                                   |
| **Lifecycle**  | `condition` (the system resolves it) or `notice` (it expires).                                                                       |
| **Origin**     | `system` (a code-defined kind, V1) or `event` (a notify handler on an event, later).                                                 |
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

// internal/delegate/jira_mirror.go — true where the canonical is not a reachable
// transition, false after a successful move; a transient error does neither
kinds.JiraMirrorFailing.Set(ctx, orgID, kinds.JiraMirrorFor(rule, issueKey), true)

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

**Audience constructors** (V1): `OrgMembers()`, `OrgAdmins()`, `TeamMembers(fn)`, `TeamAdmins(fn)`, `User(fn)`, `TeamsTrackingLinearTeam(fn, role)`, `TeamsArmingJiraProject(fn, role)`, combined with `Audiences(...)`. These resolvers read the tracked sets (for Jira, the rules that arm a project) through stores handed to `notify.Install`, skip archived teams, and are re-run on every write. A resolution failure skips the write (logged, counted) and never falls back to a wider audience; `Set` and `Sync` retry it on the next evaluation.

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

The recorder is installed for the test and its assertions filter by the test's own org id, so parallel tests do not see each other. A conformance test in `kinds` renders every kind's `Example` for every audience it can produce and checks: a non-empty title, at most two actions, every notice has an action, every `Link` route resolves, and no two kinds share a name.

## Data model

Postgres changes go into the baseline; SQLite gets a forward migration. Both behind one `db.NotificationStore` with `dbtest` conformance cases, following the store standards (writes return the stored row via `RETURNING`; system writes are `...System` methods on the admin pool; reads run under the request's claims).

### `notifications`

| Column                          | Type             | Notes                                                                               |
| ------------------------------- | ---------------- | ----------------------------------------------------------------------------------- |
| `id`                            | uuid             |                                                                                     |
| `org_id`                        | uuid             |                                                                                     |
| `origin`                        | text             | `system` \| `event`. V1 writes `system`.                                            |
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

Indexes: a partial unique index on the open occurrence — `(org_id, origin, kind, key, audience_scope, coalesce(audience_role,''), coalesce(team_id, <nil uuid>), coalesce(user_id, <nil uuid>)) WHERE resolved_at IS NULL` — and a list index on `(org_id, resolved_at, priority, escalated_at, id)`.

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

| Kind                          | Source | Audience                                    | Priority                              | Dismissible | Verb and site                                                                                                             | Actions                                                                       |
| ----------------------------- | ------ | ------------------------------------------- | ------------------------------------- | ----------- | ------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------- |
| `github.app_unusable`         | github | org admins                                  | urgent                                | no          | `Sync` on every reconcile (`internal/poller/manager.go`, beside `logAppUsabilityChange`); an active App only              | Link: GitHub access settings                                                  |
| `linear.team_unreadable`      | linear | org admins + admins of TF teams tracking it | high                                  | yes         | `Sync` on a complete visibility pass over every armed team (`checkLinearTeamVisibility`; never on outcome `failed`)       | Link: Linear settings; invoke "Stop tracking" on team rows                    |
| `jira.discovery_failing`      | jira   | admins of teams whose rules arm the project | high                                  | yes         | `Sync` from `discoverJira` after 3 rejected cycles in a row; unanswered projects kept                                     | Link: the team's Jira status rules                                            |
| `jira.mirror_failing`         | jira   | team admins                                 | high                                  | yes         | `Set` in `runJiraMirror` and the claim guard (`applyJiraClaim`)                                                           | Link: the team's Jira status rules                                            |
| `linear.mirror_failing`       | linear | team admins                                 | high                                  | yes         | `Set` in `runLinearMirror` and `applyLinearClaim`                                                                         | Link: the team's Linear rules                                                 |
| `memory.generation_failing`   | —      | org admins                                  | urgent for `no_model`, high otherwise | no          | `Sync` from the memory provisioner over the `error_kind`s currently failing across the org's attempts, after its debounce | Link: background-jobs model setting; invoke "Retry now" (rings `memory_owed`) |
| `jira.status_drift`           | jira   | team admins                                 | high                                  | yes         | `Sync` from a detection pass that does not exist yet                                                                      | Link: the rule or handler editor                                              |
| `routing.automation_conflict` | —      | admins of the losing teams                  | low                                   | (notice)    | `Post` in the router where the conflict is decided                                                                        | Link: the losing team's trigger                                               |

Debounce is each producer's responsibility, using evidence it already has (the attempts ledger, consecutive cycles). `notify` coalesces; it does not wait. Each consumer's ticket sets its producer's thresholds.

## Phases

1. **Infrastructure and two consumers.** Tables, store and conformance, `notify` and `notifytest`, `kinds`, the API, the ping, the rail row, page and banner, and `github.app_unusable` + `linear.team_unreadable`. Together they exercise org and team audiences, a multi-audience key, `Set` and `Sync`, both action types, and the urgent banner.
2. **Remaining condition consumers and notices.** `jira.discovery_failing`, both mirrors, `memory.generation_failing`, `routing.automation_conflict` (TFAC-518), and the arrival pop. Each removes its `TODO(TFAC-878)` marker.
3. **`jira.status_drift`.** Needs its own detection work first.

## Later

### Event notifications

User-configured notifications on events that already exist for automation: a `notify` kind of `event_handlers`, written into the same tables with `origin = 'event'`. A personal rule is a team handler whose audience is its creator, so the team's tracked-set gate already handles what it may match; matching must fail closed, unlike the task-visibility gate. A row opened by an event is resolved by the existing close relations ("CI passed" resolves "CI failed"); a handler on an event with no close relation produces a notice. The event-specific columns (`handler_id`, `event_id`, `entity_id`) are designed with that work. Nothing is added to the events catalog for it.

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
