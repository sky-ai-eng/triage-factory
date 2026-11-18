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

function stubConnections() {
  const fetchMock = vi.fn((input: unknown) =>
    Promise.resolve(
      String(input) === LIST
        ? {
            ok: true,
            status: 200,
            ...jsonBody({ items: [connection], next_page_token: '', total_count: 1 }),
          }
        : { ok: true, status: 200, ...jsonBody({ status: 'deleted' }) },
    ),
  )
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
