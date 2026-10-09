// The Linear connection group's two faces. Connected, it names who the key
// validated as and the workspace it belongs to. Unconnected, it is one masked
// field and nothing else — no URL, no deployment choice.
import { afterEach, describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'

import LinearAccessGroup from './LinearAccessGroup'

function renderGroup(over: Partial<{ connected: boolean; boundAs: string; workspace: string }>) {
  render(
    <LinearAccessGroup
      value={{ linear_api_key: '' }}
      onChange={() => {}}
      connected={over.connected ?? false}
      boundAs={over.boundAs}
      workspaceUrlKey={over.workspace}
      orgId="org-1"
      onReplace={() => {}}
      bare
    />,
  )
}

describe('LinearAccessGroup', () => {
  it('asks for an API key alone when unconnected', () => {
    const { container } = render(
      <LinearAccessGroup
        value={{ linear_api_key: '' }}
        onChange={() => {}}
        connected={false}
        orgId="org-1"
        bare
      />,
    )
    const inputs = container.querySelectorAll('input')
    expect(inputs).toHaveLength(1)
    expect(inputs[0]).toHaveAttribute('type', 'password')
    expect(screen.queryByRole('button', { name: 'Disconnect' })).not.toBeInTheDocument()
  })

  it('names the bound user and workspace, with both connected actions', () => {
    renderGroup({ connected: true, boundAs: 'Ada', workspace: 'acme' })
    expect(screen.getByText('Connected as Ada in linear.app/acme')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Replace key' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Disconnect' })).toBeInTheDocument()
  })
})

describe('LinearAccessGroup install path', () => {
  afterEach(() => {
    window.history.replaceState(null, '', '/')
  })

  it('offers Install first when an app resolves, with the key as the alternative', () => {
    render(
      <LinearAccessGroup
        value={{ linear_api_key: '' }}
        onChange={() => {}}
        connected={false}
        installAvailable
        orgId="org-1"
        bare
      />,
    )
    expect(screen.getByRole('button', { name: 'Install Triage Factory' })).toBeInTheDocument()
    expect(screen.getByText('Or paste a personal API key')).toBeInTheDocument()
    expect(screen.getByPlaceholderText('lin_api_…')).toBeInTheDocument()
  })

  it('offers no Install when no app resolves', () => {
    render(
      <LinearAccessGroup
        value={{ linear_api_key: '' }}
        onChange={() => {}}
        connected={false}
        orgId="org-1"
        bare
      />,
    )
    expect(screen.queryByRole('button', { name: 'Install Triage Factory' })).not.toBeInTheDocument()
  })

  it('names an installed app user, without a key to replace', () => {
    render(
      <LinearAccessGroup
        value={{ linear_api_key: '' }}
        onChange={() => {}}
        connected
        authMethod="app_install"
        boundAs="Triage Factory"
        workspaceUrlKey="acme"
        installAvailable
        orgId="org-1"
        onReplace={() => {}}
        bare
      />,
    )
    expect(screen.getByText('Installed as Triage Factory in linear.app/acme')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Replace key' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Install app instead' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Disconnect' })).toBeInTheDocument()
    expect(screen.getByText(/stays in the workspace’s member list/)).toBeInTheDocument()
  })

  it('lets a key-connected org switch to the app', () => {
    renderGroupWithInstall()
    expect(screen.getByRole('button', { name: 'Install app instead' })).toBeInTheDocument()
  })

  it('explains a revoked install', () => {
    render(
      <LinearAccessGroup
        value={{ linear_api_key: '' }}
        onChange={() => {}}
        connected={false}
        lastError="install_revoked"
        orgId="org-1"
        bare
      />,
    )
    expect(screen.getByRole('alert')).toHaveTextContent('removed from the Linear workspace')
  })

  it('shows the error an install returned with, then drops it from the URL', () => {
    window.history.replaceState(null, '', '/settings?linear_error=workspace_taken&tab=org')
    render(
      <LinearAccessGroup
        value={{ linear_api_key: '' }}
        onChange={() => {}}
        connected={false}
        orgId="org-1"
        bare
      />,
    )
    expect(screen.getByRole('alert')).toHaveTextContent('already connected to another')
    expect(window.location.search).toBe('?tab=org')
  })
})

function renderGroupWithInstall() {
  render(
    <LinearAccessGroup
      value={{ linear_api_key: '' }}
      onChange={() => {}}
      connected
      authMethod="api_key"
      boundAs="Ada"
      workspaceUrlKey="acme"
      installAvailable
      orgId="org-1"
      onReplace={() => {}}
      bare
    />,
  )
}
