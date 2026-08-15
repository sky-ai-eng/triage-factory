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
      `Expected JSON from ${path} but received a non-JSON response`,
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
