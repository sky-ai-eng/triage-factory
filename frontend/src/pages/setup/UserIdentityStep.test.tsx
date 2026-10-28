// The wizard's GitHub identity step. Pinned: multi mode links an identity
// through Connect alone — no token field to fill and no Continue that binds
// one — while local mode keeps the token paste.
import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { UserIdentityStep } from './UserIdentityStep'
import { initialWizardState, WIZARD_STEPS } from './steps'

const ORG = 'org-1'
const identityStep = WIZARD_STEPS.find((s) => s.id === 'user-github-identity')!

function renderStep(isLocal: boolean, connectAvailable: boolean) {
  const state = {
    ...initialWizardState(),
    isLocal,
    userIdentityHost: 'https://github.com',
    userConnectAvailable: connectAvailable,
  }
  return render(
    <UserIdentityStep
      state={state}
      patch={vi.fn()}
      orgId={ORG}
      teamId="default"
      isLocal={isLocal}
    />,
  )
}

describe('UserIdentityStep — multi mode', () => {
  it('offers Connect and no token field', () => {
    renderStep(false, true)
    expect(screen.getByRole('button', { name: 'Connect GitHub' })).toBeInTheDocument()
    expect(screen.queryByPlaceholderText('ghp_…')).not.toBeInTheDocument()
  })

  it('offers no token field when Connect is not advertised, and says why', () => {
    renderStep(false, false)
    expect(screen.queryByPlaceholderText('ghp_…')).not.toBeInTheDocument()
    expect(screen.getByText(/isn.t connected for this organization yet/)).toBeInTheDocument()
  })

  it('will not finish without a linked identity, even with a token typed', async () => {
    const state = { ...initialWizardState(), isLocal: false, userGitHubPat: 'ghp_typed' }
    expect(identityStep.validate?.(state)).toBe('Connect your GitHub to finish.')
    const fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    await expect(
      identityStep.persist?.({
        state,
        orgId: ORG,
        teamId: 'default',
        isLocal: false,
        patch: vi.fn(),
      }),
    ).rejects.toThrow('Connect your GitHub to finish.')
    expect(fetchMock).not.toHaveBeenCalled()
  })
})

describe('UserIdentityStep — local mode', () => {
  it('keeps the token paste beside Connect', () => {
    renderStep(true, true)
    expect(screen.getByRole('button', { name: 'Connect GitHub' })).toBeInTheDocument()
    expect(screen.getByPlaceholderText('ghp_…')).toBeInTheDocument()
  })

  it('accepts a typed token on Continue', () => {
    const state = { ...initialWizardState(), isLocal: true, userGitHubPat: 'ghp_typed' }
    expect(identityStep.validate?.(state)).toBeNull()
  })
})
