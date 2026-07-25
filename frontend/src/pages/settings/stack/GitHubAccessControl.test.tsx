// The in-place PAT rotation (TFAC-677). What's pinned here is the property the
// ticket exists for: an org already on a token can replace it from Settings —
// no mode switch, no danger-zone clear — and a token that doesn't validate stops
// the flow before anything is stored, so the credential the org is running on
// survives a failed attempt.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'

const ghMocks = vi.hoisted(() => ({ patPreflight: vi.fn() }))
vi.mock('../../../lib/githubApp', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../../lib/githubApp')>()
  return { ...actual, patPreflight: ghMocks.patPreflight }
})

const credMocks = vi.hoisted(() => ({ connectGitHubPAT: vi.fn() }))
vi.mock('../orgCredentials', () => ({ connectGitHubPAT: credMocks.connectGitHubPAT }))

// The App-status display fetch the component mounts; irrelevant to a PAT org and
// jsdom has no server to answer it.
vi.mock('../../../hooks/useGitHubAppInstall', () => ({
  useGitHubAppInstall: () => ({ status: null, setStatus: () => {}, installUrl: '' }),
}))

import GitHubAccessControl from './GitHubAccessControl'
import { initialWizardState } from '../../setup/steps'
import type { StepContext, WizardState } from '../../setup/types'

const BASE_URL = 'https://github.example.com'

function renderControl(over: Partial<WizardState> = {}) {
  const patch = vi.fn()
  const reload = vi.fn()
  const state: WizardState = {
    ...initialWizardState(),
    hasGitHubPat: true,
    githubPatLogin: 'acme-bot',
    ...over,
  }
  const ctx: StepContext = {
    orgId: 'org-1',
    teamId: 'default',
    isLocal: true,
    state,
    patch,
    advance: () => {},
  }
  render(<GitHubAccessControl ctx={ctx} baseUrl={BASE_URL} reload={reload} />)
  return { patch, reload }
}

beforeEach(() => {
  ghMocks.patPreflight.mockReset()
  credMocks.connectGitHubPAT.mockReset()
})

describe('GitHubAccessControl · PAT rotation', () => {
  it('names the bound account and offers a replacement without a mode switch', () => {
    renderControl()
    expect(screen.getByText('@acme-bot')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Replace token…' })).toBeInTheDocument()
  })

  it('offers no replacement when nothing is bound', () => {
    renderControl({ hasGitHubPat: false, githubPatLogin: '' })
    expect(screen.queryByRole('button', { name: 'Replace token…' })).not.toBeInTheDocument()
  })

  // The env overlay wins on read: a token written from here would be stored and
  // then ignored, and the recorded login belongs to the shadowed credential. So
  // the section reports the connection and offers nothing to change it.
  it('reports an env-supplied token as settled, with no replacement or login', () => {
    renderControl({ githubPatEnvProvided: true, githubPatLogin: '' })
    expect(screen.getByText(/TRIAGE_FACTORY_GITHUB_BOT_PAT/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Replace token…' })).not.toBeInTheDocument()
    expect(screen.queryByText('@acme-bot')).not.toBeInTheDocument()
  })

  it('validates, shows the reach, then binds the new token against the saved host', async () => {
    ghMocks.patPreflight.mockResolvedValue({
      login: 'acme-bot-2',
      tracked: 2,
      reachable: 2,
      dark_repos: [],
    })
    credMocks.connectGitHubPAT.mockResolvedValue({ ok: true, login: 'acme-bot-2' })
    const { patch, reload } = renderControl()

    fireEvent.click(screen.getByRole('button', { name: 'Replace token…' }))
    fireEvent.change(screen.getByPlaceholderText('ghp_…'), { target: { value: 'ghp_new' } })
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }))

    // Preflight stores nothing — the diff is shown before the swap.
    await screen.findByText('@acme-bot-2')
    expect(credMocks.connectGitHubPAT).not.toHaveBeenCalled()

    fireEvent.click(screen.getByRole('button', { name: 'Replace token' }))
    await waitFor(() => {
      expect(credMocks.connectGitHubPAT).toHaveBeenCalledWith('org-1', BASE_URL, 'ghp_new')
    })
    await waitFor(() => expect(reload).toHaveBeenCalled())
    expect(patch).toHaveBeenCalledWith({ hasGitHubPat: true, githubPatLogin: 'acme-bot-2' })
  })

  it('requires acknowledging repositories the replacement can no longer reach', async () => {
    ghMocks.patPreflight.mockResolvedValue({
      login: 'acme-bot-2',
      tracked: 2,
      reachable: 1,
      dark_repos: [{ repo: 'acme/api', teams: ['Platform'] }],
    })
    renderControl()

    fireEvent.click(screen.getByRole('button', { name: 'Replace token…' }))
    fireEvent.change(screen.getByPlaceholderText('ghp_…'), { target: { value: 'ghp_narrow' } })
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }))

    const confirm = await screen.findByRole('button', { name: 'Replace token' })
    expect(screen.getByText('acme/api')).toBeInTheDocument()
    expect(confirm).toBeDisabled()

    fireEvent.click(screen.getByRole('checkbox'))
    expect(screen.getByRole('button', { name: 'Replace token' })).toBeEnabled()
  })

  it('surfaces a rejected token inline and binds nothing', async () => {
    ghMocks.patPreflight.mockRejectedValue(new Error('That token could not be validated.'))
    renderControl()

    fireEvent.click(screen.getByRole('button', { name: 'Replace token…' }))
    fireEvent.change(screen.getByPlaceholderText('ghp_…'), { target: { value: 'ghp_bad' } })
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }))

    expect(await screen.findByText('That token could not be validated.')).toBeInTheDocument()
    expect(credMocks.connectGitHubPAT).not.toHaveBeenCalled()
    // Still on the token screen, with the existing credential untouched.
    expect(screen.getByPlaceholderText('ghp_…')).toBeInTheDocument()
  })

  it('keeps the org on its current token when the bind itself is rejected', async () => {
    ghMocks.patPreflight.mockResolvedValue({
      login: 'acme-bot-2',
      tracked: 0,
      reachable: 0,
      dark_repos: [],
    })
    credMocks.connectGitHubPAT.mockResolvedValue({ ok: false, error: 'GitHub: bad credentials' })
    const { patch, reload } = renderControl()

    fireEvent.click(screen.getByRole('button', { name: 'Replace token…' }))
    fireEvent.change(screen.getByPlaceholderText('ghp_…'), { target: { value: 'ghp_revoked' } })
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }))
    fireEvent.click(await screen.findByRole('button', { name: 'Replace token' }))

    expect(await screen.findByText('GitHub: bad credentials')).toBeInTheDocument()
    expect(patch).not.toHaveBeenCalled()
    expect(reload).not.toHaveBeenCalled()
  })
})
