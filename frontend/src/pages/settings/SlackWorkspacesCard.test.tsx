// The Slack card reads the org's connections and the deployment's Slack app,
// groups the connections by workspace, offers the connect and switch actions
// to an admin, reads what the install ceremony reported on the URL, and runs
// the switch preview.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, fireEvent, waitFor, within } from '@testing-library/react'

const toasts = vi.hoisted(() => ({
  toast: { success: vi.fn(), error: vi.fn(), failed: vi.fn(), info: vi.fn(), warning: vi.fn() },
}))
vi.mock('../../components/Toast/toastStore', () => toasts)

import SlackWorkspacesCard from './SlackWorkspacesCard'
import type { SlackActivationPreview, SlackConnection } from './slackConnect'
import { jsonBody } from '../../test/apiResponse'

const ORG = 'org-2'
const LIST = `/api/orgs/${ORG}/slack/connections/list`
const DEPLOYMENT_APP = '/api/slack/deployment-app'
const PREVIEW = `/api/orgs/${ORG}/slack/connections/T1/A9/activation-preview`
const ACTIVATE = `/api/orgs/${ORG}/slack/connections/T1/A9/activate`

function conn(over: Partial<SlackConnection> = {}): SlackConnection {
  return {
    workspace_id: 'T1',
    api_app_id: 'A1',
    source: 'org',
    active: true,
    workspace_name: 'Acme',
    app_name: 'Triage',
    transport: 'events_api',
    bot_user_id: 'U1',
    created_at: '2026-11-01T00:00:00Z',
    updated_at: '2026-11-01T00:00:00Z',
    ...over,
  }
}

const ownApp = conn()
const stagedDeployment = conn({
  api_app_id: 'A9',
  source: 'deployment',
  active: false,
  app_name: 'Triage Factory',
  transport: null,
  bot_user_id: 'U9',
})

function preview(over: Partial<SlackActivationPreview> = {}): SlackActivationPreview {
  return {
    from: ownApp,
    to: stagedDeployment,
    channels: [
      { channel_id: 'C1', name: 'eng', status: 'member' },
      { channel_id: 'C2', name: 'ops', status: 'member' },
      { channel_id: 'C3', name: 'general', status: 'will_join' },
      { channel_id: 'C4', name: 'secret', status: 'needs_invite' },
      { channel_id: 'C5', name: 'far', status: 'unknown' },
    ],
    blockers: [],
    notes: [{ code: 'history_rate_limit', message: 'Slack limits conversations.history.' }],
    fingerprint: 'fp-1',
    ...over,
  }
}

type Answer = { status?: number; body?: unknown }
type Route = Answer | ((init?: RequestInit) => Answer)

const notFound: Answer = {
  status: 404,
  body: { errors: [{ reason: 'NOT_FOUND', message: 'not found' }] },
}

// stubApi answers each `METHOD path` from routes. The list answers
// `connections` until a DELETE removes one, and the deployment app is
// absent unless given.
function stubApi({
  connections = [],
  deploymentApp = false,
  routes = {},
}: {
  connections?: SlackConnection[]
  deploymentApp?: boolean
  routes?: Record<string, Route>
}) {
  let rows = [...connections]
  const fetchMock = vi.fn((input: unknown, init?: RequestInit) => {
    const path = String(input)
    const method = init?.method ?? 'GET'
    let answer: Answer
    if (method === 'POST' && path === LIST) {
      answer = { body: { items: rows, next_page_token: '', total_count: rows.length } }
    } else if (method === 'GET' && path === DEPLOYMENT_APP) {
      answer = deploymentApp ? { body: { app_id: 'A9', transport: 'events_api' } } : notFound
    } else if (method === 'DELETE') {
      rows = rows.filter((c) => !path.endsWith(`/${c.workspace_id}/${c.api_app_id}`))
      answer = { body: { status: 'deleted' } }
    } else {
      const route = routes[`${method} ${path}`]
      answer = route === undefined ? notFound : typeof route === 'function' ? route(init) : route
    }
    const status = answer.status ?? 200
    return Promise.resolve({ ok: status < 300, status, ...jsonBody(answer.body) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

function renderCard(canManage = true) {
  return render(<SlackWorkspacesCard orgId={ORG} canManage={canManage} />)
}

beforeEach(() => {
  window.history.replaceState(null, '', '/org?tab=settings')
  toasts.toast.success.mockClear()
})

afterEach(() => {
  vi.restoreAllMocks()
  window.history.replaceState(null, '', '/')
})

describe('SlackWorkspacesCard connecting', () => {
  it('offers Add to Slack when the deployment has an app, with the own-app form behind a link', async () => {
    stubApi({ deploymentApp: true })
    renderCard()

    const add = await screen.findByRole('link', { name: 'Add to Slack' })
    expect(add).toHaveAttribute(
      'href',
      `/api/orgs/${ORG}/slack/install/start?return_to=${encodeURIComponent('/org?tab=settings')}`,
    )

    const instead = screen.getByRole('button', { name: 'Use your own Slack app instead' })
    expect(instead).toHaveAttribute('aria-expanded', 'false')
    expect(screen.getByPlaceholderText('xoxb-…')).not.toBeVisible()

    fireEvent.click(instead)
    expect(instead).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getByPlaceholderText('xoxb-…')).toBeVisible()
  })

  it('makes the own-app form the primary action when the deployment has no app', async () => {
    stubApi({})
    renderCard()

    expect(await screen.findByPlaceholderText('xoxb-…')).toBeVisible()
    expect(screen.getByRole('button', { name: 'Connect workspace' })).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'Add to Slack' })).not.toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: 'Use your own Slack app instead' }),
    ).not.toBeInTheDocument()
  })

  it('refuses the expiring bot token of an app with token rotation on before calling', async () => {
    const fetchMock = stubApi({})
    renderCard()

    const botToken = await screen.findByPlaceholderText('xoxb-…')
    const connect = screen.getByRole('button', { name: 'Connect workspace' })

    fireEvent.change(botToken, { target: { value: 'xoxe.xoxb-1-rotating' } })
    fireEvent.click(connect)
    expect(await screen.findByRole('alert')).toHaveTextContent('token rotation')

    // An app configuration token also starts "xoxe." but is not a bot token.
    fireEvent.change(botToken, { target: { value: 'xoxe.xoxp-1-config' } })
    fireEvent.click(connect)
    expect(await screen.findByRole('alert')).toHaveTextContent("doesn't look like a bot token")

    expect(fetchMock).not.toHaveBeenCalledWith(
      `/api/orgs/${ORG}/slack/connections/import`,
      expect.anything(),
    )
  })

  it('opens the preview for an app the import staged beside the active one', async () => {
    stubApi({
      connections: [ownApp],
      routes: {
        [`POST /api/orgs/${ORG}/slack/connections/import`]: {
          body: { ...stagedDeployment, source: 'org', transport: 'socket', app_name: 'Mine' },
        },
        [`GET ${PREVIEW}`]: { body: preview() },
      },
    })
    renderCard()

    fireEvent.click(await screen.findByRole('button', { name: 'Add another workspace' }))
    fireEvent.change(screen.getByPlaceholderText('xoxb-…'), { target: { value: 'xoxb-1' } })
    fireEvent.click(screen.getByRole('button', { name: 'Connect workspace' }))

    expect(await screen.findByRole('dialog', { name: 'Switch apps' })).toBeInTheDocument()
  })
})

describe('SlackWorkspacesCard rows', () => {
  it('shows a workspace’s connections as one row with a staged line', async () => {
    stubApi({ connections: [ownApp, stagedDeployment] })
    renderCard()

    await screen.findByText('Acme')
    const rows = screen.getAllByRole('listitem')
    expect(rows).toHaveLength(1)
    expect(within(rows[0]).getByText('Staged:')).toBeInTheDocument()
    expect(within(rows[0]).getByText('Triage Factory')).toBeInTheDocument()
    expect(
      within(rows[0]).getByRole('button', { name: 'Review switch to Triage Factory' }),
    ).toBeInTheDocument()
  })

  it('labels the source, an own app’s transport, and an unnamed app by its id', async () => {
    stubApi({
      connections: [
        ownApp,
        conn({
          workspace_id: 'T2',
          workspace_name: 'Beta',
          api_app_id: 'A2',
          source: 'deployment',
          app_name: '',
          transport: null,
        }),
      ],
    })
    renderCard()

    await screen.findByText('Acme')
    const [acme, beta] = screen.getAllByRole('listitem')
    expect(within(acme).getByText('Triage')).toBeInTheDocument()
    expect(within(acme).getByText('Your app')).toBeInTheDocument()
    expect(within(acme).getByText('Events API')).toBeInTheDocument()

    expect(within(beta).getByText('A2')).toBeInTheDocument()
    expect(within(beta).getByText('Deployment app')).toBeInTheDocument()
    expect(within(beta).queryByText('Events API')).not.toBeInTheDocument()
    expect(within(beta).queryByText('Socket Mode')).not.toBeInTheDocument()
  })

  it('offers the switch that fits each workspace’s active app', async () => {
    stubApi({
      deploymentApp: true,
      connections: [
        ownApp,
        conn({
          workspace_id: 'T2',
          workspace_name: 'Beta',
          api_app_id: 'A9',
          source: 'deployment',
          transport: null,
        }),
      ],
    })
    renderCard()

    await screen.findByText('Acme')
    const [acme, beta] = screen.getAllByRole('listitem')
    expect(
      within(acme).getByRole('link', { name: 'Switch to the deployment’s app' }),
    ).toHaveAttribute(
      'href',
      `/api/orgs/${ORG}/slack/install/start?return_to=${encodeURIComponent('/org?tab=settings')}&workspace_id=T1`,
    )
    expect(within(acme).queryByRole('button', { name: 'Switch to your own app' })).toBeNull()

    expect(within(beta).queryByRole('link', { name: 'Switch to the deployment’s app' })).toBeNull()
    fireEvent.click(within(beta).getByRole('button', { name: 'Switch to your own app' }))
    expect(screen.getByPlaceholderText('xoxb-…')).toBeVisible()
  })

  it('offers no switch to the deployment’s app when the deployment has none', async () => {
    stubApi({ connections: [ownApp] })
    renderCard()

    await screen.findByText('Acme')
    expect(screen.queryByRole('link', { name: 'Switch to the deployment’s app' })).toBeNull()
  })

  it('offers Activate for a workspace left with only an inactive connection', async () => {
    stubApi({
      connections: [stagedDeployment],
      routes: { [`GET ${PREVIEW}`]: { body: preview({ from: null }) } },
    })
    renderCard()

    await screen.findByText('Inactive')
    fireEvent.click(screen.getByRole('button', { name: 'Activate' }))
    const dialog = await screen.findByRole('dialog', { name: 'Activate' })
    await within(dialog).findByText('To')
    expect(within(dialog).queryByText('From')).not.toBeInTheDocument()
  })

  it('disconnects a connection under the org’s path', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    const fetchMock = stubApi({ connections: [ownApp] })
    renderCard()

    fireEvent.click(await screen.findByRole('button', { name: 'Disconnect Triage from Acme' }))

    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        `/api/orgs/${ORG}/slack/connections/T1/A1`,
        expect.objectContaining({ method: 'DELETE' }),
      ),
    )
    await waitFor(() => expect(screen.queryByText('Acme')).not.toBeInTheDocument())
  })

  it('says a deployment app stays installed in Slack when disconnected', async () => {
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
    stubApi({ connections: [ownApp, stagedDeployment] })
    renderCard()

    fireEvent.click(
      await screen.findByRole('button', { name: 'Disconnect Triage Factory from Acme' }),
    )
    expect(confirm).toHaveBeenCalledWith(
      expect.stringContaining('The app stays installed in Slack until someone removes it there.'),
    )
  })
})

describe('SlackWorkspacesCard returning from Slack', () => {
  it.each([
    ['state', 'That install link expired. Start again.'],
    ['sso_required', "Sign in with your organization's SSO to connect Slack."],
    ['denied', 'The install was cancelled in Slack.'],
    ['no_app', 'This deployment has no Slack app configured.'],
    [
      'install_failed',
      'Slack did not complete the install. Try again, or ask your operator to check the logs.',
    ],
    [
      'enterprise_install',
      'Install into a single workspace. Organization-wide installs are not supported.',
    ],
    [
      'workspace_taken',
      "Another organization on this deployment has connected that workspace through the deployment's app. Connect your own Slack app instead.",
    ],
    [
      'something_new',
      'Slack did not complete the install. Try again, or ask your operator to check the logs.',
    ],
  ])('shows slack_error=%s and drops it from the URL', async (code, text) => {
    window.history.replaceState(null, '', `/org?tab=settings&slack_error=${code}`)
    stubApi({ deploymentApp: true })
    renderCard()

    expect(screen.getByRole('alert')).toHaveTextContent(text)
    expect(window.location.search).toBe('?tab=settings')
    await screen.findByRole('link', { name: 'Add to Slack' })
  })

  it('opens the own-app form for a workspace another org holds', async () => {
    window.history.replaceState(null, '', '/org?tab=settings&slack_error=workspace_taken')
    stubApi({ deploymentApp: true })
    renderCard()

    await screen.findByRole('link', { name: 'Add to Slack' })
    expect(screen.getByPlaceholderText('xoxb-…')).toBeVisible()
  })

  it('names the workspace a connection was installed in', async () => {
    window.history.replaceState(null, '', '/org?tab=settings&slack=installed&slack_workspace=T1')
    stubApi({ connections: [ownApp] })
    renderCard()

    await waitFor(() => expect(toasts.toast.success).toHaveBeenCalledWith('Connected Acme'))
    expect(window.location.search).toBe('?tab=settings')
  })

  it('opens the preview for a staged install', async () => {
    window.history.replaceState(null, '', '/org?tab=settings&slack=staged&slack_workspace=T1')
    const fetchMock = stubApi({
      connections: [ownApp, stagedDeployment],
      routes: { [`GET ${PREVIEW}`]: { body: preview() } },
    })
    renderCard()

    expect(await screen.findByRole('dialog', { name: 'Switch apps' })).toBeInTheDocument()
    expect(fetchMock).toHaveBeenCalledWith(PREVIEW, expect.anything())
    expect(window.location.search).toBe('?tab=settings')
  })
})

describe('SlackWorkspacesCard switch preview', () => {
  async function openPreview() {
    fireEvent.click(await screen.findByRole('button', { name: 'Review switch to Triage Factory' }))
    const dialog = await screen.findByRole('dialog', { name: 'Switch apps' })
    await within(dialog).findByText('From')
    return dialog
  }

  it('names both apps and groups the watched channels', async () => {
    stubApi({
      connections: [ownApp, stagedDeployment],
      routes: { [`GET ${PREVIEW}`]: { body: preview() } },
    })
    renderCard()
    const dialog = await openPreview()

    expect(within(dialog).getByText('U1')).toBeInTheDocument()
    expect(within(dialog).getByText('U9')).toBeInTheDocument()

    const member = within(dialog).getByRole('listitem', { name: 'Already a member' })
    expect(member).toHaveTextContent('2')
    expect(member).not.toHaveTextContent('#eng')
    expect(within(dialog).getByRole('listitem', { name: 'Will join' })).toHaveTextContent(
      'The new bot joins these on confirm.',
    )
    const invite = within(dialog).getByRole('listitem', { name: 'Needs an invite' })
    expect(invite).toHaveTextContent('1')
    expect(invite).toHaveTextContent('#secret')
    expect(within(dialog).getByRole('listitem', { name: 'Unknown' })).toHaveTextContent('#far')

    expect(
      within(dialog).getByText('People @-mention the new bot from then on.'),
    ).toBeInTheDocument()
    expect(within(dialog).getByText('Slack limits conversations.history.')).toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: 'Switch' })).toBeEnabled()
  })

  it('cannot be confirmed while a blocker stands', async () => {
    stubApi({
      connections: [ownApp, stagedDeployment],
      routes: {
        [`GET ${PREVIEW}`]: {
          body: preview({
            blockers: [{ code: 'missing_scopes', scopes: ['chat:write', 'channels:join'] }],
          }),
        },
      },
    })
    renderCard()
    const dialog = await openPreview()

    expect(within(dialog).getByText(/chat:write, channels:join/)).toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: 'Switch' })).toBeDisabled()
  })

  it('reads the preview again when it changed before the confirm', async () => {
    let reads = 0
    const fingerprints: string[] = []
    const fetchMock = stubApi({
      connections: [ownApp, stagedDeployment],
      routes: {
        [`GET ${PREVIEW}`]: () => ({ body: preview({ fingerprint: `fp-${++reads}` }) }),
        [`POST ${ACTIVATE}`]: (init) => {
          fingerprints.push(JSON.parse(String(init?.body)).fingerprint)
          return fingerprints.length === 1
            ? { status: 409, body: { errors: [{ reason: 'CONFLICT', message: 'stale' }] } }
            : { body: { connection: { ...stagedDeployment, active: true }, warnings: [] } }
        },
      },
    })
    renderCard()
    const dialog = await openPreview()

    fireEvent.click(within(dialog).getByRole('button', { name: 'Switch' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(
      'This changed since you opened it. Review it again.',
    )
    await waitFor(() =>
      expect(within(dialog).getByRole('button', { name: 'Switch' })).toBeEnabled(),
    )
    expect(fetchMock.mock.calls.filter(([p]) => p === PREVIEW)).toHaveLength(2)

    fireEvent.click(within(dialog).getByRole('button', { name: 'Switch' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(fingerprints).toEqual(['fp-1', 'fp-2'])
  })

  it('stays open while the switch is in flight, so its warnings still show', async () => {
    let finish!: (body: unknown) => void
    const activation = new Promise((resolve) => {
      finish = resolve
    })
    stubApi({
      connections: [ownApp, stagedDeployment],
      routes: {
        [`GET ${PREVIEW}`]: { body: preview() },
        [`POST ${ACTIVATE}`]: { body: activation },
      },
    })
    renderCard()
    const dialog = await openPreview()

    fireEvent.click(within(dialog).getByRole('button', { name: 'Switch' }))
    await within(dialog).findByRole('button', { name: 'Switching…' })
    fireEvent.keyDown(window, { key: 'Escape' })
    fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }))
    expect(screen.getByRole('dialog', { name: 'Switch apps' })).toBeInTheDocument()

    finish({
      connection: { ...stagedDeployment, active: true },
      warnings: [{ channel_id: 'C4', reason: 'invite_required' }],
    })
    expect(await screen.findByRole('dialog', { name: 'Switched' })).toBeInTheDocument()
  })

  it('lists the channels the new bot did not join, then closes on Done', async () => {
    stubApi({
      connections: [ownApp, stagedDeployment],
      routes: {
        [`GET ${PREVIEW}`]: { body: preview() },
        [`POST ${ACTIVATE}`]: {
          body: {
            connection: { ...stagedDeployment, active: true },
            warnings: [{ channel_id: 'C4', reason: 'invite_required' }],
          },
        },
      },
    })
    renderCard()
    const dialog = await openPreview()

    fireEvent.click(within(dialog).getByRole('button', { name: 'Switch' }))
    const done = await screen.findByRole('dialog', { name: 'Switched' })
    expect(within(done).getByText('#secret')).toBeInTheDocument()
    expect(within(done).getByText(/Invite the new bot to it in Slack/)).toBeInTheDocument()

    expect(within(done).getByRole('button', { name: 'Done' })).toHaveFocus()

    fireEvent.click(within(done).getByRole('button', { name: 'Done' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  })
})

describe('SlackWorkspacesCard for a member who is not an admin', () => {
  it('shows the connections and no actions', async () => {
    stubApi({ deploymentApp: true, connections: [ownApp, stagedDeployment] })
    renderCard(false)

    await screen.findByText('Acme')
    expect(screen.getByText('Your app')).toBeInTheDocument()
    expect(screen.getByText('Staged:')).toBeInTheDocument()
    // Settle the deployment app's read, which would add the connect actions.
    await waitFor(() =>
      expect(globalThis.fetch).toHaveBeenCalledWith(DEPLOYMENT_APP, expect.anything()),
    )
    expect(screen.queryAllByRole('button')).toHaveLength(0)
    expect(screen.queryAllByRole('link')).toHaveLength(0)
    expect(screen.queryByPlaceholderText('xoxb-…')).not.toBeInTheDocument()
  })
})
