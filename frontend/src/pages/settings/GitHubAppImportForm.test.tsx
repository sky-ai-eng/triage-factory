// The client secret by mode. In multi every org's App must let members
// sign in with GitHub to link their accounts, so the form asks for it as a
// plain field and won't submit without it; in local identity capture falls back
// to a token, so it stays optional behind the recommended section.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'

const ghMocks = vi.hoisted(() => ({
  getGitHubAppStatus: vi.fn(),
  importGitHubApp: vi.fn(),
}))
vi.mock('../../lib/githubApp', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/githubApp')>()
  return {
    ...actual,
    getGitHubAppStatus: ghMocks.getGitHubAppStatus,
    importGitHubApp: ghMocks.importGitHubApp,
  }
})

import GitHubAppImportForm from './GitHubAppImportForm'

const PEM = '-----BEGIN RSA PRIVATE KEY-----\nkey\n-----END RSA PRIVATE KEY-----'

function renderForm(isLocal: boolean) {
  const onImported = vi.fn()
  render(<GitHubAppImportForm orgId="org-1" isLocal={isLocal} onImported={onImported} />)
  fireEvent.change(screen.getByPlaceholderText('123456'), { target: { value: '42' } })
  fireEvent.change(screen.getByPlaceholderText(/BEGIN RSA PRIVATE KEY/), {
    target: { value: PEM },
  })
  return { onImported }
}

const connect = () => screen.getByRole('button', { name: 'Connect App' })

beforeEach(() => {
  ghMocks.getGitHubAppStatus.mockReset()
  ghMocks.getGitHubAppStatus.mockResolvedValue({
    app: null,
    installations: [],
    using_deployment_default: false,
    deployment_app_available: false,
    webhook_health: null,
    connect_callback_url: 'https://tf.example.com/api/orgs/org-1/github/connect/callback',
  })
  ghMocks.importGitHubApp.mockReset()
  ghMocks.importGitHubApp.mockResolvedValue({ ok: true, result: {} })
})

describe('GitHubAppImportForm · client secret', () => {
  it('requires the client secret in multi mode, as a plain field', async () => {
    renderForm(false)
    expect(screen.queryByText('(recommended)')).not.toBeInTheDocument()
    expect(connect()).toBeDisabled()

    fireEvent.change(screen.getByPlaceholderText("Generate one in the App's settings"), {
      target: { value: ' cs ' },
    })
    expect(connect()).toBeEnabled()
    expect(await screen.findByDisplayValue(/connect\/callback$/)).toBeInTheDocument()

    fireEvent.click(connect())
    await waitFor(() => {
      expect(ghMocks.importGitHubApp).toHaveBeenCalledWith(
        'org-1',
        expect.objectContaining({ app_id: '42', client_secret: 'cs' }),
      )
    })
  })

  it('keeps the client secret optional in local mode', async () => {
    renderForm(true)
    expect(screen.getByText('(recommended)')).toBeInTheDocument()
    expect(screen.queryByPlaceholderText("Generate one in the App's settings")).toBeNull()
    expect(connect()).toBeEnabled()

    fireEvent.click(connect())
    await waitFor(() => expect(ghMocks.importGitHubApp).toHaveBeenCalled())
    expect(ghMocks.importGitHubApp.mock.calls[0][1]).not.toHaveProperty('client_secret')
  })
})
