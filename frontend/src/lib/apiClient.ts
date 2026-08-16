/**
 * apiClient is the only door to `/api/*`. Every call to the backend goes
 * through `apiFetch` or `apiJSON`; a bare `fetch('/api/…')` anywhere else is a
 * lint error (`api/no-raw-api-fetch`). That is not a style preference — three
 * behaviours live in here and only here:
 *
 *  - `credentials: 'include'` is set unconditionally so the sid cookie
 *    travels on every request. In local mode no cookie exists so this
 *    is a no-op.
 *  - 401 is funneled to AuthContext via the registered handler — AuthContext
 *    registers a callback at mount, the wrapper invokes it on 401. That is what
 *    re-authenticates a session that expires mid-read, and it decouples the
 *    fetch layer from the router. A raw fetch never reaches it, so the page
 *    just renders a failed read and stays there.
 *  - Non-2xx responses throw HttpError, so `httpErrorMessage` can turn any
 *    failure into the same user-facing string. No automatic redirect at the
 *    wrapper layer; that's the caller's call (AuthContext catches 401 on
 *    /api/me and AuthGate handles routing).
 *
 * Paths are written in full — `/api/orgs/${orgId}/…`. There is no org-prefixing
 * option: the prefix is one interpolation at the call site, and a helper that
 * hides it splits every path in the app into two spellings for no gain.
 */

export class HttpError extends Error {
  status: number
  body: string
  constructor(status: number, body: string, message?: string) {
    super(message ?? `HTTP ${status}: ${body}`)
    this.status = status
    this.body = body
  }
}

type UnauthHandler = () => void

let unauthHandler: UnauthHandler | null = null

/** Registers the global 401 handler. AuthContext calls this once at
 *  mount. Replaces any prior handler — one consumer at a time. */
export function setUnauthHandler(handler: UnauthHandler | null) {
  unauthHandler = handler
}

export interface ApiFetchOptions extends RequestInit {
  /** Statuses that resolve rather than throw — the response comes back and the
   *  caller branches on `status` itself. This is how a call says "a 404 here is
   *  an answer, not a failure" at the site where that is true, instead of in a
   *  catch block that has to re-derive it.
   *
   *  Listing 401 also suppresses the global re-auth handler: a caller that
   *  tolerates 401 is handling it (`loadMe()` returning null for "not signed
   *  in" must not kick off a re-auth), and one knob covering both opt-outs
   *  keeps them from drifting apart. */
  allow?: number[]
}

export async function apiFetch(path: string, options: ApiFetchOptions = {}): Promise<Response> {
  const { allow, headers, ...rest } = options

  const resp = await fetch(path, {
    ...rest,
    credentials: 'include',
    headers: {
      ...(headers ?? {}),
    },
  })

  const allowed = allow?.includes(resp.status) ?? false

  if (resp.status === 401 && !allowed && unauthHandler) {
    // Notify AuthContext before throwing so the redirect happens even
    // when the caller doesn't catch. Throwing still lets the caller's
    // try/catch surface the failure for messaging.
    unauthHandler()
  }

  if (!resp.ok && !allowed) {
    const body = await resp.text().catch(() => '')
    throw new HttpError(resp.status, body)
  }
  return resp
}

/** readJSON parses a response body as JSON, re-casting a non-JSON body as an
 *  HttpError.
 *
 *  A 2xx body isn't guaranteed to be JSON: the Vite dev server answers an
 *  unproxied /api path with a 200 index.html, and a misconfigured proxy /
 *  gateway can do the same. A bare `resp.json()` there throws a native
 *  SyntaxError ("Unexpected token '<'…") which is NOT an HttpError, so
 *  httpErrorMessage surfaces its `.message` verbatim and the parser's words
 *  reach the UI. Re-casting lands the caller on the same clean fallback path as
 *  any other failed request.
 *
 *  `apiJSON` is `apiFetch` + this. It is exported for the one shape that can't
 *  use apiJSON: a caller that needs the Response itself — an `allow`ed status to
 *  branch on, a header, a 204 — and JSON on the success path. Reading that path
 *  with a bare `resp.json()` re-opens exactly the leak above. */
export async function readJSON<T>(resp: Response, path: string): Promise<T> {
  const text = await resp.text()
  try {
    return JSON.parse(text) as T
  } catch {
    throw new HttpError(
      resp.status,
      text,
      `Expected JSON from ${path} but received a non-JSON response`,
    )
  }
}

export async function apiJSON<T>(path: string, options: ApiFetchOptions = {}): Promise<T> {
  return readJSON<T>(await apiFetch(path, options), path)
}

/** The list envelope every paginated read answers with: one page of `items`,
 *  the `total_count` the filters match (not the page length), and the token
 *  that addresses the next page — absent on the last one.
 *
 *  The token is opaque. Do not parse it, construct one, or carry it across a
 *  change of filters: the server mints it against the filter set it was issued
 *  for and rejects it under any other (see `apiList`). */
export interface ListPage<T> {
  items: T[]
  next_page_token: string
  /** The filtered total across every page, or `null` when the resource cannot
   *  count itself — a proxy list over an upstream API that reports no total
   *  (see `httpx.WriteProxyList`). `null` is never zero and never a truncation
   *  signal: `next_page_token` is the only "there is more" on any list. */
  total_count: number | null
}

/** Request shape for a `POST /api/<resource>/list` read: the route's filters
 *  plus the two paging fields. `page_size` defaults to 50 server-side and is
 *  capped at 200 — an out-of-range value is a 400, never a silent clamp. */
export type ListRequest = Record<string, unknown> & {
  page_size?: number
  page_token?: string
}

/** apiList calls a `POST /api/<resource>/list` read and normalizes the
 *  envelope, so callers never guard for a missing field.
 *
 *  It is a POST because the filters are a body — the method is about how the
 *  request is framed, not about whether it mutates. The read itself has no
 *  side effects.
 *
 *  `options` passes through to `apiFetch` for the few callers that need an
 *  AbortSignal or an `allow` status. The method, content type, and body are
 *  the hook's own: the caller's headers are spread FIRST so its
 *  `Content-Type` cannot win. That ordering is load-bearing rather than
 *  stylistic — the list routes decode strictly, and a caller that set some
 *  other content type would send a JSON body under a header the server
 *  doesn't decode as JSON, failing the request for a reason nothing in the
 *  call site names. */
export async function apiList<T>(
  path: string,
  body: ListRequest,
  options: ApiFetchOptions = {},
): Promise<ListPage<T>> {
  const page = await apiJSON<Partial<ListPage<T>>>(path, {
    ...options,
    method: 'POST',
    headers: { ...options.headers, 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  return {
    items: page.items ?? [],
    next_page_token: page.next_page_token ?? '',
    total_count: page.total_count ?? null,
  }
}

/** The most rows `apiListAll` will collect before it gives up. A declared
 *  ceiling, not a guess: past a couple of thousand options a client-side filter
 *  is the wrong affordance and a server-side search is the right one. Expressed
 *  in rows rather than pages so a caller that has to use a smaller page (an
 *  upstream-capped proxy list) gets the same bound. */
const MAX_ROWS_TO_WALK = 2000

/** apiListAll walks a `POST /…/list` read to completion and returns every row.
 *
 *  **Use it only where a surface genuinely needs the whole set**, which in
 *  practice means one thing: a consumer that filters or reasons over the
 *  options CLIENT-side. A partial load there is not a shorter list — it is a
 *  search that silently misses, a coverage check that answers "none" about a
 *  row on page three, a reorder that submits a partial order. Any surface that
 *  renders rows for reading wants `usePagedList` instead, so the reader sees a
 *  page and asks for the next one.
 *
 *  It walks with the server's own tokens, never a computed offset, and
 *  **throws** rather than returning a short set once the walk passes
 *  `MAX_ROWS_TO_WALK`. Throwing is the point: the ceiling exists to bound a
 *  runaway, so reaching it means the caller's premise — that this set fits in
 *  memory and can be reasoned over whole — is false. Returning the rows it
 *  managed to collect alongside a flag put the burden on every caller to notice
 *  and say so, and no caller did; an exception lands on the error path each one
 *  already has, and turns a quietly wrong answer into a visible failure.
 *
 *  `pageSize` exists for the routes that declare a smaller maximum than the
 *  standard 200 — a proxy list capped at its upstream's own page size, where
 *  asking for more is a 400. */
export async function apiListAll<T>(
  path: string,
  body: ListRequest = {},
  options: ApiFetchOptions = {},
  pageSize = 200,
): Promise<T[]> {
  const items: T[] = []
  let token = ''
  while (items.length <= MAX_ROWS_TO_WALK) {
    const page: ListPage<T> = await apiList<T>(
      path,
      { ...body, page_size: pageSize, ...(token ? { page_token: token } : {}) },
      options,
    )
    items.push(...page.items)
    token = page.next_page_token
    if (!token) return items
  }
  throw new Error(
    `${path} has more than ${MAX_ROWS_TO_WALK} rows; this surface reads the whole set and cannot show a partial one`,
  )
}

/** One fault in the server's error envelope: `{"errors": [{reason, message,
 *  field?}]}`. `reason` is a stable machine code (SCREAMING_SNAKE, e.g.
 *  `NOT_CONFIGURED`); `message` is prose; `field` names the payload field at
 *  fault when one specific field is. */
export interface ApiErrorItem {
  reason: string
  message: string
  field?: string
}

/** apiErrors extracts the envelope's error list from a caught error. Returns
 *  every item — validation reports all failing fields at once, so a form can
 *  highlight each `field`. A non-HttpError or a non-JSON body yields `[]`; use
 *  `httpErrorMessage` when all you need is one string. */
export function apiErrors(e: unknown): ApiErrorItem[] {
  if (!(e instanceof HttpError)) return []
  try {
    const body = JSON.parse(e.body) as { errors?: unknown }
    if (!Array.isArray(body.errors)) return []
    return body.errors.filter(
      (item): item is ApiErrorItem =>
        typeof item === 'object' &&
        item !== null &&
        typeof (item as ApiErrorItem).reason === 'string' &&
        typeof (item as ApiErrorItem).message === 'string',
    )
  } catch {
    return []
  }
}

/** httpErrorMessage extracts a user-facing string from a caught error: the
 *  envelope's first `message` when present, else the supplied fallback. The
 *  legacy top-level `{ error }` fallback is gone — every `/api/*` failure
 *  speaks the envelope, so a body without one is a body from outside the API
 *  (a proxy page, a dev-server index.html), which has nothing to show a user.
 *
 *  An HttpError WITHOUT a usable body returns the fallback, not its own
 *  `.message`: that message embeds the raw response body (`HTTP <status>:
 *  <body>`), which for a non-JSON failure (a 500 HTML page, a proxy error) is
 *  large/unfriendly and must never reach the UI. Only a non-HttpError Error
 *  (a thrown TypeError, an abort) surfaces its `.message`. */
export function httpErrorMessage(e: unknown, fallback: string): string {
  if (e instanceof HttpError) {
    const items = apiErrors(e)
    if (items.length > 0 && items[0].message) return items[0].message
    return fallback
  }
  return e instanceof Error ? e.message : fallback
}
