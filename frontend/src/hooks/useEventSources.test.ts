import { describe, it, expect, vi, beforeEach } from 'vitest'
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

// flush drains the microtask queue past a settled fetch — apiJSON awaits
// `text()` and then parses, so a read's own cleanup runs several ticks after
// its body resolves. A test that asserts before that has not observed anything.
const flush = () => new Promise((resolve) => setTimeout(resolve, 0))

const configured: EventSourceAvailability[] = [
  { kind: 'github', state: 'available', disableable: false },
  { kind: 'jira', state: 'unconfigured', disableable: true },
  { kind: 'linear', state: 'wip', disableable: true },
]

beforeEach(() => {
  resetEventSourcesForTest()
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

  // An invalidation starts a second read while the first is still open. When
  // the first one finally settles it must retire only its OWN in-flight entry
  // — a blind delete would untrack the second, leaving it running while the
  // next consumer starts a third read of the same org.
  it('a superseded read does not untrack the one that replaced it', async () => {
    const opened: Array<(value: EventSourceAvailability[]) => void> = []
    const fetchMock = vi.fn((input: unknown) => {
      if (String(input).split('?')[0] !== sourcesPath) {
        return Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })
      }
      // Park each read open; the test settles them by hand, in order.
      const body = new Promise<{ sources: EventSourceAvailability[] }>((resolve) => {
        opened.push((sources) => resolve({ sources }))
      })
      return Promise.resolve({ ok: true, ...jsonBody(body) })
    })
    vi.stubGlobal('fetch', fetchMock)

    const first = renderHook(() => useEventSources())
    await waitFor(() => expect(opened).toHaveLength(1))

    invalidateEventSources()
    await waitFor(() => expect(opened).toHaveLength(2)) // the replacement read

    // The superseded read settles now, after its replacement was dispatched —
    // and is given time to run its cleanup, which is where the bug would be.
    opened[0](configured)
    await flush()

    // A newly mounted consumer must join the read in flight, not start another.
    const second = renderHook(() => useEventSources())
    await flush()
    expect(fetchMock).toHaveBeenCalledTimes(2)

    opened[1]([{ kind: 'jira', state: 'available', disableable: true }])
    await waitFor(() => expect(first.result.current.stateOf('jira')).toBe('available'))
    expect(second.result.current.stateOf('jira')).toBe('available')
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })

  // Invalidation is per org. Naming one org must not discard another's read
  // that is already in flight — the discarded answer would never reach the
  // cache, and the consumer waiting on it would have to start a second one.
  it('invalidating another org does not discard this org’s open read', async () => {
    const opened: Array<(value: EventSourceAvailability[]) => void> = []
    const fetchMock = vi.fn((input: unknown) => {
      if (String(input).split('?')[0] !== sourcesPath) {
        return Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })
      }
      const body = new Promise<{ sources: EventSourceAvailability[] }>((resolve) => {
        opened.push((sources) => resolve({ sources }))
      })
      return Promise.resolve({ ok: true, ...jsonBody(body) })
    })
    vi.stubGlobal('fetch', fetchMock)

    const { result } = renderHook(() => useEventSources())
    await waitFor(() => expect(opened).toHaveLength(1))

    invalidateEventSources('11111111-1111-1111-1111-111111111111')

    // The open read is this org's and nothing happened to this org, so its
    // answer still counts — and no second read was started to replace it.
    opened[0](configured)
    await waitFor(() => expect(result.current.loaded).toBe(true))
    expect(result.current.canProduce('jira')).toBe(false)
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it('treats an admin-paused source as one no handler can fire on', async () => {
    // `disabled` is the fifth state, and it joins the vocabulary rather than
    // riding alongside as a second boolean precisely so a consumer keyed on
    // `available` cannot forget it.
    stubSources(() => [
      { kind: 'github', state: 'available', disableable: false },
      { kind: 'jira', state: 'disabled', disableable: true },
    ])
    const { result } = renderHook(() => useEventSources())

    await waitFor(() => expect(result.current.loaded).toBe(true))
    expect(result.current.stateOf('jira')).toBe('disabled')
    expect(result.current.canProduce('jira')).toBe(false)
    expect(result.current.canProduce('github')).toBe(true)
  })

  it('keeps the previous answer rendered while a refetch is in flight', async () => {
    // Invalidation marks stale rather than deleting. Deleting takes `loaded`
    // back to false, and every consumer collapses to its loading state — the
    // settings section flashing on its own toggle, the event picker emptying on
    // a websocket ping meant for somebody else's change.
    const opened: Array<(value: EventSourceAvailability[]) => void> = []
    vi.stubGlobal(
      'fetch',
      vi.fn((input: unknown) => {
        if (String(input).split('?')[0] !== sourcesPath) {
          return Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })
        }
        const body = new Promise<{ sources: EventSourceAvailability[] }>((resolve) => {
          opened.push((sources) => resolve({ sources }))
        })
        return Promise.resolve({ ok: true, ...jsonBody(body) })
      }),
    )
    const { result } = renderHook(() => useEventSources())
    await waitFor(() => expect(opened).toHaveLength(1))
    opened[0](configured)
    await waitFor(() => expect(result.current.loaded).toBe(true))

    invalidateEventSources()
    await waitFor(() => expect(opened).toHaveLength(2))

    // Still resolved, still the old answer, while the new read is open.
    expect(result.current.loaded).toBe(true)
    expect(result.current.stateOf('jira')).toBe('unconfigured')

    opened[1]([
      { kind: 'github', state: 'available', disableable: false },
      { kind: 'jira', state: 'available', disableable: true },
    ])
    await waitFor(() => expect(result.current.stateOf('jira')).toBe('available'))
  })

  it('refetches an org invalidated while nothing was mounted', async () => {
    // The stale mark has to survive an unmount, or a component mounting after
    // the invalidation finds a populated cache and renders the old answer with
    // nothing left to correct it.
    let answer = configured
    const fetchMock = stubSources(() => answer)
    const first = renderHook(() => useEventSources())
    await waitFor(() => expect(first.result.current.loaded).toBe(true))
    first.unmount()

    answer = [
      { kind: 'github', state: 'available', disableable: false },
      { kind: 'jira', state: 'available', disableable: true },
    ]
    invalidateEventSources()

    const second = renderHook(() => useEventSources())
    await waitFor(() => expect(second.result.current.stateOf('jira')).toBe('available'))
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })

  it('picks up a source that just became available', async () => {
    let answer = configured
    const fetchMock = stubSources(() => answer)
    const { result } = renderHook(() => useEventSources())

    await waitFor(() => expect(result.current.canProduce('jira')).toBe(false))

    // The credential was just bound — the settings surfaces invalidate.
    answer = [
      { kind: 'github', state: 'available', disableable: false },
      { kind: 'jira', state: 'available', disableable: true },
    ]
    invalidateEventSources()

    await waitFor(() => expect(result.current.canProduce('jira')).toBe(true))
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })
})
