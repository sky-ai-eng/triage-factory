// The in-place PAT rotation (TFAC-677). What's pinned here is the property the
// ticket exists for: an org already on a token can replace it from Settings —
// no mode switch, no danger-zone clear — and a token that doesn't validate stops
// the flow before anything is stored, so the credential the org is running on
// survives a failed attempt.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'

const ghMocks = vi.hoisted(() => ({
  patPreflight: vi.fn(),
  replayGitHubWebhookDeliveries: vi.fn(),
  startManagedGitHubConnect: vi.fn(),
}))
vi.mock('../../../lib/githubApp', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../../lib/githubApp')>()
  return {
    ...actual,
    patPreflight: ghMocks.patPreflight,
    replayGitHubWebhookDeliveries: ghMocks.replayGitHubWebhookDeliveries,
    startManagedGitHubConnect: ghMocks.startManagedGitHubConnect,
  }
})

const credMocks = vi.hoisted(() => ({ connectGitHubPAT: vi.fn() }))
vi.mock('../orgCredentials', () => ({ connectGitHubPAT: credMocks.connectGitHubPAT }))

// The App-status display fetch the component mounts; jsdom has no server to
// answer it, so the status is supplied directly. null (the PAT org's view) is
// the default; the webhook-health cases below set one.
const installMocks = vi.hoisted(() => ({ status: null as GitHubAppStatus | null }))
vi.mock('../../../hooks/useGitHubAppInstall', () => ({
  useGitHubAppInstall: () => ({
    status: installMocks.status,
    setStatus: () => {},
    installUrl: '',
  }),
}))

import GitHubAccessControl from './GitHubAccessControl'
import type { GitHubAppStatus, GitHubAppWebhookHealth } from '../../../lib/githubApp'
import { initialWizardState } from '../../setup/steps'
import type { StepContext, WizardState } from '../../setup/types'

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
  render(<GitHubAccessControl ctx={ctx} reload={reload} />)
  return { patch, reload }
}

beforeEach(() => {
  ghMocks.patPreflight.mockReset()
  ghMocks.replayGitHubWebhookDeliveries.mockReset()
  ghMocks.startManagedGitHubConnect.mockReset()
  credMocks.connectGitHubPAT.mockReset()
  installMocks.status = null
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
      expect(credMocks.connectGitHubPAT).toHaveBeenCalledWith('org-1', 'ghp_new')
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

// The webhook-health line on a live-App org. What's pinned is the
// property the ticket exists for: a registered App that receives no deliveries
// is no longer indistinguishable from a working one — and the states that are
// NORMAL (a local install's unconfigured hook, an unprobed App) never render as
// a fault.
describe('GitHubAccessControl · App webhook health', () => {
  const liveApp = {
    githubAppRegistered: true,
    githubAppStaged: false,
    githubAppSlug: 'acme-bot',
    hasGitHubPat: false,
    githubPatLogin: '',
  }

  function withHealth(health: GitHubAppWebhookHealth | null) {
    installMocks.status = {
      app: null,
      installations: [],
      using_deployment_default: false,
      connect_callback_url: '',
      webhook_health: health,
    } satisfies GitHubAppStatus
  }

  const rejected: GitHubAppWebhookHealth = {
    state: 'delivering_rejected',
    hook_host: 'https://tf.example.org',
    secret_configured: true,
    last_delivery_at: '2026-08-15T12:00:00Z',
    last_delivery_status_code: 401,
    checked_at: '2026-08-15T12:01:00Z',
  }

  it('says GitHub is delivering and TF is refusing, and offers the replay', async () => {
    ghMocks.replayGitHubWebhookDeliveries.mockResolvedValue({
      candidates: 3,
      replayed: 3,
      failed: 0,
    })
    withHealth(rejected)
    renderControl(liveApp)

    expect(screen.getByText(/Triage Factory is rejecting them/i)).toBeInTheDocument()
    expect(screen.getByText(/webhook secret is missing here/i)).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: /Replay missed installation events/i }))
    await waitFor(() => {
      expect(ghMocks.replayGitHubWebhookDeliveries).toHaveBeenCalledWith('org-1')
    })
  })

  it('names the other host when the App delivers somewhere else', () => {
    withHealth({
      ...rejected,
      state: 'pointing_elsewhere',
      hook_host: 'https://staging.example.com',
    })
    renderControl(liveApp)

    expect(screen.getByText(/staging\.example\.com/)).toBeInTheDocument()
    // Nothing to replay: the deliveries never came here to be refused.
    expect(
      screen.queryByRole('button', { name: /Replay missed installation events/i }),
    ).not.toBeInTheDocument()
  })

  it('reports an unconfigured hook in local mode as normal, not as a fault', () => {
    withHealth({
      state: 'not_configured',
      hook_host: '',
      secret_configured: false,
      last_delivery_at: '',
      last_delivery_status_code: 0,
      checked_at: '2026-08-15T12:01:00Z',
    })
    renderControl(liveApp) // renderControl runs with isLocal: true

    expect(screen.getByText(/Webhooks aren’t configured for this App/i)).toBeInTheDocument()
    expect(screen.getByText(/Normal for a local install/i)).toBeInTheDocument()
  })

  it('says nothing at all before the probe has an answer', () => {
    withHealth(null)
    renderControl(liveApp)

    expect(screen.queryByText(/Webhooks/i)).not.toBeInTheDocument()
  })
})

// A workspace on the deployment's GitHub App (the managed class). It has no App
// of its own and never will, so the register/import affordances are wrong for
// it; what it has is bound installations, and zero of them is an ordinary
// state the section has to render as one — with the same Connect button that
// binds a first account, and never as an error or a blank.
describe('GitHubAccessControl · deployment App', () => {
  const managedStatus = (installations: GitHubAppStatus['installations']): GitHubAppStatus => ({
    app: null,
    installations,
    using_deployment_default: true,
    webhook_health: null,
    connect_callback_url: '',
  })
  const FAULT_WORDS = /\b(error|fail|failed|failure|wrong|problem)\b/i

  it('renders the empty state with a Connect button when nothing is bound', () => {
    installMocks.status = managedStatus([])
    renderControl({ hasGitHubPat: false, githubPatLogin: '', githubAppManaged: true })

    expect(
      screen.getByText(/no GitHub account is connected to this workspace yet/i),
    ).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Connect GitHub…' }))
    expect(ghMocks.startManagedGitHubConnect).toHaveBeenCalledWith('org-1')

    // Not the PAT/none idle screen, and not a fault.
    expect(screen.queryByRole('button', { name: /set up a GitHub App/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /switch to GitHub App/i })).not.toBeInTheDocument()
    expect(screen.queryByText(/isn.t configured/i)).not.toBeInTheDocument()
    expect(document.body.textContent).not.toMatch(FAULT_WORDS)
  })

  // The loader's seed alone is enough — the live status read has not answered
  // yet — so the first paint is already the managed view rather than a PAT
  // screen that flips.
  it('renders the empty state from the seeded flag before the status read lands', () => {
    installMocks.status = null
    renderControl({
      hasGitHubPat: false,
      githubPatLogin: '',
      githubAppManaged: true,
      githubAppInstallCount: 0,
    })
    expect(screen.getByRole('button', { name: 'Connect GitHub…' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /set up a GitHub App/i })).not.toBeInTheDocument()
  })

  it('names only the accounts this workspace has bound, and offers to add another', () => {
    installMocks.status = managedStatus([
      {
        installation_id: '4242',
        account_type: 'Organization',
        account_login: 'acme',
        installed_at: '2026-01-02T03:04:05Z',
        suspended_at: '',
        suspended_by: '',
      },
    ])
    renderControl({ hasGitHubPat: false, githubPatLogin: '', githubAppManaged: true })
    expect(
      screen.getByText(/installed on 1 account connected to this workspace/i),
    ).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Connect another account…' }))
    expect(ghMocks.startManagedGitHubConnect).toHaveBeenCalledWith('org-1')
    expect(screen.queryByRole('button', { name: /personal access token/i })).not.toBeInTheDocument()
  })
})
