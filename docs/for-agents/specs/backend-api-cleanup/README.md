# Backend surface cleanup — audit and target contract

The HTTP surface is the primary way both the SPA and AI agents drive
Triage Factory. An agent navigating an API discovers it the same way a
new engineer does: by reading route names, trying calls, and learning
from errors. Every place the surface is irregular — a route that does
two things, a field that is silently dropped, an error that comes back
as plain text, a list that cannot be paged — is a place an agent (or a
person) draws a wrong conclusion and builds on it.

This spec does two things: states the **target contract** every route
converges on (§1), and records a **full audit** of where the current
surface falls short (§3), with file:line evidence, so remediation can be
ticketed without re-deriving the findings.

Status: **audit complete, contract settled — remediation unscheduled.**
Audited 2026-08-15 against main at `ef2ceb3e` (~180 routes across
`internal/server/` plus the ee Slack/SSO surfaces). Line references
drift as files change; treat them as pointers, not anchors.

---

## 1. The target contract

Five rules. Every new route follows them from day one; existing routes
converge per the remediation order in §4.

### R1 — One intent per route

A route does one nameable thing. Its body has a fixed schema: no
discriminator field that changes which other fields are meaningful, no
field whose emptiness selects a second behavior (blank-means-delete,
blank-means-keep). If two operations need different required fields,
different validation, or different response shapes, they are two routes.
Near-neighbor routes must discriminate clearly by name.

Verb routes (`POST /…/{id}/approve`) are reserved for operations with
real side effects a field write cannot express — external API calls,
multi-row atomic transitions, process control. A verb route whose entire
effect is setting a column is a PATCH in disguise (see R5).

### R2 — No silent field handling

A bad call fails. Concretely:

- Unknown body fields are rejected (strict decoding), not ignored.
- A field that is invalid is rejected with the field named — never
  clamped, defaulted, case-folded, or dropped. (Which status a given
  rejection carries is R4's concern; R2 only demands that it fail.)
- Query params parse strictly: a malformed value is rejected, not
  resolved to the default (a corrupt filter must never *widen* a
  result set).
- Enums validate against the full vocabulary; a typo is an error, not a
  fall-through to the default arm.
- A response never claims success for work that didn't happen
  (`{"status":"saved"}` from a handler that wrote nothing).

### R3 — Read consistency

Every resource exposes the same read surface:

- `GET /…/<resource>/{id}` — canonical single read.
- `GET /…/<resource>/by-name/{name}` — where the resource has a unique,
  human-meaningful name used for identification.
- `POST /…/<resource>/list` — the list read. Body carries filters plus
  `page_size` and `page_token`; the response is
  `{items, next_page_token, total_count}`. No bare-array lists, no
  fetch-the-world defaults, no hardcoded store LIMITs that truncate
  silently. This requires store-layer support (keyset or offset tokens
  plus a count) — that cost is accepted.

Reads are resource-pure (no caller-relative annotations mixed into the
resource body without naming them as such), addressed one way (not
session-scope on one route, path on a second, query param on a third),
and side-effect-free.

### R4 — Error shape

One envelope everywhere:

```json
{
  "errors": [
    {
      "reason": "STABLE_SCREAMING_SNAKE_CODE",
      "message": "human prose",
      "field": "only when a specific payload field is at fault"
    }
  ]
}
```

- Always a **list**, never a single error — validation reports *every*
  applicable failure, not the first one hit.
- `reason` is a stable machine code; `message` is prose; `field` appears
  only for payload-field faults. The HTTP status is blanket for the
  response.
- No plain-text error bodies anywhere on `/api/*` (including 401s from
  middleware). No 200-with-error bodies. No raw driver/upstream error
  strings in multi mode.
- Status codes are principled: 400 malformed request, 404 not visible
  (disclosure policy per CLAUDE.md), 403 visible but forbidden, 409
  state conflict, 422 semantically invalid, 502 upstream failure — and
  the same fault class maps to the same status on every route.

### R5 — PATCH for updates

Field-by-field updates live at `PATCH /…/<resource>/{id}`, not
full-replacement PUT, not POST-as-update, not verb routes. Absent means
keep; explicit `null` means clear (decode via `json.RawMessage` or
equivalent so the two are distinguishable — the `PATCH
/api/repos/{owner}/{repo}` handler is the reference implementation).
One clearing convention everywhere: no empty-string sentinels, no
zero-means-null, no fields that cannot be cleared at all.

### R6 — Addressing: what a scope segment means

Three ways of naming a scope coexist on this surface, and they are not
interchangeable. Each says something different about *whose* data is
being read, and the difference is what tells a reader — and an auditor —
which authorization applies. Writing them down here is the point: the
inconsistency the audit found was never that three forms exist, it was
that nothing said which was which, so new routes picked one at random.

**Session-implied scope — viewer-relative reads.** `/api/me`,
`/api/usage/me`, `/api/dashboard/*`. The subject is *the caller*, and it
comes from the session, never from the path. These are the only routes
where the absence of a scope segment is meaningful: there is no id to
put there, because the answer is different for every caller and no
caller may address another's. A `?user=` on one of these would be an
impersonation surface.

**Path-scoped — admin-scoped resources.** `/api/orgs/{org_id}/…`,
`/api/teams/{team_id}/…`, `/api/usage/teams/{team_id}/…`. The subject is
a *named* org or team, so the caller is asserting a scope and the
handler authorizes them against it. Anything an admin reads about
somebody else's scope belongs here, because the id in the path is what
the authorization check has to be about.

**Query-scoped — operator diagnostics.** The fleet console's `?org=`.
The subject is a deployment-wide view being narrowed for inspection, not
a tenant boundary being asserted; the authorization is "operator", and
it does not vary with the value. This form is available to the fleet
surface and nothing else — a tenant-facing route that narrows by a
query param is a route whose scope check can be skipped by omitting the
param.

**A team segment takes one grammar, everywhere.** `{team_id}` accepts a
uuid, and in local mode additionally the literal `default` (the sole
team, which has no id a user could know). Multi mode requires the uuid.
One resolver implements this — `authz.Checker.ResolveTeamID`, wrapped by
`TeamIDFromPath` — and every handler with a `{team_id}` segment calls
it, so a segment that resolves to nothing is a 404 rather than a 500
from a uuid cast three layers down.

**Moving a route between these forms is a route change, not a cleanup.**
The settings family is the one migration in flight (8/8 moves
`/api/settings/team/{team_id}` and `/api/settings/org` under their
resources); nothing else moves on the strength of this section. The
section exists so the next route is placed correctly, not so existing
ones get churned.

---

## 2. Kernel work (cross-cutting)

Most R2/R3/R4 findings trace to `internal/server/httpx` and the store
layer rather than to individual handlers. Fixing the kernel first makes
the per-route sweep mechanical:

1. **Strict decoder.** `httpx.DecodeJSON` gains
   `DisallowUnknownFields` plus a validation-accumulator so handlers
   can report all field errors at once. The only strict decode today is
   the predicate validator (`internal/domain/events/validate.go`).
2. **Error envelope.** `httpx` grows `WriteErrors(w, status,
   ...ErrorItem)` with the R4 shape; `BadRequest`/`NotFound`/
   `InternalError`/`WriteUnauth` become thin wrappers emitting it.
   `WriteUnauth` today is plain-text `http.Error` — every protected
   route emits it via middleware.
3. **List contract.** A shared `httpx.Page` request/response pair and a
   store-layer pagination seam (token encode/decode + `total_count`
   queries) in both dialects, added to the `dbtest` conformance suite.
4. **Route helpers.** A `by-name` and `POST /list` registration
   convention so new resources get the R3 surface by default.

The frontend follows the kernel: one API-client error parser for the R4
envelope, one pagination hook for the R3 list shape.

---

## 3. Audit findings

Organized by rule. Every finding carries evidence from the audit pass;
paths are relative to `internal/server/` unless noted.

### 3.1 R1 violations — routes with more than one intent

**Discriminator-polymorphic bodies (worst offenders):**

- `POST /api/tasks/{id}/swipe` (`swipe.go:17-24`) — six-value `action`
  discriminator (`claim|dismiss|snooze|delegate|complete|reassign`)
  selects the meaningful fields (`blueprint_id` delegate-only,
  `target_user_id` reassign-only), the authz arm, the mutation, and the
  response shape (`conversation_id`/`delegate_error` appear only on
  delegate).
- `POST /api/event-handlers` (`event_handlers_handler.go:124-149`) —
  one route creates two resources (`kind: rule|trigger`) with different
  required fields, different defaults, and an opposite `enabled`
  default (rule=true, trigger=false). PATCH has the same kind-switch.
- `POST /api/jira/stock` (`stock.go:318-321`) — per-row `action`
  discriminator (`queue|claim|done`); the arms are entirely different
  operations (local task mint / two external Jira writes + claim /
  external transition + entity close with no task).
- `POST /api/bedrock/connect` (`bedrock_connect.go:121-135`) —
  four-flavor union on `auth_method` (`role|bearer|access_keys|none`),
  nine flavor-conditional fields; `"none"` makes the connect route also
  the disconnect route.
- `PATCH /api/artifacts/{id}` and its GET/approve/dismiss siblings
  (`artifacts_handler.go:184`, `reviews_artifact_handler.go:125`) — two
  disjoint body/response schemas selected by a **server-side**
  discriminator (`art.Kind`). A review-shaped body on a PR artifact
  decodes to all-nil pointers and still performs a real GitHub
  `UpdatePR` write + audit row + 200; a PR-shaped body on a review
  artifact is silently ignored with 200.
- `PUT /api/orgs/{org_id}/jira/access/credential` (`settings.go:361`) —
  Cloud-vs-DC flavor inferred from which fields are non-empty; no
  explicit discriminator.
- `POST /api/orgs/{org_id}/jira/identity/pat`
  (`jira_connect.go:280`) — two field sets dispatched by *server-side
  state* (the org's deployment marker); the dead half of the body is
  silently ignored.
- `POST /api/blueprints` (`blueprints_handler.go:140-189`) — presence
  of `first_prompt` flips both the validation set (`name` optional vs
  required) and what gets created.

**Empty-value-as-second-intent:**

- `POST /api/anthropic/connect` (`settings.go:480-531`) — blank
  `api_key` = destructive clear of the Anthropic key **and** the entire
  Bedrock credential set; no DELETE route exists for either LLM
  credential. Bedrock gives blank secrets the *opposite* meaning
  (keep-current).

**Multi-concern bodies:**

- `POST /api/settings/team/{team_id}`
  (`settings_handlers.go:181-210`) — ≥7 unrelated concerns in one body
  (model, auto-delegate, thresholds, branch template, review posture,
  push policy, permission grace) **plus** a full replace-set of the
  `jira_project_status_rules` child collection — a sub-resource
  smuggled into a settings key while siblings (repos, github-groups)
  got their own PUT routes.
- `POST /api/settings/org` (`settings_handlers.go:604-616`) — source
  hosts + poller cadence + clone transport + spend governance in one
  body.
- `POST /api/integrations/setup` (`credentials.go:36-217`) — binds
  GitHub PAT + optionally Jira PAT + clone protocol + base URLs + agent
  login + poller restart in one call; overlaps three dedicated routes
  with *less* capability (Cloud Jira cannot bind here; the
  `JiraAuthMethod` marker is never written).
- `POST /api/projects/import` (`internal/projectbundle/import.go:236,
  473-516`) — creates a project *and* silently mutates the team's
  tracked-repo set (`ReplaceForTeam`) and seeds repo profiles; the
  response never mentions the tracked-set change.
- `DELETE /api/integrations` (`credentials.go:524`) — bulk-deletes two
  distinct credentials with no discriminator, alongside per-credential
  DELETEs.

**Aliased / duplicate / confusable near-neighbors:**

- `GET /api/queue` vs `GET /api/tasks` (`tasks.go:113/152`) — with no
  `status`, `/api/tasks` calls the same `Tasks.Queued`; but
  `?status=queued` takes the generic `WHERE status=?` arm
  (`internal/db/sqlite/tasks.go:192-199`) **without** the
  claim-NULL/snooze filters `/api/queue` applies — same nominal filter,
  different result sets. Each alias has a param the other lacks.
- `POST /api/tasks/{id}/undo` vs `/requeue` (`tasks.go:296-305`) —
  identical observable outcome; the split encodes only an
  audit-attribution difference.
- `GET /api/usage/*/activity` (`usage_activity_handler.go:111-116`) —
  `?view=` multiplexes two different resources (artifacts head vs
  action log) with different row shapes and filter vocabularies, while
  the run-scoped equivalents are properly split as `/artifacts` and
  `/actions`.
- `PUT /api/orgs/{org_id}/github/access/pat` vs
  `POST /api/orgs/{org_id}/github/identity/pat` — one path word apart,
  both take a `pat` field, opposite persistence semantics (store vs
  validate-and-discard), different 422 `field` values (`"pat"` vs
  `"github_pat"`).
- `POST /api/skills/import` vs `/upload` (`skills_handler.go:28/67`) —
  names don't convey the actual split (server filesystem scan vs
  request body; import 501s in multi mode).
- `POST /api/agent/…/message` (singular) vs `GET …/messages` (plural)
  vs curator's `POST …/curator/messages` — two chat surfaces, two
  naming conventions, different response semantics (sync
  `{"status":"sent"}` vs async 202 `{request_id}`).
- `POST /api/projects/import` (`server.go:1027`) — a literal carved
  from the `{id}` segment namespace; `GET /api/projects/import` falls
  into the single-get and answers "project not found".
- `POST /api/orgs` vs `POST /api/setup/start` — the same
  provision-my-tenant intent at two addresses split by runmode. Org
  create and invite-accept also both perform the active-org session
  switch that `POST /api/me/active-org` owns — three writers for one
  session field.
- `scope_predicate_json` is typed three ways across three routes:
  create takes `string` (`event_handlers_handler.go:127`), PATCH takes
  `json.RawMessage` accepting string *or* bare object, promote takes
  `*string` (cannot express the clear PATCH can).
- `DELETE /api/projects/{id}/curator/messages/in-flight`
  (`curator.go:306-383`) — one route, two mutations selected by hidden
  state (release a running claim vs delete a queued message row).
- The preflight family inverts verb/side-effect pairing:
  `GET …/github/app/cutover-preflight` **mutates** the installation
  mirror while `POST …/github/access/pat-preflight` stores nothing.

### 3.2 R2 violations — silent field handling

**Global:** `httpx.DecodeJSON` never calls `DisallowUnknownFields` —
every route silently ignores unknown/misspelled body fields, and every
kind/flavor-discriminated route silently drops the *other* kind's
fields.

**Routes that discard or rewrite what was sent, then report success:**

- `POST /api/settings/user` (`settings_handlers.go:76-98`,
  `internal/domain/settings.go:325`) — the decode target is an **empty
  struct**; every field in `user_settings` is discarded, the store
  touches `updated_at`, the response says `{"status":"saved"}`.
- `POST /api/setup/start` (`credentials.go:424`) — never decodes the
  body; any posted configuration → 200 `{provisioned:true}`.
- `POST /api/integrations/setup` (`credentials.go:168-170`) — invalid
  `clone_protocol` values (e.g. `"SSH"`) silently dropped at persist
  with 200 — the same bug the org-settings sibling fixed by 400ing.
  Also (`credentials.go:107,124-130,165-167`): `jira_url` without
  `jira_pat` skips validation but still writes the URL into the vault
  and `org_settings.JiraBaseURL` — an unvalidated partial write.
- `POST /api/event-handlers/{id}/toggle`
  (`event_handlers_handler.go:562-564`) — `Enabled` is a non-pointer
  bool: an empty body `{}` silently **disables** the handler.
- `PUT /api/event-handlers/reorder`
  (`event_handlers_handler.go:852-878`) — trigger ids, unknown ids,
  deleted ids are zero-row no-ops; duplicates last-write-win;
  unconditional `{"status":"reordered"}`.
- `POST /api/tasks/{id}/swipe` snooze arm (`swipe.go:544-563`,
  `internal/db/sqlite/swipes.go:60-71`) — the swipe route has no
  `until` field, so a swipe-snooze writes `snooze_until` NULL: an
  **indefinite snooze**, while `/snooze` requires and validates
  `until`.
- `PUT /api/artifacts/{id}/comments/{commentId}`
  (`reviews_artifact_handler.go:665-667`) — a severity badge in the
  submitted body is silently stripped and the old severity re-baked.
- `POST /api/projects` (`projects.go:112-114,140-158,986`) — local
  mode silently rewrites `visibility` to `"team"` after validating it;
  `team_id` is ignored outside the team branch; `jira_project_key` is
  silently case-folded.
- `POST /api/projects/import` — bypasses sibling-create validation
  wholesale: verbatim Jira key, accepts the Linear key create rejects,
  pins not checked against the tracked set; curator claims with empty
  ids silently skipped, unknown `claim_id`s blanked
  (`internal/projectbundle/import.go:226-233,414-444`).
- `POST /api/projects/{id}/backfill` (`backfill.go:192-209`) —
  empty/duplicate entity ids vanish from the accounting
  (`applied + failed ≠ submitted`); an empty array → 200
  `{"applied":0}` rather than 400.
- `POST /api/bedrock/connect` (`bedrock_connect.go:221-306`) —
  cross-flavor secrets count toward "provided" then get discarded;
  sending `role_arn` with a static method silently *deletes* the
  stored role keys.
- ee `POST /api/slack/workspaces` (`ee/slack/workspaces.go:150-160`) —
  three secret fields, two blank-means-keep conventions in one body; a
  kept app token is never re-validated.

**Silent clamps and defaults:**

- `GET /api/events/failed?limit=`
  (`internal/db/event_queue_store.go:48-57`) — ≤0 → 100, >500 →
  clamped, never rejected.
- `GET /api/usage/org/access-log` (`usage_access_log.go:147-166`) —
  malformed `limit`/`offset` silently default, while
  `parseArtifactListOpts` in the same handler family 400s the same
  input (`usage_activity_handler.go:308-324`). Two paging parsers,
  opposite postures.
- Activity `limit` over-max silently clamps to 200; the ops rollup
  passes `limit=0` which the store rewrites to `LIMIT 5000` —
  percentiles computed over a silently truncated set
  (`usage_ops_handler.go:58`,
  `internal/db/postgres/conversation_queue.go:984-986`).
- `POST /api/settings/team/{id}` — grace seconds silently clamped;
  `ai_reprioritize_threshold`/interval unvalidated; `ai_model`
  any-string while sibling enums 400; present-but-empty
  `branch_template`/`review_posture`/`push_policy` silently reset to
  defaults — a third wire semantics (nil=keep, ""=reset) alongside
  pointer merges (`settings_handlers.go:265-336`).
- `POST /api/invites` (`invites_handler.go:113-165,484-489`) — empty
  `role` defaults to member, unechoed; `email` completely unvalidated →
  a durable unredeemable ghost invite; accept trims the token for the
  emptiness check but hashes it untrimmed.
- `PUT /api/usage/teams/{id}/cap` (`usage_handler.go:415-421`) —
  explicit `0` silently means *clear the cap* (stored NULL), the
  opposite of "cap at $0".
- `POST /api/teams` (`slug.go:17-29`) — caller `slug` silently
  re-slugified + truncated to 48; `name` uncapped on create but capped
  on PATCH rename.
- `POST /api/event-handlers/{id}/promote`
  (`event_handlers_handler.go:620-627`) — missing the range checks
  create/PATCH enforce: persists `breaker_threshold:-1`,
  `min_autonomy_suitability:7`. Opposite defaulting rules vs create
  for the same trigger fields.
- Snooze `until` (`tasks.go:943-958`) — two grammars in one field
  (magic strings vs RFC3339); past timestamps accepted
  (instantly-expired snooze); `hesitation_ms` never validated.

**Swallowed query-param parse errors:**

- `teamscope.FilterParam`/`SingleParam`
  (`teamscope/teamscope.go:246-283`) — a malformed `?team_id=` is
  silently dropped, **widening** the result set (or silently
  retargeting the Jira deck to the default team). Affects /api/queue,
  /api/tasks, /api/factory/snapshot, /api/jira/stock,
  /api/event-handlers, /api/prompts, /api/blueprints,
  /api/blueprint-steps.
- `?since_id=abc` → 0 → full transcript (`agent.go:467-470`);
  `?task_ids=` beyond 500 silently truncated; unknown `include=`
  values dropped (`agent.go:934-951`).
- Boolean params are exact-string compares (`include_snoozed`,
  `include_membership`): `1`/`True`/`yes` silently mean false.
- `?view=` — any non-`actions` value (including typos) silently
  selects the objects lens; cross-lens filters silently ignored;
  filter vocabularies (`provider`/`kind`/`state`/`action`, access-log
  `category`) unvalidated → 200 empty
  (`usage_activity_handler.go:52-57,282-287,515-520`).
- `GET /api/jira/statuses?project=` bypasses the key validation the
  write path applies → garbage keys become 502s upstream instead of
  400s (`settings.go:602,647`).
- Dashboard `?repo=` (`dashboard.go:166-169`) — `"owner/"` and
  `"/repo"` pass the `len==2` check with an empty half; `{number}`
  accepts negatives.
- Fleet placement (`fleet_placement.go:111-114`) — `?repo=` and
  `?project=` interchangeable regardless of `kind`; `repo` silently
  wins when both are present.

**Swallowed store/vault errors that reshape responses:**

- `integrations.Load` errors discarded (`creds, _ =`) at
  `stock.go:93,373`, `repos_handler.go:88`, `settings.go:608,717`,
  `credentials.go:534`, `org_credentials.go:225,287`,
  `settings_handlers.go:510` — a backend failure becomes 400 "not
  configured", telling the user to re-enter credentials they have.
- The same swallowed load decides App staging at
  `github_app_register.go:564` and `github_app_import.go:471` — a
  transient vault failure reads as "no PAT" and registers the App
  `active=true` beside a live PAT, silently breaking the App-XOR-PAT
  invariant. **Outright bug.**
- `GET /api/agent/conversations/{id}` — best-effort enrichment
  silently omits `has_unresolved_artifacts`/counts or serves
  `blueprint_step_count:0` on lookup failure (`agent.go:59-78`); the
  PR-kind artifact GET drops the details-parse error entirely while
  the review kind 500s the identical condition
  (`artifacts_handler.go:153` vs `reviews_artifact_handler.go:76-80`).
- `GET /api/team/members` local arm discards the `WithTx` error → 200
  roster with blank identity (`config_handler.go:93-101`).
- `POST /api/tasks/{id}/undo` never checks a swipe exists —
  force-resets any task with an 'undo' audit row
  (`internal/db/sqlite/swipes.go:224-256`).
- Dismiss/complete swipe arms have no terminal guard — dismissing a
  `done` task silently rewrites `closed_at`/`close_reason`
  (`swipe.go:544-563`).
- `PATCH /api/repos/{owner}/{repo}` (`repos_handler.go:487-509`) — an
  empty `{}` performs no write and answers `{"status":"updated"}`;
  `base_branch` is never checked against real branches.

### 3.3 R3 violations — read inconsistency

**Global:** no pagination vocabulary exists anywhere — no
`page_size`/`page_token`/`next_page_token`/`total_count` on any route;
no `POST /<resource>/list`; no `by-name/` read (closest is marketplace
`by-source/{source_id}`). Nearly every list fetches the world:

- Unbounded store SQL (no LIMIT): tasks Queued/ByStatus, dashboard PRs
  (scans **all** github entities, filters by author in Go —
  `internal/db/sqlite/dashboard.go:99-118`), conversations
  ListForTask, transcripts, curator history, projects, project
  entities, backfill candidates (two full org scans filtered in Go),
  invites, org members, team-caps (plus an N+1 settings read per
  team), the github repos proxy, event-handlers, prompts, blueprints,
  steps, marketplace.
- Hardcoded caps with no truncation signal: factory snapshot 500
  entities (`factory_handler.go:19-25`), run actions 200
  (`agent.go:295` — constant duplicated in the frontend), branches 30,
  ops rollup 5000.
- Ad-hoc paging exists in exactly two shapes that disagree: activity
  `limit`/`offset` bare array vs access-log
  `{items, limit, offset, has_more}` — and failed-events returns
  `{"events","count"}` where `count` is the *page length*, a
  total_count-shaped field with page-size semantics
  (`failed_events_handler.go:90-93`).

**Missing canonical reads:**

- No `GET /api/teams/{team_id}` — consequence: a team's `description`
  is writable via PATCH but **unreadable** (only the PATCH response
  carries it) (`teams_handler.go:117-122`).
- No `GET /api/orgs/{org_id}`; no `GET /api/invites/{id}` (the accept
  URL exists only in the create response); no
  `GET /api/event-handlers/{id}` (five mutation routes all preload the
  row internally); no `GET /api/blueprints/{id}`; no blueprint-runs
  **list** (only per-id); no `GET /api/repos/{owner}/{repo}` item read
  (PATCH-only); no failed-event single read; no permission-prompt
  single read.
- `GET /api/prompts/{id}/stats` on a nonexistent id returns 200
  all-zero stats while the sibling GET 404s
  (`prompts_handler.go:398-415`).

**Envelope and shape scatter:** bare arrays (projects, repos, invites,
branches, github repos, jira statuses, prompts…), `{"entities":[…]}`,
`{"candidates":[…]}`, `{"teams":[…]}`, `{"members":[…]}`,
`{"items":[…]}`, `{"events":…,"count":…}`, and map-keyed
(`/api/event-schemas`). `GET /api/teams` and `/api/teams/archived` use
different envelopes *and* row shapes. Status-discriminated unions on
200: jira stock (`{"status":"polling"}` vs full deck), dashboard stats
(`{}` vs the stats shape), integrations status (three key sets by
state — `credentials.go:255-401`).

**Addressing inconsistencies:** org scope comes from the session
(usage), the path (`/api/orgs/{org_id}/…`), or a query param (fleet
`?org=`); `{team_id}` accepts the literal `default` on the settings
family but 404s it on teams/roster/archive; dashboard PRs split
identity across path (`{number}`) and query (`?repo=`); repos use a
two-segment path id while rows carry an `id` no route accepts; curator
turns are keyed by a decimal-string message id unlike everything else.

**Two read surfaces disagreeing about one object:**
conversation-scoped artifacts serve every kind in a generic shape
while `GET /api/artifacts/{id}` 404s existing artifacts of unsupported
kinds (`artifacts_handler.go:144-146` — nonexistence conflated with
unsupported-representation); `GET …/github/app` returns 200 `app:null`
where install-url and cutover-preflight 404 the same absent-App state;
`connect_available` lives on the *jira app* status but the *github
identity* status; the conversation projection mixes PascalCase legacy keys
with snake_case additions in one object (`agent.go:388-430`), and the
factory snapshot deliberately clones that accident into a second route
(`factory_handler.go:62-82`).

**Reads with side effects:** `GET …/github/app/cutover-preflight`
mutates the installation mirror (`github_access.go:459`); knowledge
file reads double-decode the path (`projects.go:1417` — the mux
already decoded, then `PathUnescape` runs again) so `%`-named files
the listing returns are unreachable (**outright bug**);
`POST /api/bedrock/role-setup` is a read-shaped endpoint that mints
state on first call. OAuth callbacks write rows on GET — protocol-
constrained, frozen paths, not actionable.

### 3.4 R4 violations — error shape

**Global:** the kernel emits `{"error": "<one prose string>"}` — no
`reason` code, no list, no `field`. Meanwhile `RequireOrg` and the WS
handshake already treat `error` as a stable snake_case *code* with a
separate `message` (`httpx.go:246`, `ws_handler.go:76-81`), so two
incompatible conventions coexist on every org-gated route. Nothing
accumulates multiple errors — every multi-field body reports only the
first failure.

**Plain-text errors on JSON routes:**

- `WriteUnauth` (`httpx/httpx.go:126`) — plain-text 401 via the
  session middleware on **every protected route**, plus direct
  emissions across orgs/invites/usage/entitlements/avatars/fleet/me.
- ~25 `http.Error` sites in `auth_handlers.go` (including `/api/me`
  and `/api/me/active-org`, routes the SPA consumes as JSON);
  `http.NotFound` (Go's text body) throughout teams/roster/archive and
  org-members/invites/usage — while sibling arms of the *same
  handlers* use JSON `notFound()`. A malformed org uuid gets JSON on
  fleet routes but plain text on usage routes.
- `github_app_register.go:475,479` — plain-text 401s in a callback
  family that otherwise answers JSON or redirects.

**200-with-error bodies:**

- `POST /api/factory/delegate` — deliberate 200 + `{"delegate_error"}`
  collapsing 400-class and 500-class failures
  (`factory_delegate.go:356-374`); the swipe delegate arm same, and
  can return 200 with *neither* `conversation_id` nor `delegate_error`
  (`swipe.go:674-676`).
- `POST /api/jira/stock` — always 200 even when every action failed;
  `POST /api/skills/import` — 200 with an error array even on total
  failure; `POST …/knowledge` — self-described "207-ish semantics in a
  200" (`projects.go:1716`); backfill — 200 for an entirely-failed
  batch, leaking raw store errors per row (`backfill.go:222,254`);
  `GET /api/integrations/status` — 200 + `"error"` key on vault
  failure; invite preview — 200 `{"status":"not_found"}`;
  preflight-ssh — 200 `{ok:false}` locally but **404** +
  `{ok:false,"error"}` in multi, while the same no-SSH condition is a
  400 on settings/org — one condition, three statuses, three shapes.
- Soft variant: approve/dismiss return 200 asserting a persisted state
  the detached cleanup may have failed to write
  (`artifacts_handler.go:517-524`); restore returns the pre-restore
  row (`team_archive_handler.go:255-257`); `PATCH /api/projects/{id}`
  500s *after* a successful commit on read-back failure
  (`projects.go:734-742`).

**Wrong or inconsistent statuses:**

- Malformed uuid → 500 (SQLSTATE 22P02) on Postgres for
  `GET /api/tasks/{id}`, snooze, undo, requeue, and all swipe arms —
  only `/advance` guards (`tasks.go:441-449` names the bug class); the
  same split inside marketplace: get/vote/unvote/install guard,
  versions/delist/relist/by-source don't; swipe dismiss/complete on a
  missing task → raw FK error 500 (`swipe.go:544-553`).
- 400 vs 422 with no rule: missing-field is 400 on
  identity-pat/switch-to-pat/jira-credential but 422 on
  app-import/jira-app/bedrock; "blueprint owned by another team" is
  400 on create/promote but 422 on retarget; "referenced prompt
  doesn't exist" is 422 on steps-PUT but 404 on duplicate; a transfer
  target who isn't a member → 422, an invite team not in the org → 400.
- The documented 404-vs-403 disclosure policy (reference:
  `repos_handler.go:462-482`) is violated by the team surfaces:
  `requireTeamManager` and `resolveTeamForLifecycle` 404 callers who
  can *see* the team via `GET /api/teams`
  (`team_members_handler.go:369`, `team_archive_handler.go:76-81`)
  while `PATCH /api/teams/{team_id}` 403s the same caller class.
- Same-resource sibling verbs disagree: a concluded conversation is
  404 from `/stop` but 409 from `/message` (`agent.go:513` vs
  `654-668`); PUT vote 404s a missing listing, DELETE vote 204s it;
  mode gates answer 501 (skills) vs 404 (marketplace); requeue maps a
  store refusal to "task not found" for a task it just loaded
  (`tasks.go:398-401`); `GET /api/jira/stock` answers 400 for "Jira
  not configured" (server state) but 200 for "still polling".
- `by-source` miss → 200 literal `null` (`marketplace_handler.go:513`),
  the only single read that nulls instead of 404ing.

**Leaks and ad-hoc dialects:**

- `POST …/message` writes raw `err.Error()` at 500 in multi mode,
  bypassing `internalError`'s redaction (`agent.go:625,666`); bedrock
  role connect folds raw AWS errors into the body unconditionally
  (`bedrock_role.go:127`); teams create/rename 409 via driver-string
  matching instead of the shimmed `IsUniqueViolation`
  (`teams_handler.go:222,324`).
- Nascent field-error dialects, each unilateral: `{"error","field"}`;
  `{"error","field","stderr"}` (SSH branches);
  `{"error","archived":true}` (authz 403); `{"error","invited_email"}`
  (invite accept); three shapes from the import handler alone
  (`projects.go:441-457`). The `field` key exists on some credential
  422s and is absent from equally field-specific
  jira/anthropic/bedrock errors.
- Import maps only two error types to 400 — invalid zip, empty bundle,
  traversal, YAML failures, oversize entries are all client faults
  answered 500 (`projects.go:455-461`), and a non-admin's RLS denial
  surfaces as 500 (`internal/projectbundle/import.go:502-503`).
- Success statuses scatter: 200+row / 200+`{"status":…}` / 201+row /
  201 empty / 204 / 202 / raw hand-written `"{}"`
  (`auth_handlers.go:739`) — with warnings smuggled as free prose
  (`{"status":"saved","warning":…}`) or a response *header* on a 204
  (`X-Cleanup-Warning`, `projects.go:833-861`; `X-Diff-Truncated`,
  `artifacts_handler.go:370`).

### 3.5 R5 violations — update semantics

**Updates that should be PATCH but aren't:**

- The task resource has **no PATCH at all** — five verb routes
  instead. `/advance` (`{"to":…}` status write) and `/snooze` are pure
  field writes in verb clothing; the dismiss/complete swipe arms
  likewise; `/undo`, `/requeue`, and delegate carry real side effects.
- `POST /api/settings/team/{id}` and `/org` — pointer-merge (PATCH
  semantics) under POST, with the org save doing read-modify-write
  across **two transactions** with no concurrency token — concurrent
  admins last-write-wins the whole row
  (`settings_handlers.go:642-649,778-783`).
- `PUT /api/event-handlers/{id}` — registered as an alias of the PATCH
  handler; all fields absent-means-keep, so PUT advertises replacement
  it doesn't deliver (`server.go:1143-1144`). `/toggle` duplicates
  PATCH `{enabled}` exactly (three routes for one bit);
  promote/retarget write fields PATCH declares immutable.
- `PUT /api/prompts/{id}` — writes only name/body/model;
  `allowed_tools` is a real wire field (populated by skill upload)
  that **no route can write or clear**, yet it echoes back looking
  accepted (`internal/db/sqlite/prompts.go:182-190`).
- `PUT /api/blueprints/{id}` — writes exactly one field (`name`) under
  the replacement verb; `PUT …/comments/{commentId}` — the body is
  `{body}` only, other fields preserved server-side: single-field
  PATCH wearing PUT.
- POST-as-update throughout: `POST /api/me/active-org`,
  `POST …/identity/pat` (both providers, upsert),
  `POST /api/integrations/setup` (documented rotation path),
  `POST /api/dashboard/prs/{number}/draft` (boolean field write).
- Verb routes that are pure field writes: the review-kind `/dismiss`
  is a CAS `state` flip with zero side effects — while the PR arm of
  the *same route* closes a PR on GitHub
  (`reviews_artifact_handler.go:444-490`); `/curator/reset` = stamp
  `archived_at`; invite `/revoke` = one timestamp (DELETE would do);
  archive/restore are non-idempotent toggles that 409 on repeat.

**"How do I clear a field" has a different answer per route:** JSON
`null` clears on `PATCH /api/repos` (`json.RawMessage`, the only route
distinguishing null from absent — the R5 reference implementation);
empty-string sentinels clear on projects; present-but-empty resets to
*defaults* on team settings; `ai_model` can never be cleared; explicit
`0` clears the usage cap; `{"name": null}` is a silent no-op on
projects/artifacts. Blank-secret means clear on Anthropic, keep on
Bedrock, keep on Slack (two of three fields).

**Replace-set writes with surprises:** PUT team repos validates only
the *added* diff and fail-opens on indeterminate probes — the same
body 200s or 400s depending on cache warmth
(`team_repos.go:130-134,264-278`) — and re-PUTting an identical set is
the secret "re-profile" trigger; PUT reorder takes a bare JSON array
and gates on `ids[0]`'s team only; PUT blueprint steps is a genuine
full replace but returns 204 while siblings return rows; the GitHub
app import is create-only-409 while the Jira sibling silently
upserts — same concept, opposite replace semantics.

---

## 4. Remediation order

Phases are ordered so each one makes the next cheaper. Backward
compatibility matters only where local mode is touched (the SPA ships
with the binary and updates atomically; multi-mode deployments roll
API+SPA together too, so the real constraint is the frontend sweep
landing in the same PR as each surface change).

**Phase 0 — outright bugs (independent of the contract, fix now):**

1. Swipe-snooze writes NULL `snooze_until` → indefinite snooze.
2. `POST /api/event-handlers/{id}/toggle` with an empty body disables
   the handler.
3. Swallowed vault read can activate a GitHub App beside a live PAT
   (App-XOR-PAT invariant break) — `github_app_register.go:564`,
   `github_app_import.go:471`.
4. Double-decoded knowledge paths make `%`-named files unreachable.

**Phase 1 — kernel (§2):** strict decoder + accumulator, R4 error
envelope, list/pagination seam, frontend client parser + hook. All new
routes adopt from here.

**Phase 2 — mechanical sweep:** convert every handler to the kernel
(plain-text eliminations, envelope adoption, strict decode, query-param
strictness, uuid guards). Highest volume, lowest judgment; good
delegation fodder once Phase 1 fixes the shape.

**Phase 3 — read surface:** add the missing single-gets, converge list
envelopes onto `POST /list`, retire aliases (`/api/queue` vs
`/api/tasks`), unify addressing (org/team scoping, artifact read
surfaces).

**Phase 4 — route surgery (per-surface design work):** split the
polymorphic routes (swipe, event-handlers create/PATCH, jira stock,
bedrock/anthropic connect, artifacts kind-dispatch), break up the
settings grab-bags, introduce task PATCH and retire the
field-write verb routes, settle the clearing convention.

Each Phase 4 surface is its own ticket with its own compatibility
notes; Phases 0–2 can proceed without design review.
