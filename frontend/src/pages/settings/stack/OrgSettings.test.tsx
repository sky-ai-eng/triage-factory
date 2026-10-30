// The Linear connection section of the org stack. With an app to install it is
// Install alone, an action section with no footer, until "Use a personal API
// key instead" turns it into the key form; Cancel folds it back. Connected and
// rebinding render as they always have.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import OrgSettings from './OrgSettings'
import { jsonBody } from '../../../test/apiResponse'
import type { WizardState } from '../../setup/types'

const loadMocks = vi.hoisted(() => ({ org: {} as Record<string, unknown> }))
vi.mock('../../setup/steps', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../setup/steps')>()
  return {
    ...actual,
    loadOrg: async () => loadMocks.org,
    loadGitHubAppInstall: async () => ({}),
  }
})

const seed = (over: Partial<WizardState>) => {
  loadMocks.org = over
}

const linearSection = () => screen.getByRole('button', { name: /^Linear connection/ })
const keyLink = () => screen.queryByRole('button', { name: 'Use a personal API key instead' })

async function openLinearSection() {
  render(<OrgSettings orgId="org-1" isLocal />)
  await userEvent.click(await screen.findByRole('button', { name: /^Linear connection/ }))
}

beforeEach(() => {
  vi.restoreAllMocks()
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })),
  )
})

describe('OrgSettings — Linear connection', () => {
  it('is Install and the key link alone while an app resolves, with no footer', async () => {
    seed({ linearInstallAvailable: true })
    await openLinearSection()
    expect(screen.getByRole('button', { name: 'Install Triage Factory' })).toBeInTheDocument()
    expect(keyLink()).toBeInTheDocument()
    expect(screen.getByText('A key acts as the person it belongs to.')).toBeInTheDocument()
    expect(screen.queryByPlaceholderText('lin_api_…')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Connect' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Cancel' })).not.toBeInTheDocument()
  })

  it('shows the key and the Connect / Cancel footer behind the link, and Cancel folds them', async () => {
    seed({ linearInstallAvailable: true })
    await openLinearSection()
    await userEvent.click(keyLink()!)
    expect(screen.getByPlaceholderText('lin_api_…')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Connect' })).toBeDisabled()
    expect(keyLink()).not.toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    // Cancel collapses the section; reopened, the key is behind the link again.
    await userEvent.click(linearSection())
    expect(keyLink()).toBeInTheDocument()
    expect(screen.queryByPlaceholderText('lin_api_…')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Connect' })).not.toBeInTheDocument()
  })

  it('keeps a typed key and its Connect when an app becomes available', async () => {
    seed({ linearInstallAvailable: false })
    vi.stubGlobal(
      'fetch',
      vi.fn((input: unknown) =>
        String(input) === '/api/orgs/org-1/linear/app'
          ? Promise.resolve({
              ok: true,
              ...jsonBody({
                app: null,
                install_available: true,
                using_deployment_default: true,
                create_url: '',
                redirect_uris: [],
              }),
            })
          : Promise.resolve({ ok: false, status: 404, ...jsonBody({}) }),
      ),
    )
    await openLinearSection()
    await userEvent.type(screen.getByPlaceholderText('lin_api_…'), 'lin_api_typed')
    // Opening the app section reads the deployment's app, which makes Install
    // available under the key already typed.
    await userEvent.click(screen.getByRole('button', { name: /^Linear OAuth app/ }))
    expect(
      await screen.findByRole('button', { name: 'Install Triage Factory' }),
    ).toBeInTheDocument()
    expect(screen.getByPlaceholderText('lin_api_…')).toHaveValue('lin_api_typed')
    expect(screen.getByRole('button', { name: 'Connect' })).toBeEnabled()
    expect(keyLink()).not.toBeInTheDocument()
  })

  it('shows the key field and footer with no link when no app resolves', async () => {
    seed({ linearInstallAvailable: false })
    await openLinearSection()
    expect(screen.getByPlaceholderText('lin_api_…')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Connect' })).toBeInTheDocument()
    expect(keyLink()).not.toBeInTheDocument()
  })

  it('rebinds a connected key with the key field, Install and the link left out', async () => {
    seed({
      linearInstallAvailable: true,
      linearConnected: true,
      linearAuthMethod: 'api_key',
      linearBoundAs: 'Ada',
      linearWorkspaceUrlKey: 'acme',
    })
    await openLinearSection()
    expect(screen.getByText('Connected as Ada in linear.app/acme')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Install app instead' })).toBeInTheDocument()
    expect(keyLink()).not.toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'Replace key' }))
    expect(screen.getByPlaceholderText('lin_api_…')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Replace key' })).toBeDisabled()
    expect(screen.queryByRole('button', { name: 'Install Triage Factory' })).not.toBeInTheDocument()
    expect(keyLink()).not.toBeInTheDocument()
  })
})
