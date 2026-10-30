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
    expect(screen.getByPlaceholderText('lin_api_…')).toBeInTheDocument()
    expect(
      screen.getByRole('checkbox', { name: /Also use this API key as my own Linear identity/ }),
    ).toBeInTheDocument()
    expect(
      screen.queryByRole('button', {
        name: 'Use a personal API key or your own Linear app instead',
      }),
    ).not.toBeInTheDocument()
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
})

describe('LinearAccessStep — an app to install', () => {
  const appPath = `/api/orgs/${LOCAL_DEFAULT_ORG_ID}/linear/app`
  const renderStep = (isLocal: boolean) => {
    vi.stubGlobal(
      'fetch',
      vi.fn((input: unknown) => {
        if (String(input) === appPath) {
          return Promise.resolve({
            ok: true,
            ...jsonBody({
              app: null,
              install_available: true,
              using_deployment_default: true,
              create_url: '',
              redirect_uris: [],
            }),
          })
        }
        return Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })
      }),
    )
    render(
      <LinearAccessStep
        state={{ ...initialWizardState(), tracker: 'linear', linearInstallAvailable: true }}
        patch={() => {}}
        orgId={LOCAL_DEFAULT_ORG_ID}
        teamId="default"
        isLocal={isLocal}
        advance={() => {}}
        hold={(work) => work()}
      />,
    )
  }
  const alternatives = () =>
    screen.queryByRole('button', { name: 'Use a personal API key or your own Linear app instead' })

  it('shows Install and the link, and nothing else', () => {
    renderStep(false)
    expect(screen.getByRole('button', { name: 'Install Triage Factory' })).toBeInTheDocument()
    expect(alternatives()).toBeInTheDocument()
    expect(screen.queryByPlaceholderText('lin_api_…')).not.toBeInTheDocument()
    expect(screen.queryByText('Or paste a personal API key')).not.toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: 'Set up your own Linear app' }),
    ).not.toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: /Install Triage Factory as an app instead/ }),
    ).not.toBeInTheDocument()
  })

  it('reveals the key field and an app of your own together, and drops the link', async () => {
    renderStep(false)
    await userEvent.click(alternatives()!)
    expect(screen.getByPlaceholderText('lin_api_…')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Set up your own Linear app' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Install Triage Factory' })).toBeInTheDocument()
    expect(alternatives()).not.toBeInTheDocument()
  })

  it('opens the app card from "Set up your own Linear app"', async () => {
    renderStep(false)
    await userEvent.click(alternatives()!)
    await userEvent.click(screen.getByRole('button', { name: 'Set up your own Linear app' }))
    expect(
      await screen.findByPlaceholderText("Your Linear OAuth app's client ID"),
    ).toBeInTheDocument()
    // The button was the deliberate click, so the card opens on its form.
    expect(screen.getByText(/Register your own below only if you need to/)).toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: /Use your own Linear app instead/ }),
    ).not.toBeInTheDocument()
  })

  it('keeps a typed key in view across a remount, since Continue binds it', () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })),
    )
    const base = initialWizardState()
    render(
      <LinearAccessStep
        state={{
          ...base,
          tracker: 'linear',
          linearInstallAvailable: true,
          org: { ...base.org, linear_api_key: 'lin_api_typed' },
        }}
        patch={() => {}}
        orgId={LOCAL_DEFAULT_ORG_ID}
        teamId="default"
        isLocal
        advance={() => {}}
        hold={(work) => work()}
      />,
    )
    expect(screen.getByPlaceholderText('lin_api_…')).toHaveValue('lin_api_typed')
    expect(
      screen.getByRole('checkbox', { name: /Also use this API key as my own Linear identity/ }),
    ).toBeInTheDocument()
    expect(alternatives()).not.toBeInTheDocument()
  })

  it('offers the reuse checkbox in local mode only once the key field shows', async () => {
    renderStep(true)
    const reuse = () =>
      screen.queryByRole('checkbox', { name: /Also use this API key as my own Linear identity/ })
    expect(reuse()).not.toBeInTheDocument()
    await userEvent.click(alternatives()!)
    expect(reuse()).toBeInTheDocument()
  })
})

describe('LinearAccessStep — connected', () => {
  it('shows the connection alone, with no alternatives to reveal', () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })),
    )
    render(
      <LinearAccessStep
        state={{
          ...initialWizardState(),
          tracker: 'linear',
          linearInstallAvailable: true,
          linearConnected: true,
          linearAuthMethod: 'app_install',
          linearBoundAs: 'Triage Factory',
          linearWorkspaceUrlKey: 'acme',
        }}
        patch={() => {}}
        orgId={LOCAL_DEFAULT_ORG_ID}
        teamId="default"
        isLocal
        advance={() => {}}
        hold={(work) => work()}
      />,
    )
    expect(screen.getByText('Installed as Triage Factory in linear.app/acme')).toBeInTheDocument()
    expect(
      screen.queryByRole('button', {
        name: 'Use a personal API key or your own Linear app instead',
      }),
    ).not.toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: 'Set up your own Linear app' }),
    ).not.toBeInTheDocument()
    expect(screen.queryByPlaceholderText('lin_api_…')).not.toBeInTheDocument()
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
  })
})
