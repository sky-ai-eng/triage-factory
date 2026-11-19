// The Slack card reads the org's connections from the org it is rendered for,
// a page at a time, and disconnects one by its workspace and app under that
// org's path.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'

const toasts = vi.hoisted(() => ({
  toast: { success: vi.fn(), error: vi.fn(), failed: vi.fn(), info: vi.fn(), warning: vi.fn() },
}))
vi.mock('../../components/Toast/toastStore', () => toasts)

import SlackWorkspacesCard from './SlackWorkspacesCard'
import { jsonBody } from '../../test/apiResponse'

const LIST = '/api/orgs/org-2/slack/connections/list'

const connection = {
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
}

// stubConnections answers the list with the connection until a DELETE of it,
// and with nothing after.
function stubConnections() {
  let deleted = false
  const fetchMock = vi.fn((input: unknown, init?: RequestInit) => {
    if (init?.method === 'DELETE') deleted = true
    const items = String(input) === LIST && !deleted ? [connection] : []
    const body =
      String(input) === LIST
        ? { items, next_page_token: '', total_count: items.length }
        : { status: 'deleted' }
    return Promise.resolve({ ok: true, status: 200, ...jsonBody(body) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('SlackWorkspacesCard', () => {
  it('lists the connections of the org it is rendered for', async () => {
    const fetchMock = stubConnections()
    render(<SlackWorkspacesCard orgId="org-2" />)

    expect(await screen.findByText('Acme')).toBeInTheDocument()
    expect(fetchMock).toHaveBeenCalledWith(LIST, expect.objectContaining({ method: 'POST' }))
  })

  it('refuses the expiring bot token of an app with token rotation on before calling', async () => {
    // An org with no connections shows the connect form open.
    const fetchMock = vi.fn(() =>
      Promise.resolve({
        ok: true,
        status: 200,
        ...jsonBody({ items: [], next_page_token: '', total_count: 0 }),
      }),
    )
    vi.stubGlobal('fetch', fetchMock)
    render(<SlackWorkspacesCard orgId="org-empty" />)

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
      '/api/orgs/org-empty/slack/connections/import',
      expect.anything(),
    )
  })

  it('disconnects a connection under the org’s path', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    const fetchMock = stubConnections()
    render(<SlackWorkspacesCard orgId="org-2" />)

    fireEvent.click(await screen.findByRole('button', { name: 'Disconnect Acme' }))

    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/orgs/org-2/slack/connections/T1/A1',
        expect.objectContaining({ method: 'DELETE' }),
      ),
    )
    await waitFor(() => expect(screen.queryByText('Acme')).not.toBeInTheDocument())
  })
})
