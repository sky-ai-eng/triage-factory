// The Linear connection group's two faces. Connected, it names who the key
// validated as and the workspace it belongs to. Unconnected, it is one masked
// field and nothing else — no URL, no deployment choice.
import { afterEach, describe, it, expect, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'

import LinearAccessGroup from './LinearAccessGroup'
import type { LinearTeamVisibility } from './linearConnect'
import { jsonBody } from '../../test/apiResponse'

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

  it('leaves Install alone when the key field is hidden', () => {
    render(
      <LinearAccessGroup
        value={{ linear_api_key: '' }}
        onChange={() => {}}
        connected={false}
        installAvailable
        orgId="org-1"
        bare
        showKeyField={false}
      />,
    )
    expect(screen.getByRole('button', { name: 'Install Triage Factory' })).toBeInTheDocument()
    expect(screen.queryByText('Or paste a personal API key')).not.toBeInTheDocument()
    expect(screen.queryByPlaceholderText('lin_api_…')).not.toBeInTheDocument()
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

  it('drops the returned error once the org connects another way', () => {
    window.history.replaceState(null, '', '/setup?linear_error=denied')
    const group = (connected: boolean) => (
      <LinearAccessGroup
        value={{ linear_api_key: '' }}
        onChange={() => {}}
        connected={connected}
        boundAs={connected ? 'Ada' : undefined}
        workspaceUrlKey={connected ? 'acme' : undefined}
        orgId="org-1"
        bare
      />
    )
    const { rerender } = render(group(false))
    expect(screen.getByRole('alert')).toHaveTextContent('cancelled in Linear')
    rerender(group(true))
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('keeps the returned error for an org that was already connected', () => {
    window.history.replaceState(null, '', '/settings?linear_error=workspace_taken')
    renderGroupWithInstall()
    expect(screen.getByRole('alert')).toHaveTextContent('already connected to another')
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

// The tracked teams the connected credential reads: what the card warns about
// before a switch and names after one.
describe('LinearAccessGroup tracked-team visibility', () => {
  const stubVisibility = (v: LinearTeamVisibility) => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, status: 200, ...jsonBody(v) })
    vi.stubGlobal('fetch', fetchMock)
    return fetchMock
  }
  const tfac = { id: 't-1', key: 'TFAC', name: 'Triage Factory' }

  it('names a tracked team the connected credential cannot read', async () => {
    const fetchMock = stubVisibility({ private: [], unreadable: [tfac] })
    renderGroupWithInstall()
    expect(await screen.findByRole('alert')).toHaveTextContent(
      "This connection can't read the tracked Linear team Triage Factory (TFAC).",
    )
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/orgs/org-1/linear/access/team-visibility',
      expect.anything(),
    )
  })

  it('shows no banner when every tracked team is readable', async () => {
    const fetchMock = stubVisibility({ private: [tfac], unreadable: [] })
    renderGroupWithInstall()
    await waitFor(() => expect(fetchMock).toHaveBeenCalled())
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('names the private teams before installing over a key, and stays on Cancel', async () => {
    stubVisibility({ private: [tfac], unreadable: [] })
    renderGroupWithInstall()
    fireEvent.click(screen.getByRole('button', { name: 'Install app instead' }))
    expect(await screen.findByText('A tracked team is private')).toBeInTheDocument()
    expect(
      screen.getByText(
        'Triage Factory (TFAC): changes stop reaching Triage Factory unless it is shared with the app',
      ),
    ).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    await waitFor(() =>
      expect(screen.queryByText('A tracked team is private')).not.toBeInTheDocument(),
    )
    expect(window.location.pathname).toBe('/')
  })
})
