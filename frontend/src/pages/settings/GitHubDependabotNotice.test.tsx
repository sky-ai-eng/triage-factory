// What the GitHub access card says about Dependabot alerts, from the counts.
import { describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'

import GitHubDependabotNotice from './GitHubDependabotNotice'
import { jsonBody } from '../../test/apiResponse'

type Counts = Record<'readable' | 'no_permission' | 'disabled' | 'unchecked', number>

function renderWith(c: Partial<Counts>, credential: 'app' | 'token') {
  const fetchMock = vi.fn().mockResolvedValue({
    ok: true,
    ...jsonBody({ readable: 0, no_permission: 0, disabled: 0, unchecked: 0, ...c }),
  })
  vi.stubGlobal('fetch', fetchMock)
  const view = render(<GitHubDependabotNotice orgId="org-1" credential={credential} />)
  return { fetchMock, view }
}

describe('GitHubDependabotNotice', () => {
  it('reads the org’s counts', async () => {
    const { fetchMock } = renderWith({ readable: 1 }, 'app')
    await screen.findByText('Triage Factory can read Dependabot alerts on 1 tracked repository.')
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/orgs/org-1/github/dependabot-access',
      expect.anything(),
    )
  })

  it('says nothing before any tracked repository is checked', async () => {
    const { fetchMock, view } = renderWith({ unchecked: 4 }, 'token')
    await waitFor(() => expect(fetchMock).toHaveBeenCalled())
    expect(view.container).toBeEmptyDOMElement()
  })

  it('warns a token org, naming the scope', async () => {
    renderWith({ readable: 1, no_permission: 2 }, 'token')
    await screen.findByText(
      'Triage Factory can’t read Dependabot alerts on 2 tracked repositories.',
    )
    expect(screen.getByText(/security_events/)).toBeInTheDocument()
    expect(screen.getByText('Everything else on GitHub works as before.')).toBeInTheDocument()
  })

  it('warns an App org, naming the permission', async () => {
    renderWith({ no_permission: 1 }, 'app')
    await screen.findByText('Triage Factory can’t read Dependabot alerts on 1 tracked repository.')
    expect(screen.getByText(/each installation has to accept/)).toBeInTheDocument()
  })

  it('names who sees alerts once they are readable', async () => {
    renderWith({ readable: 3, disabled: 1 }, 'app')
    await screen.findByText(/Every member of a team that tracks a repository/)
    expect(
      screen.getByText('Dependabot alerts are turned off on GitHub for 1 tracked repository.'),
    ).toBeInTheDocument()
  })
})
