import { describe, it, expect } from 'vitest'
import { httpErrorMessage, HttpError } from './apiClient'

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
