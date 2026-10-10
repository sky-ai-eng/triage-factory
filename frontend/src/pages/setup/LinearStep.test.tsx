import { describe, it, expect, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { LinearAccessStep } from './LinearStep'
import { initialWizardState } from './steps'
import { LOCAL_DEFAULT_ORG_ID } from '../../lib/githubApp'
import { jsonBody } from '../../test/apiResponse'

describe('LinearAccessStep — disconnecting', () => {
  it('holds the wizard while it disconnects, then reads as not connected', async () => {
    const orgPath = `/api/orgs/${LOCAL_DEFAULT_ORG_ID}`
    vi.stubGlobal(
      'fetch',
      vi.fn((input: unknown) => {
        const path = String(input)
        if (path === `${orgPath}/linear/access/credential`) {
          return Promise.resolve({ ok: true, ...jsonBody({ status: 'disconnected' }) })
        }
        return Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })
      }),
    )
    const base = initialWizardState()
    const patch = vi.fn()
    const hold = vi.fn((work: () => Promise<void>) => work())
    render(
      <LinearAccessStep
        state={{
          ...base,
          linearConnected: true,
          linearBoundAs: 'Ada',
          linearWorkspaceUrlKey: 'acme',
          org: { ...base.org, version: 7 },
        }}
        patch={patch}
        orgId={LOCAL_DEFAULT_ORG_ID}
        teamId="default"
        isLocal
        advance={() => {}}
        hold={hold}
      />,
    )

    await userEvent.click(screen.getByRole('button', { name: 'Disconnect' }))

    // The whole disconnect runs under the wizard's busy guard, so Continue
    // cannot skip the bind on a connection this click is removing.
    expect(hold).toHaveBeenCalledTimes(1)
    await waitFor(() =>
      expect(patch).toHaveBeenLastCalledWith({
        linearConnected: false,
        linearWorkspaceUrlKey: '',
        linearBoundAs: '',
        linearAuthMethod: '',
        linearLastError: '',
        org: expect.objectContaining({ version: 7, linear_api_key: '' }),
      }),
    )
  })
})

describe('LinearAccessStep — no app to install yet', () => {
  it('registers an app in place, which makes Install available', async () => {
    const appPath = `/api/orgs/${LOCAL_DEFAULT_ORG_ID}/linear/app`
    const status = (available: boolean) => ({
      app: available
        ? { client_id: 'lin-client', registered_at: '', registered_by_display_name: '' }
        : null,
      install_available: available,
      using_deployment_default: false,
      create_url: 'https://linear.app/settings/api/applications/new',
      redirect_uris: [],
    })
    vi.stubGlobal(
      'fetch',
      vi.fn((input: unknown, init?: { method?: string }) => {
        if (String(input) === appPath) {
          const saved = init?.method === 'POST'
          return Promise.resolve({ ok: true, ...jsonBody(status(saved)) })
        }
        return Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })
      }),
    )
    const patch = vi.fn()
    render(
      <LinearAccessStep
        state={{ ...initialWizardState(), tracker: 'linear' }}
        patch={patch}
        orgId={LOCAL_DEFAULT_ORG_ID}
        teamId="default"
        isLocal
        advance={() => {}}
        hold={(work) => work()}
      />,
    )

    expect(screen.queryByRole('button', { name: 'Install Triage Factory' })).not.toBeInTheDocument()
    await userEvent.click(
      screen.getByRole('button', { name: /Install Triage Factory as an app instead/ }),
    )
    await userEvent.type(
      await screen.findByPlaceholderText("Your Linear OAuth app's client ID"),
      'lin-client',
    )
    await userEvent.type(
      screen.getByPlaceholderText("Your Linear OAuth app's client secret"),
      'secret{Enter}',
    )

    await waitFor(() => expect(patch).toHaveBeenCalledWith({ linearInstallAvailable: true }))
  })

  it('offers no app setup once an app resolves', () => {
    render(
      <LinearAccessStep
        state={{ ...initialWizardState(), tracker: 'linear', linearInstallAvailable: true }}
        patch={() => {}}
        orgId={LOCAL_DEFAULT_ORG_ID}
        teamId="default"
        isLocal
        advance={() => {}}
        hold={(work) => work()}
      />,
    )
    expect(screen.getByRole('button', { name: 'Install Triage Factory' })).toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: /Install Triage Factory as an app instead/ }),
    ).not.toBeInTheDocument()
  })
})
