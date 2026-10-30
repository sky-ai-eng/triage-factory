# Backend surface cleanup — target contract

The HTTP surface is the primary way both the SPA and AI agents drive
Triage Factory. An agent navigating an API discovers it the same way a
new engineer does: by reading route names, trying calls, and learning
from errors. Every place the surface is irregular — a route that does
two things, a field that is silently dropped, an error that comes back
as plain text, a list that cannot be paged — is a place an agent (or a
person) draws a wrong conclusion and builds on it.

Status: Completed.

---

## 1. The target contract

Six rules, and every route follows them.

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
  resolved to the default (a corrupt filter must never _widen_ a
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

- Always a **list**, never a single error — validation reports _every_
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
interchangeable. Each says something different about _whose_ data is
being read, and the difference is what tells a reader — and an auditor —
which authorization applies. Writing them down here is the point: the
inconsistency the audit found was never that three forms exist, it was
that nothing said which was which, so new routes picked one at random.

**Session-implied scope — viewer-relative reads.** `/api/me`,
`/api/me/usage`, `/api/dashboard/*`. The subject is _the caller_, and it
comes from the session, never from the path. These are the only routes
where the absence of a scope segment is meaningful: there is no id to
put there, because the answer is different for every caller and no
caller may address another's. A `?user=` on one of these would be an
impersonation surface.

**Path-scoped — admin-scoped resources.** `/api/orgs/{org_id}/…`,
`/api/teams/{team_id}/…`, `/api/teams/{team_id}/usage/…`. The subject is
a _named_ org or team, so the caller is asserting a scope and the
handler authorizes them against it. Anything an admin reads about
somebody else's scope belongs here, because the id in the path is what
the authorization check has to be about. Durable work is addressed this
way, per registered kind: `GET /api/orgs/{org_id}/work` (the catalogue),
`GET /api/orgs/{org_id}/work/{kind}/depth`,
`POST /api/orgs/{org_id}/work/{kind}/items/list`,
`GET /api/orgs/{org_id}/work/{kind}/items/{id}`,
`POST /api/orgs/{org_id}/work/{kind}/items/redrive` and
`POST /api/orgs/{org_id}/work/{kind}/items/cancel` (plus
`POST …/items/{id}/supersede` behind the kind's controls), each gated by
the kind's declared access policy in `internal/server/work_handler.go`.

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
It stays one, which is why the sweep this section deferred got its own
ticket rather than riding along with the shape work: TFAC-872 placed the
ten families that were addressed by something other than their subject,
and the rule it wrote into CLAUDE.md ("The path is the address") is the
version to read — this section is its audit.

What moved, for the record: `/api/setup/start` → `POST /api/orgs` (the
local arm of the create-org route); the two team-settings siblings
the settings decomposition left behind → `/api/teams/{team_id}/`
`github-repos` and `github-groups`; `/api/settings/user` →
`GET|PATCH /api/me/settings`; `github/access/pat*` → `github/pat*`;
`/api/skills/*` → `POST /api/prompts/upload` and
`POST /api/prompts/from-disk`; `/api/factory/delegate` →
`POST /api/tasks` plus the delegate route that already existed; and
**`/api/usage/*` moved under its scopes** — `/api/me/usage`,
`/api/orgs/{org_id}/usage/…`, `/api/teams/{team_id}/usage/…`. The org
half of that one is a real authorization change, not a rename: those
reads took the org from the session and now take it from the path, so
`az.RequireOrgAdmin` gates the named org rather than
`RequireOrgAdminRole` gating whichever one the session pointed at.

Three things deliberately stayed. `/api/fleet/*` keeps its `?org=`: the
subject is the deployment. `/api/dashboard/*` keeps session-implied
scope: viewer-relative by definition. And the per-user GitHub and Jira
identity reads keep their `{org_id}` — the org decides which host the
identity is keyed under, and for Jira it owns the stored credential, so
the segment is what the answer depends on rather than a lookup key.

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
