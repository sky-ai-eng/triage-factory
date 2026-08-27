import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { OrgBackgroundJobsModelStep } from './ModelStep'
import { initialWizardState, WIZARD_STEPS } from './steps'
import type { WizardState } from './types'
import { resetModelCatalogForTest } from '../../hooks/useModelCatalog'
import type { ModelCatalogEntry } from '../../lib/models'
import { LOCAL_DEFAULT_ORG_ID } from '../../lib/githubApp'
import { jsonBody } from '../../test/apiResponse'

const catalog: ModelCatalogEntry[] = [
  {
    key: 'claude-haiku-4-5-20251001',
    display_name: 'Claude Haiku 4.5',
    provider: 'anthropic',
    enabled: true,
  },
  { key: 'claude-opus-5', display_name: 'Claude Opus 5', provider: 'anthropic', enabled: true },
]

function stubCatalog() {
  vi.stubGlobal(
    'fetch',
    vi.fn((input: unknown) => {
      if (String(input).split('?')[0] === `/api/orgs/${LOCAL_DEFAULT_ORG_ID}/models`) {
        return Promise.resolve({ ok: true, ...jsonBody({ items: catalog }) })
      }
      return Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })
    }),
  )
}

// The step body's context. Only org state and the patcher matter here; the rest
// of StepContext is shape.
function ctx(backgroundJobsModel: string, patch: (p: Partial<WizardState>) => void) {
  const base = initialWizardState()
  return {
    state: { ...base, org: { ...base.org, background_jobs_model: backgroundJobsModel } },
    patch,
    orgId: LOCAL_DEFAULT_ORG_ID,
    teamId: 'default',
    isLocal: true,
    advance: () => {},
  }
}

beforeEach(() => {
  resetModelCatalogForTest()
  stubCatalog()
})

describe('OrgBackgroundJobsModelStep — turning the jobs off', () => {
  // The backend accepts null on this field to stop the three background jobs,
  // and Settings is the only surface meant to send it — unbinding the Claude
  // credential every other feature shares is not an alternative. A picker with
  // no way out would make that a documented capability reachable only by API.
  it('offers an off switch in Settings, which clears the stored model', async () => {
    const patch = vi.fn()
    render(<OrgBackgroundJobsModelStep {...ctx('claude-opus-5', patch)} allowOff />)

    const off = await screen.findByRole('button', { name: /turn background jobs off/i })
    await userEvent.click(off)

    expect(patch).toHaveBeenCalledWith(
      expect.objectContaining({ org: expect.objectContaining({ background_jobs_model: '' }) }),
    )
  })

  // Already off, the switch would be a no-op that reads as a live control. The
  // state says so instead, and naming the way back is the whole point: nothing
  // else in the product will tell you those jobs are idle by choice.
  it('states the off state rather than re-offering the switch', async () => {
    render(<OrgBackgroundJobsModelStep {...ctx('', vi.fn())} allowOff />)

    await waitFor(() => expect(screen.getByText(/background jobs are off/i)).toBeInTheDocument())
    expect(screen.queryByRole('button', { name: /turn background jobs off/i })).toBeNull()
  })

  // The wizard step is mandatory, so it must not carry the way out: an org that
  // could skip it during setup is an org whose scorer, classifier and profiler
  // never run, with nothing having said so.
  it('withholds the off switch from the setup wizard', async () => {
    render(<OrgBackgroundJobsModelStep {...ctx('claude-opus-5', vi.fn())} />)

    await screen.findByRole('radiogroup', { name: /background jobs model/i })
    expect(screen.queryByRole('button', { name: /turn background jobs off/i })).toBeNull()
  })

  // ...and the step's own gate is what makes that mandatory rather than merely
  // unskippable-looking.
  it('blocks the wizard until a model is chosen', () => {
    const step = WIZARD_STEPS.find((s) => s.id === 'org-background-jobs-model')
    if (!step) throw new Error('the background-jobs model step is not registered')
    const base = initialWizardState()
    const withModel = { ...base, org: { ...base.org, background_jobs_model: 'claude-opus-5' } }
    const without = { ...base, org: { ...base.org, background_jobs_model: '' } }
    expect(step.isComplete(without)).toBe(false)
    expect(step.isComplete(withModel)).toBe(true)
  })
})
