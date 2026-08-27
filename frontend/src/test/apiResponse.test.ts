import { describe, it, expect, vi } from 'vitest'
import { apiFetch, apiJSON, httpErrorMessage, HttpError } from '../lib/apiClient'
import { jsonBody } from './apiResponse'

// The stub helper has its own test because eleven test files build their fetch
// mocks from it: a defect here reads as a defect in whatever component happened
// to be under test, which is the worst place to debug it from.

describe('jsonBody', () => {
  const caught = (p: Promise<unknown>) =>
    p.then(
      () => null,
      (e: unknown) => e,
    )

  it('fills both readers from one value', async () => {
    const rows = [{ id: 1 }, { id: 2 }]
    const stub = jsonBody(rows)
    await expect(stub.json()).resolves.toEqual(rows)
    await expect(stub.text()).resolves.toBe('[{"id":1},{"id":2}]')
  })

  it('awaits a promised body, so a parked read still answers', async () => {
    // useConversationDetail's tests hold the transcript read open to exercise the window
    // where the conversation's SUM is displayed before the ids inside it are known.
    let settle!: (v: unknown) => void
    const stub = jsonBody(
      new Promise((resolve) => {
        settle = resolve
      }),
    )
    settle([{ id: 7 }])
    await expect(stub.json()).resolves.toEqual([{ id: 7 }])
    await expect(stub.text()).resolves.toBe('[{"id":7}]')
  })

  it('honours the Response.text(): Promise<string> contract for an absent body', async () => {
    // `{ ok: false }` with no `body` is how a failure case is written, and
    // JSON.stringify(undefined) is the VALUE undefined, not a string.
    await expect(jsonBody(undefined).text()).resolves.toBe('')
    expect(typeof (await jsonBody(undefined).text())).toBe('string')
    // Not the same thing as a null body, which serializes.
    await expect(jsonBody(null).text()).resolves.toBe('null')
  })

  it('rejects from json() on an absent body, as a real Response does', async () => {
    // The two readers must agree: a body text() calls empty is not one json()
    // can hand back a value for.
    expect(await caught(jsonBody(undefined).json())).toBeInstanceOf(SyntaxError)
    await expect(jsonBody(null).json()).resolves.toBeNull()
  })

  it('does not leak "undefined" into an HttpError a body-less failure produces', async () => {
    // The regression: text() answering `undefined` made apiFetch build
    // `HTTP 500: undefined`, and any assertion on the message read as a
    // component bug rather than a stub one.
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({ ok: false, status: 500, ...jsonBody(undefined) }),
    )
    const err = (await caught(apiFetch('/api/thing'))) as HttpError
    expect(err).toBeInstanceOf(HttpError)
    expect(err.body).toBe('')
    expect(err.message).not.toContain('undefined')
    // And it still degrades to the caller's fallback, like any non-JSON body.
    expect(httpErrorMessage(err, 'Could not load the thing.')).toBe('Could not load the thing.')
  })

  it('drives apiJSON the way the real endpoints do', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({ ok: true, status: 200, ...jsonBody([1, 2]) }),
    )
    await expect(apiJSON<number[]>('/api/thing')).resolves.toEqual([1, 2])
  })
})
