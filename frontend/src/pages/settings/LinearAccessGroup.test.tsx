// The Linear connection group's two faces. Connected, it names who the key
// validated as and the workspace it belongs to, and withholds the in-place
// rebind when the environment supplies the key (that overlay wins on every
// read, so a key typed here would be stored and ignored). Unconnected, it is
// one masked field and nothing else — no URL, no deployment choice.
import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'

import LinearAccessGroup from './LinearAccessGroup'

function renderGroup(
  over: Partial<{ connected: boolean; envProvided: boolean; boundAs: string; workspace: string }>,
) {
  render(
    <LinearAccessGroup
      value={{ linear_api_key: '' }}
      onChange={() => {}}
      connected={over.connected ?? false}
      boundAs={over.boundAs}
      workspaceUrlKey={over.workspace}
      orgId="org-1"
      onReplace={() => {}}
      envProvided={over.envProvided ?? false}
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

  it('withholds the rebind when the environment supplies the key', () => {
    renderGroup({ connected: true, envProvided: true })
    expect(screen.getByText('Connected')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Replace key' })).not.toBeInTheDocument()
    expect(screen.getByText(/TRIAGE_FACTORY_LINEAR_API_KEY/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Disconnect' })).toBeInTheDocument()
  })
})
