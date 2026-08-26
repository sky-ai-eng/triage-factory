import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import ModelAvailabilityPanel from './ModelAvailabilityPanel'
import { resetModelCatalogForTest } from '../../hooks/useModelCatalog'
import type { ModelCatalogEntry } from '../../lib/models'
import { LOCAL_DEFAULT_ORG_ID } from '../../lib/githubApp'
import { jsonBody } from '../../test/apiResponse'

const WITH_VERDICTS: ModelCatalogEntry[] = [
  {
    key: 'claude-sonnet-5',
    display_name: 'Claude Sonnet 5',
    provider: 'anthropic',
    provider_display_name: 'Anthropic',
    enabled: true,
    prices_per_mtok: { input: 3, output: 15, cache_read: 0.3, cache_write: 3.75 },
    availability: 'unverified',
  },
  // Not in the org's enable-set, and still listed: availability is org truth
  // about what the credentials can do, and an admin deciding whether to enable
  // a model wants to know it works first.
  {
    key: 'claude-opus-5',
    display_name: 'Claude Opus 5',
    provider: 'anthropic',
    provider_display_name: 'Anthropic',
    enabled: false,
    prices_per_mtok: { input: 5, output: 25, cache_read: 0.5, cache_write: 6.25 },
    availability: 'verified',
  },
]

// The other universe: no TF-owned credential, so no row carries a verdict.
const HARNESS_SETTLED: ModelCatalogEntry[] = [
  { key: 'sonnet', display_name: 'Claude Sonnet', enabled: true },
  { key: 'haiku', display_name: 'Claude Haiku', enabled: true },
]

function stub(items: ModelCatalogEntry[], testResponse?: unknown) {
  const fetchMock = vi.fn((input: unknown) => {
    if (String(input).endsWith('/test')) {
      return Promise.resolve({ ok: true, ...jsonBody(testResponse) })
    }
    return Promise.resolve({ ok: true, ...jsonBody({ items }) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

beforeEach(() => resetModelCatalogForTest())
afterEach(() => vi.unstubAllGlobals())

describe('ModelAvailabilityPanel', () => {
  it('lists every model with a verdict, enabled or not, each with its own test', async () => {
    stub(WITH_VERDICTS)
    render(<ModelAvailabilityPanel orgId={LOCAL_DEFAULT_ORG_ID} />)

    await waitFor(() =>
      expect(screen.getAllByRole('button', { name: /test connection/i })).toHaveLength(2),
    )
    expect(screen.getByText('Claude Opus 5')).toBeVisible()
    expect(screen.getByText(/not enabled/)).toBeVisible()
  })

  it('tests one model and re-reads the row the verdict landed on', async () => {
    const fetchMock = stub(WITH_VERDICTS, {
      model_key: 'claude-sonnet-5',
      outcome: 'verified',
    })
    render(<ModelAvailabilityPanel orgId={LOCAL_DEFAULT_ORG_ID} />)

    const buttons = await screen.findAllByRole('button', { name: /test connection/i })
    await userEvent.click(buttons[0])

    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        `/api/orgs/${LOCAL_DEFAULT_ORG_ID}/models/claude-sonnet-5/test`,
        expect.objectContaining({ method: 'POST' }),
      ),
    )
    // The badge is a stored verdict, so the catalog is re-read rather than
    // patched from the response — two copies of one fact could disagree.
    await waitFor(() =>
      expect(
        fetchMock.mock.calls.filter(([u]) => String(u).endsWith('/models')).length,
      ).toBeGreaterThan(1),
    )
  })

  // The probe surface exists exactly when the org brings its own credential, and
  // the panel works that out from the rows rather than from a mode: a row with
  // no availability field is one no verdict can be about, so there is nothing to
  // test and the panel says why instead of offering a button the route refuses.
  it('offers no test at all where nothing is stored to test against', async () => {
    stub(HARNESS_SETTLED)
    render(<ModelAvailabilityPanel orgId={LOCAL_DEFAULT_ORG_ID} />)

    expect(
      await screen.findByText(/runs on the Claude credentials of the machine hosting it/i),
    ).toBeVisible()
    expect(screen.queryByRole('button', { name: /test connection/i })).toBeNull()
  })
})
