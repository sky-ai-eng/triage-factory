import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import {
  useEventSources,
  invalidateEventSources,
  resetEventSourcesForTest,
  type EventSourceAvailability,
} from './useEventSources'
import { LOCAL_DEFAULT_ORG_ID } from '../lib/githubApp'
import { jsonBody } from '../test/apiResponse'

// The hook holds a module-level cache keyed by org (one fetch per page), so
// each test clears it and stubs fetch with a canned availability read.

const sourcesPath = `/api/orgs/${LOCAL_DEFAULT_ORG_ID}/sources`

function stubSources(read: () => EventSourceAvailability[] | 'fail') {
  const fetchMock = vi.fn((input: unknown) => {
    if (String(input).split('?')[0] === sourcesPath) {
      const answer = read()
      if (answer === 'fail') return Promise.resolve({ ok: false, status: 500, ...jsonBody({}) })
      return Promise.resolve({ ok: true, ...jsonBody({ sources: answer }) })
    }
    return Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

const configured: EventSourceAvailability[] = [
  { kind: 'github', state: 'available' },
  { kind: 'jira', state: 'unconfigured' },
  { kind: 'linear', state: 'wip' },
]

beforeEach(() => {
  resetEventSourcesForTest()
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('useEventSources', () => {
  it('reports each source’s state once the read resolves', async () => {
    stubSources(() => configured)
    const { result } = renderHook(() => useEventSources())

    await waitFor(() => expect(result.current.loaded).toBe(true))
    expect(result.current.stateOf('github')).toBe('available')
    expect(result.current.stateOf('jira')).toBe('unconfigured')
    expect(result.current.stateOf('linear')).toBe('wip')
    expect(result.current.canProduce('github')).toBe(true)
    expect(result.current.canProduce('jira')).toBe(false)
    expect(result.current.canProduce('linear')).toBe(false)
  })

  // The gate hides event types and marks handlers inert. Doing that on an
  // answer we do not have yet would empty the picker on every mount and flash
  // every row as broken, so an unresolved read makes no claim.
  it('claims nothing before the read resolves', () => {
    stubSources(() => configured)
    const { result } = renderHook(() => useEventSources())

    expect(result.current.loaded).toBe(false)
    expect(result.current.sources).toEqual([])
    expect(result.current.stateOf('jira')).toBeUndefined()
    expect(result.current.canProduce('jira')).toBe(true)
  })

  // A source the deployment does not carry — slack in local mode, which omits
  // it rather than reporting it off — is outside the vocabulary, and the hook
  // must not answer for it.
  it('makes no claim about a source the read did not list', async () => {
    stubSources(() => configured)
    const { result } = renderHook(() => useEventSources())

    await waitFor(() => expect(result.current.loaded).toBe(true))
    expect(result.current.stateOf('slack')).toBeUndefined()
    expect(result.current.canProduce('slack')).toBe(true)
    expect(result.current.canProduce('system')).toBe(true)
  })

  // A failed read must not cache an empty vocabulary: that would read as "no
  // source is available" and mark everything inert.
  it('stays unresolved on a failed read rather than caching nothing', async () => {
    stubSources(() => 'fail')
    const { result } = renderHook(() => useEventSources())

    await waitFor(() => expect(fetch).toHaveBeenCalled())
    expect(result.current.loaded).toBe(false)
    expect(result.current.canProduce('jira')).toBe(true)
  })

  it('reads once across multiple consumers', async () => {
    const fetchMock = stubSources(() => configured)
    const a = renderHook(() => useEventSources())
    const b = renderHook(() => useEventSources())

    await waitFor(() => expect(a.result.current.loaded).toBe(true))
    await waitFor(() => expect(b.result.current.loaded).toBe(true))
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it('picks up a source that just became available', async () => {
    let answer = configured
    const fetchMock = stubSources(() => answer)
    const { result } = renderHook(() => useEventSources())

    await waitFor(() => expect(result.current.canProduce('jira')).toBe(false))

    // The credential was just bound — the settings surfaces invalidate.
    answer = [
      { kind: 'github', state: 'available' },
      { kind: 'jira', state: 'available' },
    ]
    invalidateEventSources()

    await waitFor(() => expect(result.current.canProduce('jira')).toBe(true))
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })
})
