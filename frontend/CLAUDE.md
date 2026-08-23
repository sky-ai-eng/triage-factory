# CLAUDE.md — frontend

Guidance for the React SPA. The repo-root `CLAUDE.md` covers the binary, the
data model, and the build; this file covers the conventions that only exist in
here. See it for build/lint/test commands (`pnpm run build`, `./scripts/lint.sh`,
`pnpm run test:run`).

## Calling the backend

**`lib/apiClient` is the only door.** Every call to `/api/*` goes through
`apiFetch` (a `Response`) or `apiJSON<T>` (a parsed body). `api/no-raw-api-fetch`
errors on any `fetch()` it cannot prove points elsewhere — a literal
`https://…`, or a root-relative path that isn't `/api`. **A computed URL is
flagged**, including one hoisted into a variable or built by a helper: that is
how ten `/api` calls survived the original migration, so "I can't see where this
points" is a finding, not an exemption. `lib/apiClient.ts` is the one file-level
exemption. Three behaviours live in the wrapper and nowhere else, and a raw
fetch silently opts out of all three:

- **The 401 funnel.** AuthContext registers a handler at mount; `apiFetch`
  invokes it on 401 so a session that expires mid-read re-authenticates. Without
  it the page renders a failed read and sits there until the user reloads.
- **The non-JSON guard.** `apiJSON` re-casts a 2xx whose body isn't JSON as an
  `HttpError` — the Vite dev server answers an unproxied `/api` path with 200 +
  `index.html`, and a bare `res.json()` throws a `SyntaxError` whose parser
  message reaches the UI.
- **One error convention** (below), because everything that fails throws the
  same `HttpError`.

Pick by what the endpoint returns: `apiJSON<T>` for a JSON body, `apiFetch` for
a 204, a blob, a `text()` body, or when you need a response header. **A 204
endpoint must not use `apiJSON`** — 204 is 2xx so it never throws, but there is
no body to parse.

**Never call `res.json()` on an `apiFetch` response.** That re-opens the
non-JSON guard hole the wrapper exists to close: a 2xx HTML body throws a raw
`SyntaxError`, which isn't an `HttpError`, so `httpErrorMessage` passes the
parser's words straight to the UI. When a call needs the `Response` _and_ JSON
on the success path, compose the two steps — `readJSON<T>(resp, path)` is
`apiJSON`'s parse half, exported for exactly that:

```ts
// `allow: [401]` must stay: firing the re-auth funnel here would turn every
// render of a logged-out surface into a redirect.
apiFetch('/api/me', { allow: [401] }).then((r) =>
  r.status === 401 ? null : readJSON<MeResponse>(r, '/api/me'),
)
```

A body read best-effort (`res.json().catch(() => null)`, for a DELETE whose body
is optional) is fine as-is — the `catch` already absorbs the parse failure.

### Tolerating a status

A non-2xx that is an _answer_ rather than a failure goes in `allow`:

```ts
// A 404 means the prompt was already answered — the caller drops it either way.
const res = await apiFetch(`/api/agent/conversations/${id}/permissions/${toolCallID}`, {
  method: 'POST',
  body: JSON.stringify(decision),
  allow: [404],
})
return res.status === 404 ? { kind: 'gone' } : { kind: 'resolved' }
```

Listing 401 additionally suppresses the global re-auth handler — a caller that
tolerates 401 is handling it, so `loadMe()` returning `null` for "not signed in"
doesn't kick off a redirect. That is the one case where `allow` is load-bearing
rather than a convenience.

`allow` states the intent at the call site, and it is the only option when
suppressing the 401 funnel is the point. Otherwise **prefer branching in a
`catch` on `HttpError.status`**, which keeps `apiJSON`'s parse for the success
path — a 404 → not-found state has no funnel to suppress, so it wants the catch,
not `allow`. `contexts/AuthContext.tsx`, `hooks/useConversationDetail.ts`
and `hooks/useInvites.ts` are the reference shape.

### Error strings

**`httpErrorMessage(e, fallback)` is the only error-string helper.** It returns
the server's `{ error }` verbatim, else the fallback. Nothing prefixes it, so
**fallbacks are whole sentences** — `'Could not load the run.'`, not
`'load run'`. An `HttpError`'s own `.message` embeds the raw response body
and must never reach the UI.

```ts
try {
  const data = await apiJSON<Conversation>(`/api/agent/conversations/${encodeURIComponent(id)}`)
  setConversation(data)
} catch (err) {
  setError(httpErrorMessage(err, 'Could not load the run.'))
}
```

A module in `lib/` whose callers render `err.message` directly wraps this once
(`asError` in `lib/githubApp.ts`, `lib/jiraApp.ts`, `lib/teamLifecycle.ts`)
rather than letting an `HttpError` escape.

### Paths

**Org paths are written in full**: `` `/api/orgs/${encodeURIComponent(orgId)}/members` ``.
There is no org-prefixing option — the prefix is one interpolation at the call
site, and a helper that hides it splits every path in the app into two
spellings for no gain.

### Where a call lives

A surface with more than a call or two gets a module of **exported functions**
in `lib/` — `lib/teamLifecycle.ts`, `lib/githubApp.ts`, `lib/jiraApp.ts` — not
inline URLs scattered through the component. Functions, not classes: the binary
serves the SPA and `/api/*` from one origin and there is a single websocket
(`hooks/useWebSocket.ts` on `/api/ws`), so there is no base URL to bind and
nothing per-service to hold.

### Testing a call

Stub the global `fetch` with `jsonBody` from `src/test/apiResponse.ts`:

```ts
vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true, ...jsonBody(rows) }))
```

`apiJSON` reads the body with `text()`, so a stub implementing only `json()`
fails on `resp.text is not a function`. A call-args assertion should name the
URL and let the wrapper's own additions through —
`expect(fetchMock).toHaveBeenCalledWith(url, expect.objectContaining({ method: 'POST' }))`.

## Lint rules

Two repo-local ESLint rules live in `frontend/eslint-rules/`, each with its own
test file and registered in `eslint.config.js`:

- `api/no-raw-api-fetch` — the above. It reports two ways: `rawFetch` names a
  literal `/api` path, `opaqueFetch` says the URL isn't visible to the lint. A
  genuinely-external fetch built from a variable needs an inline disable naming
  where it points; none exists today.
- `conversation-status/no-ghost-conversation-status` — fails the lint when component code compares
  a conversation status against a name outside the vocabulary in `src/types.ts`
  (mirrored from `internal/domain/conversation_status.go`).
