/**
 * apiClient wraps fetch with the cookie + org-prefix conventions the
 * D8 multi-mode flow needs:
 *
 *  - `credentials: 'include'` is set unconditionally so the sid cookie
 *    travels on every request. In local mode no cookie exists so this
 *    is a no-op.
 *  - `org` is opt-in: when provided as a string, the path is prefixed
 *    with `/api/orgs/<org>/`. Cross-org endpoints (`/api/me`,
 *    `/api/auth/*`, `/api/config`) call without `org`. This is the
 *    inverse of sky-frontend's pattern: we make org-scoping explicit
 *    rather than the default, because most D7 endpoints aren't
 *    retrofitted yet — defaulting to a prefix would 404 them all.
 *  - Non-2xx responses throw HttpError so callers can branch on
 *    `status === 401` for re-auth. No automatic redirect at the
 *    wrapper layer; that's the caller's call (AuthContext catches 401
 *    on /api/me and AuthGate handles routing).
 *
 * The wrapper is also the chokepoint where 401 is funneled to
 * AuthContext via the registered handler — AuthContext registers a
 * callback at startup, the wrapper invokes it on 401. Decouples the
 * fetch layer from the router.
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
  /** When provided, prefix the path with `/api/orgs/<org>/`. The
   *  caller is responsible for not double-prefixing — `path` should
   *  start with a slash and NOT include `/api/` (we add it). For
   *  cross-org endpoints (auth, /api/me, /api/config), omit `org` and
   *  pass the full `/api/...` path. */
  org?: string
}

function resolveUrl(path: string, org: string | undefined): string {
  if (org) {
    const cleaned = path.startsWith('/') ? path : '/' + path
    return '/api/orgs/' + encodeURIComponent(org) + cleaned
  }
  return path
}

export async function apiFetch(path: string, options: ApiFetchOptions = {}): Promise<Response> {
  const { org, headers, ...rest } = options
  const url = resolveUrl(path, org)

  const resp = await fetch(url, {
    ...rest,
    credentials: 'include',
    headers: {
      ...(headers ?? {}),
    },
  })

  if (resp.status === 401 && unauthHandler) {
    // Notify AuthContext before throwing so the redirect happens even
    // when the caller doesn't catch. Throwing still lets the caller's
    // try/catch surface the failure for messaging.
    unauthHandler()
  }

  if (!resp.ok) {
    const body = await resp.text().catch(() => '')
    throw new HttpError(resp.status, body)
  }
  return resp
}

export async function apiJSON<T>(path: string, options: ApiFetchOptions = {}): Promise<T> {
  const resp = await apiFetch(path, options)
  // apiFetch guarantees a 2xx here, but a 2xx body isn't guaranteed to be JSON:
  // the Vite dev server answers an unproxied /api path with a 200 index.html, and
  // a misconfigured proxy / gateway can do the same. A bare resp.json() there
  // throws a native SyntaxError ("Unexpected token '<'…") that bypasses
  // httpErrorMessage's HttpError sanitizing and leaks the parser message to the
  // UI. Re-cast a non-JSON body as an HttpError so callers land on the same clean
  // fallback path as any other failed request.
  const text = await resp.text()
  try {
    return JSON.parse(text) as T
  } catch {
    throw new HttpError(
      resp.status,
      text,
      `Expected JSON from ${resolveUrl(path, options.org)} but received a non-JSON response`,
    )
  }
}

/** httpErrorMessage extracts a user-facing string from a caught error,
 *  preferring the server's JSON `{ error }` body (so a backend message — e.g.
 *  the invite 409 "already pending" or the last-owner guard — reaches the user
 *  verbatim), then the supplied fallback.
 *
 *  An HttpError WITHOUT a usable JSON `{ error }` returns the fallback, not its
 *  own `.message`: that message embeds the raw response body (`HTTP <status>:
 *  <body>`), which for a non-JSON failure (a 500 HTML page, a proxy error) is
 *  large/unfriendly and must never reach the UI. Only a non-HttpError Error
 *  (a thrown TypeError, an abort) surfaces its `.message`. */
export function httpErrorMessage(e: unknown, fallback: string): string {
  if (e instanceof HttpError) {
    try {
      const body = JSON.parse(e.body) as { error?: unknown }
      if (typeof body.error === 'string' && body.error) return body.error
    } catch {
      // body wasn't JSON — fall through to the fallback below.
    }
    return fallback
  }
  return e instanceof Error ? e.message : fallback
}
