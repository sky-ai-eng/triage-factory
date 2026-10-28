// The Atlassian OAuth app card asks before an admin removes the org's app
// or replaces it with a different one, since a Jira connection made through it
// can only be refreshed by it. Rotating the same app's secret does not ask.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'

import AtlassianOAuthAppCard from './AtlassianOAuthAppCard'
import { jsonBody } from '../../test/apiResponse'

const configured = {
  app: {
    client_id: 'atl-client',
    registered_at: '2026-10-09T00:00:00Z',
    registered_by_display_name: 'Ada',
  },
  connect_available: true,
  using_deployment_default: false,
  connect_callback_url: 'https://tf.example/api/orgs/org-1/jira/connect/callback',
}

function renderConfigured() {
  const fetchMock = vi.fn().mockResolvedValue({ ok: true, ...jsonBody(configured) })
  vi.stubGlobal('fetch', fetchMock)
  render(<AtlassianOAuthAppCard orgId="org-1" />)
  return fetchMock
}

async function enterReplacement(clientId: string) {
  fireEvent.click(await screen.findByRole('button', { name: 'Replace' }))
  fireEvent.change(screen.getByPlaceholderText("Your Atlassian OAuth app's client ID"), {
    target: { value: clientId },
  })
  fireEvent.change(screen.getByPlaceholderText("Your Atlassian OAuth app's client secret"), {
    target: { value: 'new-secret' },
  })
  fireEvent.click(screen.getByRole('button', { name: 'Replace app' }))
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('AtlassianOAuthAppCard', () => {
  it('asks before removing the app, and keeps it when the admin declines', async () => {
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
    const fetchMock = renderConfigured()

    fireEvent.click(await screen.findByRole('button', { name: 'Remove' }))

    expect(confirm).toHaveBeenCalledWith(
      expect.stringContaining('Anyone who connected Jira through it will have to connect again'),
    )
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it('asks before saving a different client ID, and saves nothing when the admin declines', async () => {
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
    const fetchMock = renderConfigured()

    await enterReplacement('other-client')

    expect(confirm).toHaveBeenCalledWith(
      expect.stringContaining('Anyone who connected Jira through it will have to connect again'),
    )
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it('saves a different client ID once the admin agrees', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    const fetchMock = renderConfigured()

    await enterReplacement('other-client')

    await waitFor(() =>
      expect(fetchMock).toHaveBeenLastCalledWith(
        '/api/orgs/org-1/jira/app',
        expect.objectContaining({
          method: 'POST',
          body: JSON.stringify({ client_id: 'other-client', client_secret: 'new-secret' }),
        }),
      ),
    )
  })

  it("does not ask when rotating the same app's secret", async () => {
    const confirm = vi.spyOn(window, 'confirm')
    const fetchMock = renderConfigured()

    await enterReplacement(' atl-client ')

    await waitFor(() =>
      expect(fetchMock).toHaveBeenLastCalledWith(
        '/api/orgs/org-1/jira/app',
        expect.objectContaining({
          method: 'POST',
          body: JSON.stringify({ client_id: 'atl-client', client_secret: 'new-secret' }),
        }),
      ),
    )
    expect(confirm).not.toHaveBeenCalled()
  })
})
