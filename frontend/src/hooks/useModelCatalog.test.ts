import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { useModelCatalog, modelDisplayName, resetModelCatalogForTest } from './useModelCatalog'
import type { ModelCatalogEntry } from '../lib/models'
import { LOCAL_DEFAULT_ORG_ID } from '../lib/githubApp'
import { jsonBody } from '../test/apiResponse'

// The hook holds a module-level cache keyed by org (one fetch per page), so
// each test clears it and stubs fetch with a canned catalog read.

const modelsPath = `/api/orgs/${LOCAL_DEFAULT_ORG_ID}/models`

function stubModels(read: () => ModelCatalogEntry[] | 'fail') {
  const fetchMock = vi.fn((input: unknown) => {
    if (String(input).split('?')[0] === modelsPath) {
      const answer = read()
      if (answer === 'fail') return Promise.resolve({ ok: false, status: 500, ...jsonBody({}) })
      return Promise.resolve({ ok: true, ...jsonBody({ items: answer }) })
    }
    return Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

const catalog: ModelCatalogEntry[] = [
  {
    key: 'claude-haiku-4-5-20251001',
    display_name: 'Claude Haiku 4.5',
    provider: 'anthropic',
    enabled: true,
  },
  { key: 'claude-sonnet-5', display_name: 'Claude Sonnet 5', provider: 'anthropic', enabled: true },
  { key: 'claude-fable-5', display_name: 'Claude Fable 5', provider: 'anthropic', enabled: false },
]

beforeEach(() => {
  resetModelCatalogForTest()
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('useModelCatalog', () => {
  it('offers the enabled models in wire order', async () => {
    stubModels(() => catalog)
    const { result } = renderHook(() => useModelCatalog())

    await waitFor(() => expect(result.current.loaded).toBe(true))
    // The org's enable-set is what a picker may offer — a disabled model is
    // not a choice, so it never reaches the options.
    expect(result.current.models.map((m) => m.key)).toEqual([
      'claude-haiku-4-5-20251001',
      'claude-sonnet-5',
    ])
  })

  it('shares one read across every mounted picker', async () => {
    const fetchMock = stubModels(() => catalog)
    const first = renderHook(() => useModelCatalog())
    const second = renderHook(() => useModelCatalog())

    await waitFor(() => expect(first.result.current.loaded).toBe(true))
    await waitFor(() => expect(second.result.current.loaded).toBe(true))
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it('stays unresolved on a failed read rather than claiming no models exist', async () => {
    stubModels(() => 'fail')
    const { result } = renderHook(() => useModelCatalog())

    await waitFor(() => expect(result.current.models).toEqual([]))
    // Not loaded: an empty catalog would read as "this deployment offers
    // nothing", which is a claim a failed read cannot make.
    expect(result.current.loaded).toBe(false)
  })

  it('names a stored key, and falls back to the key itself', async () => {
    stubModels(() => catalog)
    const { result } = renderHook(() => useModelCatalog())
    await waitFor(() => expect(result.current.loaded).toBe(true))

    expect(modelDisplayName('claude-sonnet-5')).toBe('Claude Sonnet 5')
    // A name is a fact about the build, so a model the org disabled still has
    // one — a summary has to render what is stored either way.
    expect(modelDisplayName('claude-fable-5')).toBe('Claude Fable 5')
    expect(modelDisplayName('claude-opus-4-8')).toBe('claude-opus-4-8')
    // A blank key is the absence of a model, not a model with no name. It stays
    // blank here on purpose — each surface says what "unset" reads as, because
    // they mean different things by it — so callers must never render this
    // result without deciding that first.
    expect(modelDisplayName('')).toBe('')
  })
})
