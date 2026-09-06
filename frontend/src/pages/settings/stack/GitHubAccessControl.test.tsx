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
  refreshGitHubAppInstallations: vi.fn(),
  startManagedGitHubConnect: vi.fn(),
  startManagedGitHubConnectAccount: vi.fn(),
  disconnectManagedGitHub: vi.fn(),
  disconnectManagedInstallation: vi.fn(),
  disconnectOwnApp: vi.fn(),
  getGitHubAppStatus: vi.fn(),
}))
vi.mock('../../../lib/githubApp', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../../lib/githubApp')>()
  return {
    ...actual,
    patPreflight: ghMocks.patPreflight,
    replayGitHubWebhookDeliveries: ghMocks.replayGitHubWebhookDeliveries,
    refreshGitHubAppInstallations: ghMocks.refreshGitHubAppInstallations,
    startManagedGitHubConnect: ghMocks.startManagedGitHubConnect,
    startManagedGitHubConnectAccount: ghMocks.startManagedGitHubConnectAccount,
    disconnectManagedGitHub: ghMocks.disconnectManagedGitHub,
    disconnectManagedInstallation: ghMocks.disconnectManagedInstallation,
    disconnectOwnApp: ghMocks.disconnectOwnApp,
    getGitHubAppStatus: ghMocks.getGitHubAppStatus,
  }
})

const credMocks = vi.hoisted(() => ({ connectGitHubPAT: vi.fn() }))
vi.mock('../orgCredentials', () => ({ connectGitHubPAT: credMocks.connectGitHubPAT }))

// The two grant findings ride usePagedList, which reads through apiList; the
// mock answers by path so a test can seed each finding, and records every
// call so a test can assert which reads opening the panel issued.
const listMocks = vi.hoisted(() => ({
  pages: {} as Record<string, { items: unknown[]; total_count: number; next_page_token?: string }>,
  calls: [] as string[],
}))
vi.mock('../../../lib/apiClient', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../../lib/apiClient')>()
  return {
    ...actual,
    apiList: async (path: string) => {
      listMocks.calls.push(path)
      const page = listMocks.pages[path] ?? { items: [], total_count: 0 }
      return {
        items: page.items,
        next_page_token: page.next_page_token ?? '',
        total_count: page.total_count,
      }
    },
  }
})

// The App-status display fetch the component mounts; jsdom has no server to
// answer it, so the status is supplied directly. null (the PAT org's view) is
// the default; the webhook-health cases below set one. Stateful, because the
// component folds a post-disconnect re-read into the hook and re-renders off
// it.
const installMocks = vi.hoisted(() => ({ status: null as GitHubAppStatus | null }))
vi.mock('../../../hooks/useGitHubAppInstall', async () => {
  const { useState } = await import('react')
  return {
    useGitHubAppInstall: () => {
      const [status, setStatus] = useState<GitHubAppStatus | null>(installMocks.status)
      return { status, setStatus, installUrl: '' }
    },
  }
})

import GitHubAccessControl from './GitHubAccessControl'
import {
  reachWithoutPurposeListPath,
  scopeDriftListPath,
  type GitHubAppInstallation,
  type GitHubAppStatus,
  type GitHubAppWebhookHealth,
} from '../../../lib/githubApp'
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

// A status payload with every field the panel reads, for the fixtures below.
function statusOf(over: Partial<GitHubAppStatus>): GitHubAppStatus {
  return {
    app: null,
    installations: [],
    using_deployment_default: false,
    deployment_app_available: false,
    webhook_health: null,
    connect_callback_url: '',
    ...over,
  }
}

function installation(over: Partial<GitHubAppInstallation>): GitHubAppInstallation {
  return {
    installation_id: '4242',
    account_type: 'Organization',
    account_login: 'acme',
    installed_at: '2026-01-02T03:04:05Z',
    suspended_at: '',
    suspended_by: '',
    repository_selection: 'selected',
    settings_url: 'https://github.com/organizations/acme/settings/installations/4242',
    ...over,
  }
}

beforeEach(() => {
  ghMocks.patPreflight.mockReset()
  ghMocks.replayGitHubWebhookDeliveries.mockReset()
  ghMocks.refreshGitHubAppInstallations.mockReset()
  ghMocks.startManagedGitHubConnect.mockReset()
  ghMocks.startManagedGitHubConnectAccount.mockReset()
  ghMocks.startManagedGitHubConnectAccount.mockResolvedValue(undefined)
  ghMocks.disconnectManagedGitHub.mockReset()
  ghMocks.disconnectManagedInstallation.mockReset()
  ghMocks.disconnectOwnApp.mockReset()
  ghMocks.getGitHubAppStatus.mockReset()
  credMocks.connectGitHubPAT.mockReset()
  installMocks.status = null
  listMocks.pages = {}
  listMocks.calls = []
  vi.spyOn(window, 'confirm').mockReturnValue(true)
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
    installMocks.status = statusOf({ webhook_health: health })
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
  const managedStatus = (installations: GitHubAppStatus['installations']): GitHubAppStatus =>
    statusOf({ installations, using_deployment_default: true, deployment_app_available: true })
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
    installMocks.status = managedStatus([installation({})])
    renderControl({ hasGitHubPat: false, githubPatLogin: '', githubAppManaged: true })
    expect(
      screen.getByText(/installed on 1 account connected to this workspace/i),
    ).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Connect another account…' }))
    expect(ghMocks.startManagedGitHubConnect).toHaveBeenCalledWith('org-1')
    expect(screen.queryByRole('button', { name: /personal access token/i })).not.toBeInTheDocument()
  })

  // The managed panel: the App it rides is the deployment's, so it shows no
  // slug, offers no registration and no import, and its only destructive
  // affordances are the two disconnect verbs.
  it('offers Disconnect and a per-account unbind, and neither registration nor import', () => {
    installMocks.status = managedStatus([
      installation({}),
      installation({ installation_id: '4343', account_login: 'beta', account_type: 'User' }),
    ])
    renderControl({ hasGitHubPat: false, githubPatLogin: '', githubAppManaged: true })

    expect(
      screen.getByRole('button', { name: /Disconnect from the deployment’s App/ }),
    ).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Disconnect @acme' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Disconnect @beta' })).toBeInTheDocument()
    // The grant is edited on GitHub, never here.
    expect(screen.getAllByRole('link', { name: /Manage on GitHub/ })).toHaveLength(2)
    expect(screen.getByText(/chosen on GitHub’s installation page, never here/)).toBeInTheDocument()

    expect(screen.queryByRole('button', { name: /Register a GitHub App/ })).not.toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: /Connect an existing App/ }),
    ).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Set up a GitHub App/ })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Switch to GitHub App/ })).not.toBeInTheDocument()
    expect(screen.queryByText(/registered App/)).not.toBeInTheDocument()
  })

  it('calls the narrowed verb for one account and reloads the findings against what remains', async () => {
    installMocks.status = managedStatus([
      installation({}),
      installation({ installation_id: '4343', account_login: 'beta' }),
    ])
    ghMocks.disconnectManagedInstallation.mockResolvedValue(undefined)
    ghMocks.getGitHubAppStatus.mockResolvedValue(
      managedStatus([installation({ installation_id: '4343', account_login: 'beta' })]),
    )
    const { patch, reload } = renderControl({
      hasGitHubPat: false,
      githubPatLogin: '',
      githubAppManaged: true,
    })

    fireEvent.click(screen.getByRole('button', { name: 'Disconnect @acme' }))
    await waitFor(() => {
      expect(ghMocks.disconnectManagedInstallation).toHaveBeenCalledWith('org-1', '4242')
    })
    expect(ghMocks.disconnectManagedGitHub).not.toHaveBeenCalled()
    await waitFor(() => expect(reload).toHaveBeenCalled())
    expect(patch).toHaveBeenCalledWith({
      githubAppManaged: true,
      githubAppInstalled: true,
      githubAppInstallCount: 1,
    })
    expect(screen.queryByRole('button', { name: 'Disconnect @acme' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Disconnect @beta' })).toBeInTheDocument()
  })

  it('calls the org-level verb for Disconnect, and renders the no-credential state after', async () => {
    installMocks.status = managedStatus([installation({})])
    ghMocks.disconnectManagedGitHub.mockResolvedValue(undefined)
    // What the server answers once the workspace has left the class: the
    // rowless default, with the deployment App still on offer.
    ghMocks.getGitHubAppStatus.mockResolvedValue(statusOf({ deployment_app_available: true }))
    const { patch } = renderControl({
      hasGitHubPat: false,
      githubPatLogin: '',
      githubAppManaged: true,
    })

    fireEvent.click(screen.getByRole('button', { name: /Disconnect from the deployment’s App/ }))
    await waitFor(() => {
      expect(ghMocks.disconnectManagedGitHub).toHaveBeenCalledWith('org-1')
    })
    expect(ghMocks.disconnectManagedInstallation).not.toHaveBeenCalled()
    expect(patch).toHaveBeenCalledWith({
      githubAppManaged: false,
      githubAppInstalled: false,
      githubAppInstallCount: 0,
    })
    // The PAT-class empty state, with all four ways in.
    expect(await screen.findByText(/isn’t configured for this workspace yet/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Connect GitHub…' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Register a GitHub App…' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Connect an existing App…' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Use a personal access token…' })).toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: /Disconnect from the deployment’s App/ }),
    ).not.toBeInTheDocument()
  })

  it('unbinding the last account is the full disconnect, and lands on the same empty state', async () => {
    installMocks.status = managedStatus([installation({})])
    ghMocks.disconnectManagedInstallation.mockResolvedValue(undefined)
    ghMocks.getGitHubAppStatus.mockResolvedValue(statusOf({ deployment_app_available: true }))
    renderControl({ hasGitHubPat: false, githubPatLogin: '', githubAppManaged: true })

    fireEvent.click(screen.getByRole('button', { name: 'Disconnect @acme' }))
    await waitFor(() => {
      expect(ghMocks.disconnectManagedInstallation).toHaveBeenCalledWith('org-1', '4242')
    })
    expect(await screen.findByRole('button', { name: 'Connect GitHub…' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Use a personal access token…' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Disconnect @/ })).not.toBeInTheDocument()
  })

  it('leaves the bindings alone when the confirm is declined', () => {
    vi.spyOn(window, 'confirm').mockReturnValue(false)
    installMocks.status = managedStatus([installation({})])
    renderControl({ hasGitHubPat: false, githubPatLogin: '', githubAppManaged: true })
    fireEvent.click(screen.getByRole('button', { name: 'Disconnect @acme' }))
    fireEvent.click(screen.getByRole('button', { name: /Disconnect from the deployment’s App/ }))
    expect(ghMocks.disconnectManagedInstallation).not.toHaveBeenCalled()
    expect(ghMocks.disconnectManagedGitHub).not.toHaveBeenCalled()
  })
})

// The no-credential empty state. The deployment App is a default, never a
// mandate: registering your own App, importing one, and binding a token are
// offered beside Connect, never behind it — and without a deployment App the
// same three stand alone.
describe('GitHubAccessControl · nothing bound', () => {
  const none = { hasGitHubPat: false, githubPatLogin: '' }

  it('offers Connect, register, import and a token together when the deployment has an App', () => {
    installMocks.status = statusOf({ deployment_app_available: true })
    renderControl(none)
    expect(screen.getByRole('button', { name: 'Connect GitHub…' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Register a GitHub App…' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Connect an existing App…' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Use a personal access token…' })).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'Connect GitHub…' }))
    expect(ghMocks.startManagedGitHubConnect).toHaveBeenCalledWith('org-1')
  })

  it('offers register, import and a token — and no Connect — without a deployment App', () => {
    installMocks.status = statusOf({ deployment_app_available: false })
    renderControl(none)
    expect(screen.queryByRole('button', { name: 'Connect GitHub…' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Register a GitHub App…' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Connect an existing App…' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Use a personal access token…' })).toBeInTheDocument()
  })

  it('binds a first token through the same validate-then-commit screens as a rotation', async () => {
    installMocks.status = statusOf({})
    ghMocks.patPreflight.mockResolvedValue({
      login: 'acme-bot',
      tracked: 0,
      reachable: 0,
      dark_repos: [],
    })
    credMocks.connectGitHubPAT.mockResolvedValue({ ok: true, login: 'acme-bot' })
    const { reload } = renderControl(none)

    fireEvent.click(screen.getByRole('button', { name: 'Use a personal access token…' }))
    expect(screen.getByText('Connect with a personal access token')).toBeInTheDocument()
    fireEvent.change(screen.getByPlaceholderText('ghp_…'), { target: { value: 'ghp_first' } })
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }))
    fireEvent.click(await screen.findByRole('button', { name: 'Connect' }))
    await waitFor(() => {
      expect(credMocks.connectGitHubPAT).toHaveBeenCalledWith('org-1', 'ghp_first')
    })
    await waitFor(() => expect(reload).toHaveBeenCalled())
  })

  it('routes register and import straight to their screens', () => {
    installMocks.status = statusOf({})
    // The import form fetches the status for its callback URL on mount.
    ghMocks.getGitHubAppStatus.mockResolvedValue(statusOf({}))
    const { patch } = renderControl(none)
    fireEvent.click(screen.getByRole('button', { name: 'Connect an existing App…' }))
    expect(patch).toHaveBeenCalledWith({ githubAppSource: 'existing' })
    expect(screen.getByText('Connect an existing App')).toBeInTheDocument()
  })
})

// The findings and the installation cards, per credential class. What is
// pinned: the two App classes see both findings with an actionable verb, a
// token workspace sees neither and no copy implying it holds a grant, an empty
// finding says so, opening the panel issues no refresh, and the three grant
// widths get three different sentences.
// Leaving the workspace's own App for the deployment's. One affordance: the
// teardown verb followed straight away by Connect, offered only where Connect
// is. Deliberately no bare disconnect — a teardown with no replacement lands
// an admin in the setup wizard, and the only reason to want one is to connect
// something else.
describe('GitHubAccessControl · switching the own App to the deployment App', () => {
  const liveApp = {
    githubAppRegistered: true,
    githubAppStaged: false,
    githubAppSlug: 'acme-bot',
    hasGitHubPat: false,
    githubPatLogin: '',
  }
  const torndown = {
    status: 'disconnected',
    github_app_deleted_locally: true,
    github_app_settings_url: 'https://github.com/settings/apps',
  }

  it('offers the switch beside the token switch, and no bare disconnect', () => {
    installMocks.status = statusOf({
      installations: [installation({})],
      deployment_app_available: true,
    })
    renderControl(liveApp)

    expect(
      screen.getByRole('button', { name: 'Switch to the deployment’s App…' }),
    ).toBeInTheDocument()
    expect(
      screen.getByRole('button', { name: 'Switch to a personal access token…' }),
    ).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^Disconnect/ })).not.toBeInTheDocument()
  })

  it('offers no switch where the deployment has no App', () => {
    installMocks.status = statusOf({ installations: [installation({})] })
    renderControl(liveApp)

    expect(
      screen.queryByRole('button', { name: 'Switch to the deployment’s App…' }),
    ).not.toBeInTheDocument()
    expect(
      screen.getByRole('button', { name: 'Switch to a personal access token…' }),
    ).toBeInTheDocument()
  })

  it('tears the own App down and then goes straight into Connect', async () => {
    installMocks.status = statusOf({
      installations: [installation({})],
      deployment_app_available: true,
    })
    ghMocks.disconnectOwnApp.mockResolvedValue(torndown)
    const { patch } = renderControl(liveApp)

    fireEvent.click(screen.getByRole('button', { name: 'Switch to the deployment’s App…' }))
    await waitFor(() => {
      expect(ghMocks.startManagedGitHubConnect).toHaveBeenCalledWith('org-1')
    })
    expect(ghMocks.disconnectOwnApp).toHaveBeenCalledWith('org-1')
    // The draft no longer claims an App, so a return from GitHub's page
    // without a bind renders the empty state.
    expect(patch).toHaveBeenCalledWith({
      githubAppRegistered: false,
      githubAppStaged: false,
      githubAppInstalled: false,
      githubAppInstallCount: 0,
      githubAppSlug: '',
      hasGitHubPat: false,
      githubPatLogin: '',
    })
  })

  it('surfaces a refused teardown inline and never navigates', async () => {
    installMocks.status = statusOf({
      installations: [installation({})],
      deployment_app_available: true,
    })
    ghMocks.disconnectOwnApp.mockRejectedValue(new Error('this GitHub App is staged, not live'))
    const { patch } = renderControl(liveApp)

    fireEvent.click(screen.getByRole('button', { name: 'Switch to the deployment’s App…' }))
    expect(await screen.findByText('this GitHub App is staged, not live')).toBeInTheDocument()
    expect(ghMocks.startManagedGitHubConnect).not.toHaveBeenCalled()
    expect(patch).not.toHaveBeenCalled()
    expect(
      screen.getByRole('button', { name: 'Switch to the deployment’s App…' }),
    ).toBeInTheDocument()
  })

  it('tears nothing down when the confirm is declined', () => {
    installMocks.status = statusOf({
      installations: [installation({})],
      deployment_app_available: true,
    })
    vi.spyOn(window, 'confirm').mockReturnValue(false)
    renderControl(liveApp)

    fireEvent.click(screen.getByRole('button', { name: 'Switch to the deployment’s App…' }))
    expect(ghMocks.disconnectOwnApp).not.toHaveBeenCalled()
    expect(ghMocks.startManagedGitHubConnect).not.toHaveBeenCalled()
  })
})

// The named-account way in: for an account that already has the deployment
// App, which GitHub's install page would offer nothing but Configure. Offered
// wherever Connect is, as a reveal; it names an account and never lists one.
describe('GitHubAccessControl · connecting an account that already has the App', () => {
  const reveal = () =>
    fireEvent.click(
      screen.getByRole('button', { name: 'Connect an account that already has the App…' }),
    )

  it('is offered beside Connect in the empty state where the deployment has an App', () => {
    installMocks.status = statusOf({ deployment_app_available: true })
    renderControl({ hasGitHubPat: false, githubPatLogin: '' })
    expect(
      screen.getByRole('button', { name: 'Connect an account that already has the App…' }),
    ).toBeInTheDocument()
  })

  it('is not offered where the deployment has no App', () => {
    installMocks.status = statusOf({})
    renderControl({ hasGitHubPat: false, githubPatLogin: '' })
    expect(
      screen.queryByRole('button', { name: 'Connect an account that already has the App…' }),
    ).not.toBeInTheDocument()
  })

  it('is offered on the managed panel beside Connect another account', () => {
    installMocks.status = statusOf({
      installations: [installation({})],
      using_deployment_default: true,
      deployment_app_available: true,
    })
    renderControl({ hasGitHubPat: false, githubPatLogin: '', githubAppManaged: true })
    expect(screen.getByRole('button', { name: 'Connect another account…' })).toBeInTheDocument()
    expect(
      screen.getByRole('button', { name: 'Connect an account that already has the App…' }),
    ).toBeInTheDocument()
  })

  it('names the account and starts the OAuth leg for it, listing nothing', async () => {
    installMocks.status = statusOf({ deployment_app_available: true })
    renderControl({ hasGitHubPat: false, githubPatLogin: '' })
    reveal()
    expect(screen.queryByRole('list')).not.toBeInTheDocument()
    fireEvent.change(screen.getByRole('textbox'), { target: { value: 'acme-corp' } })
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }))
    await waitFor(() => {
      expect(ghMocks.startManagedGitHubConnectAccount).toHaveBeenCalledWith('org-1', 'acme-corp')
    })
    expect(ghMocks.startManagedGitHubConnect).not.toHaveBeenCalled()
  })

  it('does not submit an empty name', () => {
    installMocks.status = statusOf({ deployment_app_available: true })
    renderControl({ hasGitHubPat: false, githubPatLogin: '' })
    reveal()
    expect(screen.getByRole('button', { name: 'Connect' })).toBeDisabled()
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }))
    expect(ghMocks.startManagedGitHubConnectAccount).not.toHaveBeenCalled()
  })

  it('surfaces a refused start inline and keeps the form', async () => {
    installMocks.status = statusOf({ deployment_app_available: true })
    ghMocks.startManagedGitHubConnectAccount.mockRejectedValue(
      new Error("Couldn't connect acme-corp."),
    )
    renderControl({ hasGitHubPat: false, githubPatLogin: '' })
    reveal()
    fireEvent.change(screen.getByRole('textbox'), { target: { value: 'acme-corp' } })
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }))
    expect(await screen.findByText("Couldn't connect acme-corp.")).toBeInTheDocument()
    expect(screen.getByRole('textbox')).toBeInTheDocument()
  })
})

describe('GitHubAccessControl · grant findings', () => {
  const liveApp = {
    githubAppRegistered: true,
    githubAppStaged: false,
    githubAppSlug: 'acme-bot',
    hasGitHubPat: false,
    githubPatLogin: '',
  }
  const REACH = reachWithoutPurposeListPath('org-1')
  const DRIFT = scopeDriftListPath('org-1')

  function seedFindings() {
    listMocks.pages[REACH] = {
      total_count: 1,
      items: [
        {
          installation_id: '4242',
          account_login: 'acme',
          settings_url: 'https://github.com/organizations/acme/settings/installations/4242',
          owner: 'acme',
          repo: 'secrets',
          slug: 'acme/secrets',
          private: true,
          html_url: 'https://github.com/acme/secrets',
          observed_at: '2026-08-15T12:00:00Z',
        },
      ],
    }
    listMocks.pages[DRIFT] = {
      total_count: 2,
      items: [
        {
          owner: 'acme',
          repo: 'legacy',
          slug: 'acme/legacy',
          installation_id: '4242',
          account_login: 'acme',
          settings_url: 'https://github.com/organizations/acme/settings/installations/4242',
        },
        {
          owner: 'stranger',
          repo: 'tool',
          slug: 'stranger/tool',
          installation_id: '',
          account_login: '',
          settings_url: '',
        },
      ],
    }
  }

  it('renders both findings with their verbs for a workspace with its own App', async () => {
    seedFindings()
    installMocks.status = statusOf({ installations: [installation({})] })
    renderControl(liveApp)

    expect(await screen.findByText('acme/secrets')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /Narrow the grant/ })).toHaveAttribute(
      'href',
      'https://github.com/organizations/acme/settings/installations/4242',
    )
    expect(screen.getByText('acme/legacy')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /Add it on GitHub/ })).toBeInTheDocument()
    // No installation covers stranger: the verb is to connect the account or
    // untrack, not a grant to edit.
    expect(screen.getByText('stranger/tool')).toBeInTheDocument()
    expect(screen.getByText(/no connected account owns @stranger/)).toBeInTheDocument()
    // Its own App: no managed verbs.
    expect(
      screen.queryByRole('button', { name: /Disconnect from the deployment’s App/ }),
    ).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Disconnect @/ })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Connect GitHub…' })).not.toBeInTheDocument()
  })

  it('renders both findings for a workspace on the deployment App', async () => {
    seedFindings()
    installMocks.status = statusOf({
      installations: [installation({})],
      using_deployment_default: true,
      deployment_app_available: true,
    })
    renderControl({ hasGitHubPat: false, githubPatLogin: '', githubAppManaged: true })
    expect(await screen.findByText('acme/secrets')).toBeInTheDocument()
    expect(screen.getByText('acme/legacy')).toBeInTheDocument()
  })

  it('shows a token workspace neither finding and no grant copy', () => {
    seedFindings()
    installMocks.status = statusOf({})
    renderControl() // a PAT org
    expect(listMocks.calls).toEqual([])
    expect(screen.queryByText(/Reachable but untracked/)).not.toBeInTheDocument()
    expect(screen.queryByText(/outside the grant/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/grant/i)).not.toBeInTheDocument()
  })

  it('says nothing to address when a finding is empty, never a blank', async () => {
    installMocks.status = statusOf({ installations: [installation({})] })
    renderControl(liveApp)
    expect(
      await screen.findByText(/Nothing to address — every repository the App reaches is tracked/),
    ).toBeInTheDocument()
    expect(
      screen.getByText(/Nothing to address — every tracked repository is inside the grant/),
    ).toBeInTheDocument()
  })

  it('opens without refreshing the installation mirror', async () => {
    installMocks.status = statusOf({ installations: [installation({})] })
    renderControl(liveApp)
    await screen.findByText(/Nothing to address — every tracked repository/)
    expect(ghMocks.refreshGitHubAppInstallations).not.toHaveBeenCalled()
    expect(listMocks.calls.sort()).toEqual([REACH, DRIFT].sort())
  })

  it('gives the three grant widths three different sentences', async () => {
    listMocks.pages[DRIFT] = {
      total_count: 1,
      items: [
        {
          owner: 'some',
          repo: 'legacy',
          slug: 'some/legacy',
          installation_id: '2',
          account_login: 'some',
          settings_url: 'https://github.com/organizations/some/settings/installations/2',
        },
      ],
    }
    installMocks.status = statusOf({
      installations: [
        installation({ installation_id: '1', account_login: 'every', repository_selection: 'all' }),
        installation({
          installation_id: '2',
          account_login: 'some',
          repository_selection: 'selected',
        }),
        installation({
          installation_id: '3',
          account_login: 'unknown',
          repository_selection: null,
        }),
      ],
    })
    renderControl(liveApp)
    await screen.findByText('some/legacy')

    const cards = screen.getAllByRole('listitem').filter((li) => li.textContent?.startsWith('@'))
    const every = cards.find((li) => li.textContent?.includes('@every'))
    const some = cards.find((li) => li.textContent?.includes('@some'))
    const unknown = cards.find((li) => li.textContent?.includes('@unknown'))
    expect(every?.textContent).toMatch(/can never fall outside the grant/)
    expect(some?.textContent).toMatch(/1 tracked repository is outside the grant/)
    expect(unknown?.textContent).toMatch(/isn’t known yet/)
    expect(unknown?.textContent).not.toMatch(/outside the grant|never fall/)
  })

  // "Nothing tracked is outside the grant" is a claim about the whole finding.
  // With a further page unloaded, a card whose account has no drift on the
  // loaded page defers to the list, and one that has some reads as a floor.
  it('never claims nothing is outside a grant off a partial drift page', async () => {
    listMocks.pages[DRIFT] = {
      total_count: 60,
      next_page_token: 'more',
      items: [
        {
          owner: 'some',
          repo: 'legacy',
          slug: 'some/legacy',
          installation_id: '2',
          account_login: 'some',
          settings_url: 'https://github.com/organizations/some/settings/installations/2',
        },
      ],
    }
    installMocks.status = statusOf({
      installations: [
        installation({
          installation_id: '1',
          account_login: 'quiet',
          repository_selection: 'selected',
        }),
        installation({
          installation_id: '2',
          account_login: 'some',
          repository_selection: 'selected',
        }),
      ],
    })
    renderControl(liveApp)
    await screen.findByText('some/legacy')

    const cards = screen.getAllByRole('listitem').filter((li) => li.textContent?.startsWith('@'))
    const quiet = cards.find((li) => li.textContent?.includes('@quiet'))
    const some = cards.find((li) => li.textContent?.includes('@some'))
    expect(quiet?.textContent).not.toMatch(/nothing tracked is outside/)
    expect(quiet?.textContent).toMatch(/list below names any that are/)
    expect(some?.textContent).toMatch(/at least 1 tracked repository is outside the grant/)
    expect(screen.getByRole('button', { name: 'Show more' })).toBeInTheDocument()
  })

  it('renders a suspended installation in its own state', () => {
    installMocks.status = statusOf({
      installations: [
        installation({ suspended_at: '2026-08-15T12:00:00Z', suspended_by: 'octocat' }),
        installation({ installation_id: '4343', account_login: 'beta' }),
      ],
    })
    renderControl(liveApp)
    const cards = screen.getAllByRole('listitem').filter((li) => li.textContent?.startsWith('@'))
    const acme = cards.find((li) => li.textContent?.includes('@acme'))
    const beta = cards.find((li) => li.textContent?.includes('@beta'))
    expect(acme).toHaveAttribute('data-suspended', 'true')
    expect(acme?.textContent).toMatch(/Suspended on GitHub by @octocat/)
    expect(acme?.textContent).toMatch(/refuses every token/)
    expect(beta).not.toHaveAttribute('data-suspended')
    expect(beta?.textContent).not.toMatch(/Suspended/)
  })
})
