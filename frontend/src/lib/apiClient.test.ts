import { describe, it, expect, vi, afterEach } from 'vitest'
import { httpErrorMessage, HttpError, apiJSON } from './apiClient'

describe('httpErrorMessage', () => {
  it('returns the server JSON { error } string when present', () => {
    const e = new HttpError(
      409,
      JSON.stringify({ error: 'an invite for that email is already pending' }),
    )
    expect(httpErrorMessage(e, 'fallback')).toBe('an invite for that email is already pending')
  })

  it('returns the fallback for an HttpError with a non-JSON body (no raw-body leak)', () => {
    // HttpError.message would be "HTTP 500: <html>…</html>" — must NOT surface.
    const e = new HttpError(500, '<html><body>Internal Server Error</body></html>')
    const msg = httpErrorMessage(e, 'Something went wrong.')
    expect(msg).toBe('Something went wrong.')
    expect(msg).not.toContain('<html>')
  })

  it('returns the fallback for JSON without a usable error field', () => {
    expect(httpErrorMessage(new HttpError(400, JSON.stringify({ detail: 'x' })), 'fb')).toBe('fb')
    expect(httpErrorMessage(new HttpError(400, JSON.stringify({ error: '' })), 'fb')).toBe('fb')
    expect(httpErrorMessage(new HttpError(400, JSON.stringify({ error: 42 })), 'fb')).toBe('fb')
  })

  it('surfaces a non-HttpError Error message', () => {
    expect(httpErrorMessage(new TypeError('Failed to fetch'), 'fb')).toBe('Failed to fetch')
  })

  it('returns the fallback for a non-Error throw', () => {
    expect(httpErrorMessage('boom', 'fb')).toBe('fb')
    expect(httpErrorMessage(undefined, 'fb')).toBe('fb')
  })
})

describe('apiJSON non-JSON guard', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

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
