import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, waitFor, within, act } from '@testing-library/react'
import UserSettingsPage from './UserSettingsPage'
import { HttpError } from '../../lib/apiClient'
import type { ApiToken, LoginMethod } from '../../types'

// The page's claims, checked against what it actually calls: the header is
// the login identity (the door), the accounts list is the integration
// identities (who the factory acts as), the tokens table shows a prefix and
// never the secret, presets are display math that send an absolute date, the
// secret appears exactly once, and revoke is a hold that DELETEs.

const DAY = 86_400_000
const NOW = Date.UTC(2026, 8, 5, 12, 0, 0)

const api = vi.hoisted(() => ({
  apiJSON: vi.fn(),
  apiListAll: vi.fn(),
  apiFetch: vi.fn(),
}))
vi.mock('../../lib/apiClient', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/apiClient')>()
  return { ...actual, ...api }
})

const auth = vi.hoisted(() => ({
  me: {
    id: 'u1',
    email: 'aidan@allchin.com',
    display_name: 'Aidan Allchin',
    orgs: [
      { id: 'o1', name: 'sky-ai-eng', role: 'admin' },
      { id: 'o2', name: 'sky-ai-labs', role: 'member' },
    ],
    teams: [],
    org_creation_enabled: true,
    is_operator: false,
  },
  refresh: vi.fn().mockResolvedValue(undefined),
}))
vi.mock('../../contexts/AuthContext', () => ({ useAuth: () => auth }))
vi.mock('../../contexts/OrgContext', () => ({ useActiveOrgId: () => 'o1' }))

const gh = vi.hoisted(() => ({
  state: {
    status: 'ready',
    data: { connected: true, login: 'aallchin', host: 'github.com', connect_available: true },
  },
  refresh: vi.fn(),
}))
const jira = vi.hoisted(() => ({
  state: {
    status: 'ready',
    data: {
      connected: true,
      account: 'aidan@allchin.com',
      host: 'sky-ai.atlassian.net',
      connect_available: false,
      deployment: 'cloud',
    },
  },
  refresh: vi.fn(),
}))
vi.mock('../../hooks/useGitHubIdentity', () => ({ useGitHubIdentity: () => gh }))
vi.mock('../../hooks/useJiraIdentity', () => ({ useJiraIdentity: () => jira }))

const captures = vi.hoisted(() => ({
  captureGitHubIdentityPat: vi.fn(),
  captureJiraIdentityApiToken: vi.fn(),
  captureJiraIdentityPat: vi.fn(),
}))
vi.mock('../../lib/githubIdentity', () => ({
  captureGitHubIdentityPat: captures.captureGitHubIdentityPat,
}))
vi.mock('../../lib/jiraIdentity', () => ({
  captureJiraIdentityApiToken: captures.captureJiraIdentityApiToken,
  captureJiraIdentityPat: captures.captureJiraIdentityPat,
}))

const IDENTITIES: LoginMethod[] = [
  {
    provider: 'github',
    email: 'aidan@allchin.com',
    email_verified: true,
    linked_at: '2026-01-12T09:00:00Z',
    current: true,
    login: 'aallchin',
  },
]

function token(over: Partial<ApiToken>): ApiToken {
  return {
    id: 't1',
    name: 'laptop',
    org_id: 'o1',
    token_prefix: 'tf_Ab41Xe0p',
    created_at: new Date(NOW - 6 * DAY).toISOString(),
    last_used_at: new Date(NOW - 12 * 60_000).toISOString(),
    expires_at: new Date(NOW + 24 * DAY).toISOString(),
    effective_expires_at: new Date(NOW + 24 * DAY).toISOString(),
    allowed_cidrs: [],
    ...over,
  }
}

const TOKENS: ApiToken[] = [
  token({}),
  token({
    id: 't2',
    name: 'deploy hook',
    token_prefix: 'tf_M2rTq8Wd',
    created_at: new Date(NOW - 88 * DAY).toISOString(),
    last_used_at: new Date(NOW - 26 * 3_600_000).toISOString(),
    expires_at: new Date(NOW + 277 * DAY).toISOString(),
    effective_expires_at: new Date(NOW + 2 * DAY).toISOString(),
    allowed_cidrs: ['52.14.9.20/32', '10.4.0.0/16', '2600:1f18::/32'],
  }),
  token({
    id: 't3',
    name: 'github action · release',
    token_prefix: 'tf_uT7wQn2c',
    org_id: 'o2',
    last_used_at: null,
    expires_at: null,
    effective_expires_at: null,
  }),
]

type Routes = Record<string, unknown | ((body: unknown) => unknown)>

function stub(routes: Routes, opts: { tokens?: ApiToken[] } = {}) {
  api.apiJSON.mockImplementation(async (path: string, init?: RequestInit) => {
    const r = routes[path]
    if (r === undefined) throw new Error('unstubbed ' + path)
    if (r instanceof Error) throw r
    return typeof r === 'function' ? r(init?.body ? JSON.parse(String(init.body)) : undefined) : r
  })
  api.apiListAll.mockImplementation(async (path: string) => {
    if (path === '/api/me/tokens/list') return opts.tokens ?? TOKENS
    throw new Error('unstubbed list ' + path)
  })
  api.apiFetch.mockResolvedValue({ status: 204, ok: true })
}

const BASE: Routes = {
  '/api/me/identities': { methods: IDENTITIES },
  '/api/orgs/o1/api-token-policy': { max_age_days: 90 },
  '/api/orgs/o2/api-token-policy': { max_age_days: null },
}

async function openTokens() {
  render(<UserSettingsPage />)
  // The count lands with the list.
  await screen.findByText('3 tokens')
  fireEvent.click(screen.getByRole('button', { name: /API TOKENS/ }))
  await screen.findByText('deploy hook')
}

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true, now: NOW })
  api.apiJSON.mockReset()
  api.apiListAll.mockReset()
  api.apiFetch.mockReset()
})
afterEach(() => vi.useRealTimers())

describe('UserSettingsPage — the header is the door', () => {
  it('names the person and the login they came in by, with the handle and the date', async () => {
    stub(BASE)
    render(<UserSettingsPage />)
    expect(screen.getByText('Aidan Allchin')).toBeInTheDocument()
    expect(await screen.findByText('GitHub @aallchin · since 12 Jan')).toBeInTheDocument()
  })

  it('wears the IdP vendor mark for a SAML login and footnotes a second login', async () => {
    stub({
      ...BASE,
      '/api/me/identities': {
        methods: [
          {
            provider: 'saml',
            email: 'aidan@allchin.com',
            email_verified: true,
            linked_at: '2026-01-12T09:00:00Z',
            current: true,
            idp: 'entra',
          },
          { ...IDENTITIES[0], current: false },
        ],
      },
    })
    render(<UserSettingsPage />)
    expect(
      await screen.findByText('Microsoft Entra aidan@allchin.com · since 12 Jan'),
    ).toBeInTheDocument()
    expect(screen.getByAltText('Microsoft Entra')).toHaveAttribute('src', '/idp/entra.svg')
    expect(screen.getByText('also signs in with GitHub @aallchin')).toBeInTheDocument()
  })

  it('falls back to SSO in type when the product is not known', async () => {
    stub({
      ...BASE,
      '/api/me/identities': {
        methods: [
          { provider: 'saml', email: 'a@corp.example', email_verified: true, current: true },
        ],
      },
    })
    render(<UserSettingsPage />)
    expect(await screen.findByText('SSO a@corp.example')).toBeInTheDocument()
    expect(screen.queryByRole('img')).not.toBeInTheDocument()
  })
})

describe('UserSettingsPage — accounts are who the factory acts as', () => {
  it('lists GitHub and Jira with their verbs, and Connect redirects to the org host', async () => {
    stub(BASE)
    render(<UserSettingsPage />)
    expect(screen.getByText('@aallchin · github.com')).toBeInTheDocument()
    expect(screen.getByText('aidan@allchin.com · sky-ai.atlassian.net')).toBeInTheDocument()
    // Sign-in is never in this list.
    expect(
      within(document.querySelector('.ac') as HTMLElement).queryByText(/since/),
    ).not.toBeInTheDocument()

    const assign = vi.fn()
    Object.defineProperty(window, 'location', { value: { href: '' }, writable: true })
    fireEvent.click(screen.getByRole('button', { name: 'Change' }))
    fireEvent.click(screen.getByRole('button', { name: 'Continue with GitHub' }))
    expect(window.location.href).toBe('/api/orgs/o1/github/connect/start?return_to=%2Fsettings')
    void assign
  })

  it('verifies a Cloud Jira credential through the capture path and refreshes', async () => {
    stub(BASE)
    captures.captureJiraIdentityApiToken.mockResolvedValue({
      account: 'aidan@sky.ai',
      host: 'sky-ai.atlassian.net',
    })
    render(<UserSettingsPage />)
    fireEvent.click(screen.getByRole('button', { name: 'Reconnect' }))
    fireEvent.change(screen.getByLabelText('ATLASSIAN ACCOUNT EMAIL'), {
      target: { value: 'aidan@sky.ai' },
    })
    fireEvent.change(screen.getByLabelText('API TOKEN'), { target: { value: 'atl-1' } })
    fireEvent.click(screen.getByRole('button', { name: 'Verify' }))
    await waitFor(() =>
      expect(captures.captureJiraIdentityApiToken).toHaveBeenCalledWith(
        'o1',
        'aidan@sky.ai',
        'atl-1',
      ),
    )
    expect(await screen.findByText('aidan@sky.ai · sky-ai.atlassian.net')).toBeInTheDocument()
    expect(jira.refresh).toHaveBeenCalled()
  })
})

describe('UserSettingsPage — the tokens', () => {
  it('reads as a collapsed row: the count, and the next expiry warm when soon', async () => {
    stub(BASE)
    render(<UserSettingsPage />)
    expect(await screen.findByText('3 tokens')).toBeInTheDocument()
    const tail = screen.getByText('next expiry in 2d')
    expect(tail).toHaveAttribute('data-tone', 'warm')
    expect(screen.queryByText('deploy hook')).not.toBeInTheDocument()
  })

  it('opens to a table with prefix-only secrets, day-math expiry, and the org column for two memberships', async () => {
    stub(BASE)
    await openTokens()
    // The secret never appears — only its head.
    expect(screen.getByText('tf_Ab41Xe0p…')).toBeInTheDocument()
    expect(document.body.textContent).not.toMatch(/tf_[A-Za-z0-9_-]{20,}/)
    expect(screen.getByText('in 24d')).toBeInTheDocument()
    expect(screen.getByText('in 2d')).toHaveStyle({ color: 'var(--color-warm)' })
    expect(screen.getAllByText('never').length).toBeGreaterThan(0)
    expect(screen.getByRole('columnheader', { name: /ORGANIZATION/ })).toBeInTheDocument()
    expect(screen.getByText('sky-ai-labs')).toBeInTheDocument()
    // The head's tail changes with the section open; the footer names each cap.
    expect(
      screen.getByText('a token acts as you inside one organization and nowhere else'),
    ).toBeInTheDocument()
    expect(
      screen.getByText('sky-ai-eng caps tokens at 90 days · sky-ai-labs sets no cap'),
    ).toBeInTheDocument()
  })

  it('drops the org column for a single membership', async () => {
    const orgs = auth.me.orgs
    auth.me.orgs = [orgs[0]]
    try {
      stub(BASE, { tokens: TOKENS.filter((t) => t.org_id === 'o1') })
      render(<UserSettingsPage />)
      await screen.findByText('2 tokens')
      fireEvent.click(screen.getByRole('button', { name: /API TOKENS/ }))
      await screen.findByText('deploy hook')
      expect(screen.queryByRole('columnheader', { name: /ORGANIZATION/ })).not.toBeInTheDocument()
    } finally {
      auth.me.orgs = orgs
    }
  })

  it('says so when there are none', async () => {
    stub(BASE, { tokens: [] })
    render(<UserSettingsPage />)
    expect(await screen.findByText('none')).toBeInTheDocument()
    expect(screen.getByText('headless access to the API, as you')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: /API TOKENS/ }))
    expect(
      await screen.findByText(
        'No tokens. Everything you do in the browser uses your session instead.',
      ),
    ).toBeInTheDocument()
  })
})

describe('UserSettingsPage — the sheet', () => {
  it('opens on the row, reads the figures and the ranges, and explains the cap', async () => {
    stub(BASE)
    await openTokens()
    fireEvent.click(screen.getByText('deploy hook'))
    const dlg = await screen.findByRole('dialog')
    expect(within(dlg).getByText('88d')).toBeInTheDocument()
    expect(within(dlg).getByText('old · since 9 Jun')).toBeInTheDocument()
    expect(within(dlg).getByText('1d')).toBeInTheDocument()
    expect(within(dlg).getByText('2d')).toHaveAttribute('data-tone', 'warm')
    expect(within(dlg).getByText('left · expires 7 Sept')).toBeInTheDocument()
    expect(within(dlg).getByText('3 of 20')).toBeInTheDocument()
    expect(within(dlg).getByText('2600:1f18::/32')).toBeInTheDocument()
    expect(
      within(dlg).getByText(/Expires by sky-ai-eng’s 90-day cap, not the 9 Jun 2027 you set\./),
    ).toBeInTheDocument()
    expect(
      within(dlg).getByText(/A request from any other address fails as an invalid token would\./),
    ).toBeInTheDocument()
    // The sheet's prefix and org, on one line.
    expect(within(dlg).getByText('tf_M2rTq8Wd…')).toBeInTheDocument()
    expect(within(dlg).getByText('sky-ai-eng')).toBeInTheDocument()
  })

  it('renames in place with a PATCH', async () => {
    stub({
      ...BASE,
      '/api/me/tokens/t1': (body: { name: string }) => ({ ...TOKENS[0], name: body.name }),
    })
    await openTokens()
    fireEvent.click(screen.getByText('laptop'))
    const dlg = await screen.findByRole('dialog')
    // The name is the field: click it to edit.
    fireEvent.click(within(dlg).getByRole('button', { name: 'laptop' }))
    const field = within(dlg).getByLabelText('Rename')
    fireEvent.change(field, { target: { value: 'work laptop' } })
    fireEvent.keyDown(field, { key: 'Enter' })
    await waitFor(() =>
      expect(api.apiJSON).toHaveBeenCalledWith(
        '/api/me/tokens/t1',
        expect.objectContaining({ method: 'PATCH', body: JSON.stringify({ name: 'work laptop' }) }),
      ),
    )
    expect(await within(dlg).findByText('work laptop')).toBeInTheDocument()
  })

  it('revokes on a completed hold, closes, and the row leaves', async () => {
    stub(BASE)
    await openTokens()
    fireEvent.click(screen.getByText('laptop'))
    const dlg = await screen.findByRole('dialog')
    const verb = within(dlg).getByRole('button', { name: 'Hold to revoke' })
    fireEvent.pointerDown(verb, { button: 0, clientX: 0, clientY: 0 })
    await act(async () => {
      vi.advanceTimersByTime(900 + 220)
    })
    await waitFor(() =>
      expect(api.apiFetch).toHaveBeenCalledWith(
        '/api/me/tokens/t1',
        expect.objectContaining({ method: 'DELETE' }),
      ),
    )
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(screen.queryByText('laptop')).not.toBeInTheDocument()
  })
})

describe('UserSettingsPage — create, and the secret shown once', () => {
  it('sends an absolute expires_at for a preset, never a duration, and strikes presets past the cap', async () => {
    const made = token({ id: 't9', name: 'ci', token_prefix: 'tf_NEWNEWNE' })
    stub({
      ...BASE,
      '/api/me/tokens': (body: Record<string, unknown>) => ({
        ...made,
        expires_at: body.expires_at,
        effective_expires_at: body.expires_at,
        token: 'tf_NEWNEWNEWabcdefghijklmnopqrstuvwxyz0123456',
      }),
    })
    await openTokens()
    fireEvent.click(screen.getByRole('button', { name: '+ new token' }))
    const dlg = await screen.findByRole('dialog', { name: 'New API token' })

    fireEvent.change(within(dlg).getByLabelText('Token name'), { target: { value: 'ci' } })
    fireEvent.click(within(dlg).getByRole('button', { name: 'sky-ai-eng' }))
    // Past the 90-day cap: struck, with the reason, and not pickable.
    const never = within(dlg).getByRole('button', { name: 'never' })
    expect(never).toHaveAttribute('data-off')
    expect(never).toHaveAttribute('title', 'sky-ai-eng caps tokens at 90 days')
    expect(within(dlg).getByText('sky-ai-eng caps tokens at 90 days')).toBeInTheDocument()
    fireEvent.click(within(dlg).getByRole('button', { name: '30 days' }))
    fireEvent.click(within(dlg).getByRole('button', { name: 'Create token' }))

    await waitFor(() =>
      expect(api.apiJSON).toHaveBeenCalledWith(
        '/api/me/tokens',
        expect.objectContaining({ method: 'POST' }),
      ),
    )
    const call = api.apiJSON.mock.calls.find((c) => c[0] === '/api/me/tokens')!
    const body = JSON.parse(String((call[1] as RequestInit).body))
    expect(body).toEqual({
      name: 'ci',
      org_id: 'o1',
      expires_at: new Date(NOW + 30 * DAY).toISOString(),
    })
    expect(body).not.toHaveProperty('expires_in_days')

    // The show-once dialog: the plaintext, its consequences, and Copy in the
    // cancel slot so Escape and the backdrop copy rather than dismiss.
    const once = await screen.findByRole('dialog', { name: 'Token created' })
    expect(
      within(once).getByText('tf_NEWNEWNEWabcdefghijklmnopqrstuvwxyz0123456'),
    ).toBeInTheDocument()
    expect(
      within(once).getByText('Acts as you inside sky-ai-eng, and nowhere else'),
    ).toBeInTheDocument()
    expect(within(once).getByText('Expires in 30 days')).toBeInTheDocument()
    expect(within(once).getByText('Accepted from any address')).toBeInTheDocument()
    expect(within(once).getByRole('button', { name: 'Copy' })).toBeInTheDocument()

    fireEvent.click(within(once).getByRole('button', { name: 'I’ve saved it' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    // Gone for good: the table shows the head only.
    expect(document.body.textContent).not.toContain('tf_NEWNEWNEWabcdefghijklmnopqrstuvwxyz0123456')
    expect(screen.getByText('tf_NEWNEWNE…')).toBeInTheDocument()
  })

  it('adds ranges on Enter with fast validation, and sends them', async () => {
    stub({
      ...BASE,
      '/api/me/tokens': (body: Record<string, unknown>) => ({
        ...token({
          id: 't9',
          name: 'hook',
          org_id: 'o2',
          expires_at: (body.expires_at as string | undefined) ?? null,
          effective_expires_at: (body.expires_at as string | undefined) ?? null,
          allowed_cidrs: body.allowed_cidrs as string[],
        }),
        token: 'tf_secret',
      }),
    })
    await openTokens()
    fireEvent.click(screen.getByRole('button', { name: '+ new token' }))
    const dlg = await screen.findByRole('dialog', { name: 'New API token' })
    fireEvent.change(within(dlg).getByLabelText('Token name'), { target: { value: 'hook' } })
    fireEvent.click(within(dlg).getByRole('button', { name: 'sky-ai-labs' }))

    const cidr = within(dlg).getByLabelText('Allowed IP range')
    fireEvent.change(cidr, { target: { value: 'nope' } })
    fireEvent.keyDown(cidr, { key: 'Enter' })
    expect(
      within(dlg).getByText('nope is not a CIDR range (10.4.0.0/16, 2600:1f18::/32)'),
    ).toBeInTheDocument()
    fireEvent.change(cidr, { target: { value: '10.4.0.0/16' } })
    fireEvent.keyDown(cidr, { key: 'Enter' })
    expect(
      within(dlg).getByText('1 of 20 · requests from elsewhere fail as an invalid token would'),
    ).toBeInTheDocument()
    // No cap on sky-ai-labs: nothing struck, never is pickable.
    fireEvent.click(within(dlg).getByRole('button', { name: 'never' }))
    fireEvent.click(within(dlg).getByRole('button', { name: 'Create token' }))

    await waitFor(() =>
      expect(api.apiJSON).toHaveBeenCalledWith(
        '/api/me/tokens',
        expect.objectContaining({ method: 'POST' }),
      ),
    )
    const call = api.apiJSON.mock.calls.find((c) => c[0] === '/api/me/tokens')!
    expect(JSON.parse(String((call[1] as RequestInit).body))).toEqual({
      name: 'hook',
      org_id: 'o2',
      allowed_cidrs: ['10.4.0.0/16'],
    })
    const once = await screen.findByRole('dialog', { name: 'Token created' })
    expect(within(once).getByText('Never expires')).toBeInTheDocument()
    expect(within(once).getByText('Accepted from 1 IP range')).toBeInTheDocument()
  })

  it("surfaces the server's refusal verbatim and keeps the draft open", async () => {
    stub({
      ...BASE,
      '/api/me/tokens': new HttpError(
        409,
        JSON.stringify({
          errors: [
            { reason: 'CONFLICT', message: 'at most 50 active API tokens per user per org' },
          ],
        }),
        '/api/me/tokens',
      ),
    })
    await openTokens()
    fireEvent.click(screen.getByRole('button', { name: '+ new token' }))
    const dlg = await screen.findByRole('dialog', { name: 'New API token' })
    fireEvent.change(within(dlg).getByLabelText('Token name'), { target: { value: 'x' } })
    fireEvent.click(within(dlg).getByRole('button', { name: 'sky-ai-eng' }))
    fireEvent.click(within(dlg).getByRole('button', { name: 'Create token' }))
    expect(
      await within(dlg).findByText('at most 50 active API tokens per user per org'),
    ).toBeInTheDocument()
    expect(screen.getByRole('dialog', { name: 'New API token' })).toBeInTheDocument()
  })
})
