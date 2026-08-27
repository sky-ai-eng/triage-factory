import { describe, it, expect, vi, afterEach } from 'vitest'
import {
  apiErrors,
  apiFetch,
  apiJSON,
  apiList,
  httpErrorMessage,
  HttpError,
  setUnauthHandler,
} from './apiClient'

describe('httpErrorMessage', () => {
  it('prefers the envelope first message over the legacy error key', () => {
    // Converted routes dual-emit during the migration; the envelope item is
    // the contract, the top-level "error" only mirrors it.
    const e = new HttpError(
      400,
      JSON.stringify({
        errors: [{ reason: 'MISSING_FIELD', message: 'until is required', field: 'until' }],
        error: 'until is required',
      }),
    )
    expect(httpErrorMessage(e, 'fallback')).toBe('until is required')
  })

  it('returns the fallback for a legacy single-string { error } body', () => {
    // The dual-emission shim is gone: every /api/* failure carries the
    // envelope, so a bare { error } body is not one of ours and its string is
    // not shown to the user.
    const e = new HttpError(
      409,
      JSON.stringify({ error: 'an invite for that email is already pending' }),
    )
    expect(httpErrorMessage(e, 'fallback')).toBe('fallback')
  })

  it('returns the fallback for an HttpError with a non-JSON body (no raw-body leak)', () => {
    // HttpError.message would be "HTTP 500: <html>…</html>" — must NOT surface.
    const e = new HttpError(500, '<html><body>Internal Server Error</body></html>')
    const msg = httpErrorMessage(e, 'Something went wrong.')
    expect(msg).toBe('Something went wrong.')
    expect(msg).not.toContain('<html>')
  })

  it('returns the fallback for JSON without a usable envelope', () => {
    expect(httpErrorMessage(new HttpError(400, JSON.stringify({ detail: 'x' })), 'fb')).toBe('fb')
    expect(httpErrorMessage(new HttpError(400, JSON.stringify({ errors: [] })), 'fb')).toBe('fb')
    expect(
      httpErrorMessage(new HttpError(400, JSON.stringify({ errors: [{ reason: 'X' }] })), 'fb'),
    ).toBe('fb')
  })

  it('surfaces a non-HttpError Error message', () => {
    expect(httpErrorMessage(new TypeError('Failed to fetch'), 'fb')).toBe('Failed to fetch')
  })

  it('returns the fallback for a non-Error throw', () => {
    expect(httpErrorMessage('boom', 'fb')).toBe('fb')
    expect(httpErrorMessage(undefined, 'fb')).toBe('fb')
  })
})

describe('apiErrors', () => {
  it('returns every envelope item, fields included', () => {
    const e = new HttpError(
      400,
      JSON.stringify({
        errors: [
          { reason: 'MISSING_FIELD', message: 'entity_id is required', field: 'entity_id' },
          { reason: 'MISSING_FIELD', message: 'blueprint_id is required', field: 'blueprint_id' },
        ],
        error: 'entity_id is required',
      }),
    )
    expect(apiErrors(e)).toEqual([
      { reason: 'MISSING_FIELD', message: 'entity_id is required', field: 'entity_id' },
      { reason: 'MISSING_FIELD', message: 'blueprint_id is required', field: 'blueprint_id' },
    ])
  })

  it('returns [] for non-envelope bodies, non-JSON bodies, and non-HttpErrors', () => {
    expect(apiErrors(new HttpError(409, JSON.stringify({ error: 'nope' })))).toEqual([])
    expect(apiErrors(new HttpError(500, '<html>boom</html>'))).toEqual([])
    expect(apiErrors(new TypeError('Failed to fetch'))).toEqual([])
  })

  it('drops malformed items instead of returning junk', () => {
    const e = new HttpError(
      400,
      JSON.stringify({
        errors: [{ reason: 'CONFLICT', message: 'real item' }, { message: 42 }, 'garbage'],
      }),
    )
    expect(apiErrors(e)).toEqual([{ reason: 'CONFLICT', message: 'real item' }])
  })
})

describe('apiJSON non-JSON guard', () => {
  // Minimal Response stand-in: apiFetch reads status/ok/text(), the hardened
  // apiJSON reads text(). Avoids depending on a Response global across envs.
  function stubFetch(status: number, body: string) {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: status >= 200 && status < 300,
        status,
        text: () => Promise.resolve(body),
      } as unknown as Response),
    )
  }

  const caught = (p: Promise<unknown>) =>
    p.then(
      () => null,
      (e: unknown) => e,
    )

  it('parses a JSON 2xx body', async () => {
    stubFetch(200, JSON.stringify({ ok: true }))
    await expect(apiJSON<{ ok: boolean }>('/api/thing')).resolves.toEqual({ ok: true })
  })

  it('throws a clean HttpError (not a raw SyntaxError) when a 2xx body is HTML', async () => {
    // The dev server serves 200 index.html for an unproxied /api path; a bare
    // resp.json() would throw "Unexpected token '<'". apiJSON must re-cast it so
    // httpErrorMessage sanitizes it to the caller's fallback and never leaks the
    // markup or the parser message.
    stubFetch(200, '<!doctype html><html><body>app</body></html>')
    const err = await caught(apiJSON('/api/sso/connection'))
    expect(err).toBeInstanceOf(HttpError)
    const msg = httpErrorMessage(err, 'Could not load SSO settings.')
    expect(msg).toBe('Could not load SSO settings.')
    expect(msg).not.toContain('<')
  })

  it('still surfaces a non-2xx as an HttpError with its status (unchanged)', async () => {
    stubFetch(404, '404 page not found')
    const err = await caught(apiJSON('/api/sso/connection'))
    expect(err).toBeInstanceOf(HttpError)
    expect((err as HttpError).status).toBe(404)
  })
})

describe('apiFetch allow', () => {
  afterEach(() => {
    setUnauthHandler(null)
  })

  function stubFetch(status: number, body = '') {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: status >= 200 && status < 300,
        status,
        text: () => Promise.resolve(body),
      } as unknown as Response),
    )
  }

  const caught = (p: Promise<unknown>) =>
    p.then(
      () => null,
      (e: unknown) => e,
    )

  it('resolves a listed status instead of throwing, and hands back the response', async () => {
    stubFetch(404, 'not found')
    const res = await apiFetch('/api/agent/conversations/x/permissions/y', { allow: [404] })
    // The point of `allow` is that the caller branches on the status itself —
    // a 404 that means "already answered" is an outcome, not a failure.
    expect(res.status).toBe(404)
  })

  it('still throws for a status the caller did not list', async () => {
    stubFetch(409, JSON.stringify({ error: 'a turn is in flight' }))
    const err = await caught(apiFetch('/api/thing', { allow: [404] }))
    expect(err).toBeInstanceOf(HttpError)
    expect((err as HttpError).status).toBe(409)
  })

  it('does not invoke the unauth handler when 401 is allowed', async () => {
    // loadMe()'s case: "not signed in" is the answer the caller wants, so
    // firing the global re-auth funnel would turn every logged-out render into
    // a redirect.
    const onUnauth = vi.fn()
    setUnauthHandler(onUnauth)
    stubFetch(401, '')
    const res = await apiFetch('/api/me', { allow: [401] })
    expect(res.status).toBe(401)
    expect(onUnauth).not.toHaveBeenCalled()
  })

  it('invokes the unauth handler on a plain 401', async () => {
    // The behaviour the whole sweep exists to restore: a session that expires
    // mid-read re-authenticates instead of rendering a dead page.
    const onUnauth = vi.fn()
    setUnauthHandler(onUnauth)
    stubFetch(401, '')
    const err = await caught(apiFetch('/api/repos'))
    expect(onUnauth).toHaveBeenCalledTimes(1)
    expect(err).toBeInstanceOf(HttpError)
  })

  it('threads allow through apiJSON, parsing the allowed status body', async () => {
    stubFetch(409, JSON.stringify({ error: 'already pending' }))
    await expect(apiJSON<{ error: string }>('/api/invites', { allow: [409] })).resolves.toEqual({
      error: 'already pending',
    })
  })
})

describe('apiList', () => {
  it('POSTs the filters and returns the envelope', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      text: () =>
        Promise.resolve(
          JSON.stringify({ items: [{ id: 'a' }], next_page_token: 'tok', total_count: 7 }),
        ),
    } as unknown as Response)
    vi.stubGlobal('fetch', fetchMock)

    const page = await apiList<{ id: string }>('/api/tasks/list', { statuses: ['queued'] })

    expect(page).toEqual({ items: [{ id: 'a' }], next_page_token: 'tok', total_count: 7 })
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/tasks/list',
      expect.objectContaining({ method: 'POST', body: JSON.stringify({ statuses: ['queued'] }) }),
    )
  })

  it('normalizes a last page, which omits next_page_token', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        text: () => Promise.resolve(JSON.stringify({ items: [], total_count: 0 })),
      } as unknown as Response),
    )

    // Callers branch on `next_page_token !== ''`; an absent field must not
    // reach them as undefined.
    await expect(apiList('/api/tasks/list', {})).resolves.toEqual({
      items: [],
      next_page_token: '',
      total_count: 0,
    })
  })
})
