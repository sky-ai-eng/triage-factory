import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import ModelPicker from './ModelPicker'
import type { ModelCatalogEntry } from '../lib/models'

// One payload covering every shape the models read can answer with, because the
// whole contract is that a client renders WHAT IS PRESENT: three availability
// states, a row with no availability field at all, and a row with no prices.
const CATALOG: ModelCatalogEntry[] = [
  {
    key: 'verified-model',
    display_name: 'Verified Model',
    provider: 'anthropic',
    provider_display_name: 'Anthropic',
    enabled: true,
    prices_per_mtok: { input: 3, output: 15, cache_read: 0.3, cache_write: 3.75 },
    context_window: 200000,
    supports_prompt_caching: true,
    availability: 'verified',
  },
  {
    key: 'untested-model',
    display_name: 'Untested Model',
    provider: 'anthropic',
    provider_display_name: 'Anthropic',
    enabled: true,
    prices_per_mtok: { input: 1, output: 5, cache_read: 0.1, cache_write: 1.25 },
    // TF knows this one cannot cache, which is a fact worth a warning — not the
    // same thing as a row that makes no claim.
    supports_prompt_caching: false,
    availability: 'unverified',
  },
  {
    key: 'refused-model',
    display_name: 'Refused Model',
    provider: 'bedrock',
    provider_display_name: 'Amazon Bedrock',
    enabled: true,
    prices_per_mtok: { input: 2, output: 8, cache_read: 0.2, cache_write: 2.5 },
    availability: 'red',
    availability_detail: 'not entitled in this account',
  },
  // The harness-settled row: no provider, no prices, and no availability field,
  // because there is no TF-owned credential for a verdict to be about.
  { key: 'sonnet', display_name: 'Claude Sonnet', enabled: true },
]

function renderPicker(value = '', onChange = vi.fn()) {
  render(
    <ModelPicker
      value={value}
      onChange={onChange}
      models={CATALOG}
      loaded
      ariaLabel="Team default model"
    />,
  )
  return onChange
}

describe('ModelPicker — one row per model, rendering what the read carries', () => {
  it('names every model, with its provider and headline output rate where it has them', () => {
    renderPicker()

    expect(screen.getByRole('radio', { name: /Verified Model/ })).toBeInTheDocument()
    expect(screen.getByText(/Anthropic · \$15\.00\/Mtok out/)).toBeInTheDocument()
  })

  // Absent is not zero and not "free": the harness settles what this costs, so
  // the row says nothing about it rather than publishing a number nothing backs.
  it('shows no price line for a row whose cost the harness settles', () => {
    renderPicker()

    const row = screen.getByRole('radio', { name: /Claude Sonnet/ })
    expect(row.textContent).not.toMatch(/Mtok/)
  })

  it('badges each availability state it was given', () => {
    renderPicker()

    expect(screen.getByRole('radio', { name: /Verified Model/ }).textContent).toContain('Verified')
    expect(screen.getByRole('radio', { name: /Untested Model/ }).textContent).toContain('Untested')
    expect(screen.getByRole('radio', { name: /Refused Model/ }).textContent).toContain(
      'Unavailable',
    )
  })

  // The presence gate, and the reason the surface needs no mode branch: a row
  // with no availability field is one no stored verdict can describe, so it
  // carries no badge rather than an "unknown" one.
  it('renders no badge at all for a row carrying no availability field', () => {
    renderPicker()

    const row = screen.getByRole('radio', { name: /Claude Sonnet/ })
    for (const word of ['Verified', 'Untested', 'Unavailable', 'Not connected']) {
      expect(row.textContent).not.toContain(word)
    }
  })

  // A refused model stays listed — hiding it would leave somebody hunting for a
  // model they were shown yesterday — but it cannot be chosen, and the row says
  // what the provider actually refused so the fix is nameable.
  it('lists a refused model without letting it be chosen, and shows the detail', async () => {
    const onChange = renderPicker()

    const row = screen.getByRole('radio', { name: /Refused Model/ })
    expect(row).toBeDisabled()
    expect(screen.getByText(/not entitled in this account/)).toBeInTheDocument()

    await userEvent.click(row)
    expect(onChange).not.toHaveBeenCalled()
  })

  it('warns on a model that cannot cache its prompt', () => {
    renderPicker()

    const row = screen.getByRole('radio', { name: /Untested Model/ })
    expect(row.textContent).toMatch(/cost several times more without prompt caching/i)
  })

  // ...and says nothing of the kind about a row that makes no claim either way.
  it('does not warn about caching on a row that asserts nothing about it', () => {
    renderPicker()

    const row = screen.getByRole('radio', { name: /Claude Sonnet/ })
    expect(row.textContent).not.toMatch(/prompt caching/i)
  })

  it('records the chosen key', async () => {
    const onChange = renderPicker()

    await userEvent.click(screen.getByRole('radio', { name: /Untested Model/ }))
    expect(onChange).toHaveBeenCalledWith('untested-model')
  })

  // The empty state names the fix rather than the state. An org that has
  // connected nothing sees every model as unreachable, and a list of rows that
  // all refuse says only that something is wrong.
  it('sends an org with no credential to connect one, instead of listing refusals', () => {
    render(
      <ModelPicker
        value=""
        onChange={vi.fn()}
        models={CATALOG.map((m) => ({ ...m, availability: 'unconfigured' as const }))}
        loaded
        ariaLabel="Team default model"
      />,
    )

    expect(screen.getByText(/Connect a provider in Settings → Claude credentials/)).toBeVisible()
    expect(screen.queryByRole('radiogroup')).toBeNull()
  })

  // "Not yet" and "none" are different answers, and only one of them is worth
  // telling somebody: an unresolved read renders nothing rather than claiming
  // this deployment offers no models.
  it('renders nothing while the catalog read is outstanding', () => {
    const { container } = render(
      <ModelPicker
        value=""
        onChange={vi.fn()}
        models={[]}
        loaded={false}
        ariaLabel="Team default model"
      />,
    )

    expect(container).toBeEmptyDOMElement()
  })

  // Unset is a real choice only where it means something — a prompt inherits
  // its team's default — and it is never badged, because it names no model.
  it('offers the unset row where a surface has one, unbadged', async () => {
    const onChange = vi.fn()
    render(
      <ModelPicker
        value="verified-model"
        onChange={onChange}
        models={CATALOG}
        loaded
        ariaLabel="Prompt model"
        unsetOption={{ label: 'Default', detail: 'Whatever this team runs on.' }}
      />,
    )

    const unset = screen.getByRole('radio', { name: /Default/ })
    expect(unset.textContent).not.toContain('Untested')
    await userEvent.click(unset)
    expect(onChange).toHaveBeenCalledWith('')
  })
})
