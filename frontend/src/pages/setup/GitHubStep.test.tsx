// The wizard's GitHub connect step: the deployment App's Connect, with the
// organization's own App one link away. Pinned: the account field is open from
// the start, the link moves on to the URL step and Back returns, Continue
// refuses until an account is connected, and a connected organization sees
// its accounts with no way off the deployment App here.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { act, render, renderHook, screen, fireEvent, waitFor } from '@testing-library/react'
import { GitHubConnectStep } from './GitHubStep'
import { initialWizardState, WIZARD_STEPS } from './steps'
import { useWizard } from './useWizard'
import type { WizardState } from './types'

const ORG = 'org-1'

const deploymentAppState = (over: Partial<WizardState> = {}): WizardState => ({
  ...initialWizardState(),
  isLocal: false,
  githubAccessTab: 'app',
  githubDeploymentAppAvailable: true,
  ...over,
})

function renderStep(state: WizardState) {
  const patch = vi.fn()
  const advance = vi.fn()
  render(
    <GitHubConnectStep
      state={state}
      patch={patch}
      advance={advance}
      orgId={ORG}
      teamId="default"
      isLocal={false}
    />,
  )
  return { patch, advance }
}

beforeEach(() => vi.restoreAllMocks())

describe('GitHubConnectStep — not connected', () => {
  it('opens on the account field, with no Cancel', () => {
    renderStep(deploymentAppState())
    expect(screen.getByRole('heading', { name: 'Connect GitHub' })).toBeInTheDocument()
    expect(screen.getByRole('textbox')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Connect' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Cancel' })).not.toBeInTheDocument()
  })

  it('records the own-App choice before advancing', () => {
    const { patch, advance } = renderStep(deploymentAppState())
    fireEvent.click(screen.getByRole('button', { name: 'Use your own GitHub App instead' }))
    expect(patch).toHaveBeenCalledWith({ githubOwnApp: true })
    expect(advance).toHaveBeenCalledTimes(1)
    expect(patch.mock.invocationCallOrder[0]).toBeLessThan(advance.mock.invocationCallOrder[0])
  })

  it('still offers Connect to an organization on the deployment App with no account', () => {
    renderStep(deploymentAppState({ githubAppManaged: true }))
    expect(screen.getByRole('heading', { name: 'Connect GitHub' })).toBeInTheDocument()
    expect(screen.getByRole('textbox')).toBeInTheDocument()
  })
})

describe('GitHubConnectStep — connected', () => {
  it('lists the accounts and offers another, with no way off the deployment App', () => {
    renderStep(
      deploymentAppState({
        githubAppManaged: true,
        githubAppInstalled: true,
        githubAppInstallCount: 2,
        githubAppAccounts: ['acme-corp', 'acme-labs'],
      }),
    )
    expect(screen.getByRole('heading', { name: 'GitHub is connected' })).toBeInTheDocument()
    expect(screen.getByText('acme-corp')).toBeInTheDocument()
    expect(screen.getByText('acme-labs')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Connect another account…' })).toBeInTheDocument()
    expect(screen.queryByRole('textbox')).not.toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: 'Use your own GitHub App instead' }),
    ).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /disconnect/i })).not.toBeInTheDocument()
  })
})

describe('the connect step in the wizard', () => {
  // Every load fails, so the wizard runs on the state below alone.
  const makeInitialState = () => deploymentAppState()

  it('refuses Continue until connected, then the link reaches the URL step and Back returns', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('offline')))
    const identity = { orgId: ORG, teamId: 'default', isLocal: false }
    const { result } = renderHook(() =>
      useWizard(WIZARD_STEPS, identity, makeInitialState, () => {}),
    )
    await waitFor(() => expect(result.current.phase).toBe('ready'))
    const connectAt = WIZARD_STEPS.findIndex((s) => s.id === 'org-github-connect')
    expect(result.current.activeIndex).toBe(connectAt)

    act(() => result.current.advance())
    expect(result.current.error).toBe(
      'Connect a GitHub account to continue, or use your own GitHub App.',
    )
    expect(result.current.activeIndex).toBe(connectAt)

    act(() => {
      result.current.patch({ githubOwnApp: true })
      result.current.advance()
    })
    await waitFor(() => expect(WIZARD_STEPS[result.current.activeIndex].id).toBe('org-github-url'))

    act(() => {
      result.current.back()
    })
    expect(result.current.activeIndex).toBe(connectAt)
  })
})
