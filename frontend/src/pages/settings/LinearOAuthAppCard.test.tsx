// The Linear OAuth app card: it links to Linear's pre-filled creation page,
// lists the redirect URIs to register, and reports the server's refusal to
// remove an app a live install depends on.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'

import LinearOAuthAppCard from './LinearOAuthAppCard'
import { jsonBody } from '../../test/apiResponse'

const createURL = 'https://linear.app/settings/api/applications/new?distribution=private'
const uris = [
  'https://tf.example/api/linear/install/callback',
  'https://tf.example/api/linear/connect/callback',
]

function status(over: Record<string, unknown> = {}) {
  return {
    app: null,
    install_available: false,
    using_deployment_default: false,
    create_url: createURL,
    redirect_uris: uris,
    ...over,
  }
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('LinearOAuthAppCard', () => {
  it('links to the pre-filled creation page and lists both redirect URIs', async () => {
    const onStatus = vi.fn()
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true, ...jsonBody(status()) }))
    render(<LinearOAuthAppCard orgId="org-1" onStatus={onStatus} />)

    const link = await screen.findByRole('link', { name: /Create the app in Linear/ })
    expect(link).toHaveAttribute('href', createURL)
    expect(screen.getByLabelText('redirect URI 1')).toHaveValue(uris[0])
    expect(screen.getByLabelText('redirect URI 2')).toHaveValue(uris[1])
    expect(onStatus).toHaveBeenCalledWith(expect.objectContaining({ install_available: false }))
  })

  it('saves the pasted credentials and reports the install as available', async () => {
    const onStatus = vi.fn()
    const saved = status({
      app: {
        client_id: 'lin-client',
        registered_at: '2026-10-09T00:00:00Z',
        registered_by_display_name: 'Ada',
      },
      install_available: true,
    })
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce({ ok: true, ...jsonBody(status()) })
      .mockResolvedValueOnce({ ok: true, ...jsonBody(saved) })
    vi.stubGlobal('fetch', fetchMock)
    render(<LinearOAuthAppCard orgId="org-1" onStatus={onStatus} />)

    await screen.findByRole('link', { name: /Create the app in Linear/ })
    fireEvent.change(screen.getByPlaceholderText("Your Linear OAuth app's client ID"), {
      target: { value: ' lin-client ' },
    })
    fireEvent.change(screen.getByPlaceholderText("Your Linear OAuth app's client secret"), {
      target: { value: 'secret' },
    })
    fireEvent.click(screen.getByRole('button', { name: 'Save app' }))

    expect(await screen.findByText('Linear app configured')).toBeInTheDocument()
    expect(fetchMock).toHaveBeenLastCalledWith(
      '/api/orgs/org-1/linear/app',
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({ client_id: 'lin-client', client_secret: 'secret' }),
      }),
    )
    expect(onStatus).toHaveBeenLastCalledWith(expect.objectContaining({ install_available: true }))
  })

  it('shows why a remove was refused', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    const configured = status({
      app: {
        client_id: 'lin-client',
        registered_at: '2026-10-09T00:00:00Z',
        registered_by_display_name: '',
      },
      install_available: true,
    })
    const refusal = {
      errors: [
        {
          reason: 'CONFLICT',
          message:
            'Triage Factory is installed in Linear with this app — disconnect the installed app first',
        },
      ],
    }
    vi.stubGlobal(
      'fetch',
      vi
        .fn()
        .mockResolvedValueOnce({ ok: true, ...jsonBody(configured) })
        .mockResolvedValueOnce({ ok: false, status: 409, ...jsonBody(refusal) }),
    )
    render(<LinearOAuthAppCard orgId="org-1" />)

    fireEvent.click(await screen.findByRole('button', { name: 'Remove' }))
    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent('disconnect the installed app first'),
    )
  })

  it('says so when the status read fails, rather than look like no app is saved', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({ ok: false, status: 500, ...jsonBody({ errors: [] }) }),
    )
    render(<LinearOAuthAppCard orgId="org-1" />)

    expect(await screen.findByRole('alert')).toHaveTextContent('an app may already be saved')
  })
})
