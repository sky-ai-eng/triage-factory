# CLAUDE.md — frontend

Guidance for the React SPA. The repo-root `CLAUDE.md` covers the binary, the
data model, and the build; this file covers the conventions that only exist in
here. See it for build/lint/test commands (`pnpm run build`, `./scripts/lint.sh`,
`pnpm run test:run`).

## Calling the backend

**`lib/apiClient` is the only door.** Every call to `/api/*` goes through
`apiFetch` (a `Response`) or `apiJSON<T>` (a parsed body). A bare
`fetch('/api/…')` is a lint error (`api/no-raw-api-fetch`); `lib/apiClient.ts`
itself is the one exemption. Three behaviours live in the wrapper and nowhere
else, and a raw fetch silently opts out of all three:

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

`allow` states the intent at the call site. Branching in a `catch` on
`HttpError.status` is equally correct and preferable when you want `apiJSON`'s
parse for the success path — `contexts/AuthContext.tsx`, `hooks/useInvites.ts`
and `pages/InviteAccept.tsx` are the reference shape.

### Error strings

**`httpErrorMessage(e, fallback)` is the only error-string helper.** It returns
the server's `{ error }` verbatim, else the fallback. Nothing prefixes it, so
**fallbacks are whole sentences** — `'Could not load the project.'`, not
`'load project'`. An `HttpError`'s own `.message` embeds the raw response body
and must never reach the UI.

```ts
try {
  const data = await apiJSON<Project>(`/api/projects/${encodeURIComponent(id)}`)
  setProject(data)
} catch (err) {
  setError(httpErrorMessage(err, 'Could not load the project.'))
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

- `api/no-raw-api-fetch` — the above.
- `run-status/no-ghost-run-status` — fails the lint when component code compares
  a conversation status against a name outside the vocabulary in `src/types.ts`
  (mirrored from `internal/domain/run_status.go`).
