import { describe, it, expect, vi } from 'vitest'
import { fetchPendingPermissions, ttlForPrompt, PERMISSION_TTL_GRACE_MS } from './permissions'
import { jsonBody } from '../test/apiResponse'

// fetchPendingPermissions is the boundary between "the server told us what is
// pending" and "we couldn't ask". Those are different facts and the caller acts
// on them differently, so the distinction is pinned here rather than left to a
// convention a future edit could quietly collapse.

function stubFetch(impl: () => Promise<unknown> | never) {
  const mock = vi.fn(impl)
  vi.stubGlobal('fetch', mock)
  return mock
}

describe('fetchPendingPermissions', () => {
  it('returns the pending set on a 200', async () => {
    stubFetch(() =>
      Promise.resolve({
        ok: true,
        ...jsonBody([{ tool_call_id: 'toolu_1', tool_name: 'Bash', input: { command: 'ls' } }]),
      }),
    )
    const got = await fetchPendingPermissions('run-1')
    expect(got).toHaveLength(1)
    expect(got?.[0].input).toEqual({ command: 'ls' })
  })

  it('distinguishes "nothing pending" from "could not ask"', async () => {
    // [] is the server stating there is nothing outstanding — the caller clears
    // its queue on it. null is the absence of an answer, on which the caller
    // must change nothing; collapsing the two blanks live prompts on a blip.
    stubFetch(() => Promise.resolve({ ok: true, ...jsonBody([]) }))
    expect(await fetchPendingPermissions('run-1')).toEqual([])

    stubFetch(() => Promise.resolve({ ok: false, status: 500, ...jsonBody(null) }))
    expect(await fetchPendingPermissions('run-1')).toBeNull()

    stubFetch(() => Promise.reject(new Error('network down')))
    expect(await fetchPendingPermissions('run-1')).toBeNull()

    stubFetch(() => Promise.resolve({ ok: true, ...jsonBody({ error: 'nope' }) }))
    expect(await fetchPendingPermissions('run-1')).toBeNull()

    // A 2xx whose body isn't JSON — the dev server's index.html is the real
    // case. apiJSON re-casts it as an HttpError, so this lands in the same
    // "couldn't ask" arm rather than throwing a raw SyntaxError at the caller.
    stubFetch(() =>
      Promise.resolve({ ok: true, text: () => Promise.resolve('<!doctype html><html></html>') }),
    )
    expect(await fetchPendingPermissions('run-1')).toBeNull()
  })

  it('fills in a missing input rather than passing undefined through', async () => {
    // The response is cast, not validated, and consumers index into `input`
    // directly — so an absent key would be a render crash rather than a
    // thinner prompt.
    stubFetch(() =>
      Promise.resolve({
        ok: true,
        ...jsonBody([{ tool_call_id: 'toolu_1', tool_name: 'Bash' }]),
      }),
    )
    const got = await fetchPendingPermissions('run-1')
    expect(got?.[0].input).toEqual({})
  })
})

describe('ttlForPrompt', () => {
  it('derives the dismiss TTL from the payload deadline plus grace', () => {
    expect(
      ttlForPrompt({ tool_call_id: 'a', tool_name: 'Bash', input: {}, timeout_ms: 1000 }),
    ).toBe(1000 + PERMISSION_TTL_GRACE_MS)
  })

  it('falls back when the payload carries no usable deadline', () => {
    const noDeadline = ttlForPrompt({ tool_call_id: 'a', tool_name: 'Bash', input: {} })
    const zeroDeadline = ttlForPrompt({
      tool_call_id: 'a',
      tool_name: 'Bash',
      input: {},
      timeout_ms: 0,
    })
    expect(zeroDeadline).toBe(noDeadline)
    expect(noDeadline).toBeGreaterThan(PERMISSION_TTL_GRACE_MS)
  })
})
