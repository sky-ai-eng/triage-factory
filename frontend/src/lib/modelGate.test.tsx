import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import ModelGateDialog from '../components/ModelGateDialog'
import { resetModelCatalogForTest } from '../hooks/useModelCatalog'
import { gateModelSave, offerModelSweep, resetModelGateForTest } from './modelGate'
import type { ModelCatalogEntry, ModelTestOutcome } from './models'
import { jsonBody } from '../test/apiResponse'

// The gate is exercised end to end — the store, the mounted dialog and the test
// route — because the property under test is "the save happens only on green",
// and that spans all three.

const ORG = '00000000-0000-0000-0000-000000000001'

const UNTESTED: ModelCatalogEntry = {
  key: 'claude-sonnet-5',
  display_name: 'Claude Sonnet 5',
  provider: 'anthropic',
  provider_display_name: 'Anthropic',
  enabled: true,
  prices_per_mtok: { input: 3, output: 15, cache_read: 0.3, cache_write: 3.75 },
  availability: 'unverified',
}

// A test route answering one canned outcome, plus the catalog re-read the gate
// does afterwards (a verdict is stored server-side, so the badge is re-read
// rather than patched locally).
function stubTest(outcome: ModelTestOutcome, detail?: string) {
  const fetchMock = vi.fn((input: unknown) => {
    const url = String(input)
    if (url.endsWith('/test')) {
      return Promise.resolve({
        ok: true,
        ...jsonBody({ model_key: UNTESTED.key, outcome, detail }),
      })
    }
    return Promise.resolve({ ok: true, ...jsonBody({ items: [UNTESTED] }) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

beforeEach(() => {
  resetModelGateForTest()
  resetModelCatalogForTest()
})

describe('the save gate — a selection nothing has verified', () => {
  it('asks first, states what the test costs, and completes the save on green', async () => {
    const fetchMock = stubTest('verified')
    render(<ModelGateDialog />)
    const saved = gateModelSave(ORG, UNTESTED)

    const dialog = await screen.findByRole('alertdialog')
    expect(dialog.textContent).toMatch(/Test Claude Sonnet 5 before saving\?/)
    // The cost is stated in the terms of the transport that will spend it: this
    // row carries prices, so it is dispatched directly and a probe is tiny.
    expect(dialog.textContent).toMatch(/about two tokens/i)
    expect(fetchMock).not.toHaveBeenCalled()

    await userEvent.click(screen.getByRole('button', { name: /test and save/i }))

    await expect(saved).resolves.toBe(true)
    expect(fetchMock).toHaveBeenCalledWith(
      `/api/orgs/${ORG}/models/${UNTESTED.key}/test`,
      expect.objectContaining({ method: 'POST' }),
    )
  })

  // A refusal is a successful test with a negative answer: the save does not
  // happen, and what the provider actually said is what makes it fixable.
  it('refuses the save on a red verdict and shows the provider’s own detail', async () => {
    stubTest('red', 'not entitled in this account')
    render(<ModelGateDialog />)
    const saved = gateModelSave(ORG, UNTESTED)

    await screen.findByRole('alertdialog')
    await userEvent.click(screen.getByRole('button', { name: /test and save/i }))

    // The dialog is a motion surface: its inline style reaches the animate
    // target on the first ANIMATION FRAME — skipAnimations (test setup) skips
    // the tween, not the frame — and a microtask-bound test can assert before
    // jsdom ever fires one, reading the entrance's opacity: 0 as invisible.
    // Visibility on motion content is therefore awaited, never read directly:
    // waitFor yields real macrotasks, which is what lets that frame run.
    await waitFor(() => expect(screen.getByText(/not entitled in this account/)).toBeVisible())
    // Nothing left to press but out: pressing again could only re-learn the
    // same answer.
    expect(screen.queryByRole('button', { name: /test and save|try again/i })).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: /close/i }))
    await expect(saved).resolves.toBe(false)
  })

  // Nobody answered, so nothing was recorded — the state is exactly what it
  // was, and pressing again is the whole remedy. The save stays blocked in the
  // meantime rather than proceeding on an unestablished model.
  it('blocks the save and offers a retry when the test establishes nothing', async () => {
    const fetchMock = stubTest('inconclusive', 'the provider timed out')
    render(<ModelGateDialog />)
    const saved = gateModelSave(ORG, UNTESTED)

    await screen.findByRole('alertdialog')
    await userEvent.click(screen.getByRole('button', { name: /test and save/i }))

    // Awaited for the same animation-frame reason as the red-verdict case.
    await waitFor(() => expect(screen.getByText(/the provider timed out/)).toBeVisible())
    const retry = await screen.findByRole('button', { name: /try again/i })

    await userEvent.click(retry)
    await waitFor(() =>
      expect(fetchMock.mock.calls.filter(([u]) => String(u).endsWith('/test'))).toHaveLength(2),
    )
    // Still blocked: two inconclusive attempts establish exactly as much as one.
    await userEvent.click(screen.getByRole('button', { name: /cancel/i }))
    await expect(saved).resolves.toBe(false)
  })

  it('lets the save through untouched when the model is already verified', async () => {
    const fetchMock = stubTest('verified')
    render(<ModelGateDialog />)

    await expect(gateModelSave(ORG, { ...UNTESTED, availability: 'verified' })).resolves.toBe(true)
    expect(screen.queryByRole('alertdialog')).toBeNull()
    expect(fetchMock).not.toHaveBeenCalled()
  })

  // The presence gate again, from the other side: a row with no availability
  // field has no stored verdict to establish, so there is no gate to engage and
  // nothing is spent.
  it('never engages for a row carrying no availability field', async () => {
    const fetchMock = stubTest('verified')
    render(<ModelGateDialog />)

    const harnessSettled: ModelCatalogEntry = {
      key: 'sonnet',
      display_name: 'Sonnet',
      enabled: true,
    }
    await expect(gateModelSave(ORG, harnessSettled)).resolves.toBe(true)
    expect(screen.queryByRole('alertdialog')).toBeNull()
    expect(fetchMock).not.toHaveBeenCalled()
  })
})

describe('the eager sweep offered after a credential is connected', () => {
  it('says how many requests it is about to make, and spends nothing when declined', async () => {
    const fetchMock = stubTest('verified')
    render(<ModelGateDialog />)
    const swept = offerModelSweep(ORG, 'anthropic', 'Anthropic', [
      UNTESTED,
      { ...UNTESTED, key: 'b' },
    ])

    const dialog = await screen.findByRole('alertdialog')
    expect(dialog.textContent).toMatch(/Test 2 models against Anthropic\?/)
    // Declining has to read as a real option, because it is one — the save gate
    // catches each untested model later.
    expect(dialog.textContent).toMatch(/tested the first time somebody saves it/i)

    await userEvent.click(screen.getByRole('button', { name: /not now/i }))
    await expect(swept).resolves.toBe(false)
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('runs one sweep request for the named provider when accepted', async () => {
    const fetchMock = stubTest('verified')
    render(<ModelGateDialog />)
    const swept = offerModelSweep(ORG, 'anthropic', 'Anthropic', [UNTESTED])

    await screen.findByRole('alertdialog')
    await userEvent.click(screen.getByRole('button', { name: /test 1 model/i }))

    await expect(swept).resolves.toBe(true)
    expect(fetchMock).toHaveBeenCalledWith(
      `/api/orgs/${ORG}/models/tests`,
      expect.objectContaining({ method: 'POST', body: JSON.stringify({ provider: 'anthropic' }) }),
    )
  })

  // Nothing to test is not a question worth asking.
  it('offers nothing when every candidate is already verified', async () => {
    render(<ModelGateDialog />)
    await expect(offerModelSweep(ORG, 'anthropic', 'Anthropic', [])).resolves.toBe(false)
    expect(screen.queryByRole('alertdialog')).toBeNull()
  })
})
