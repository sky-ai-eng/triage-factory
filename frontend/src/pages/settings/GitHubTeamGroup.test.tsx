import { describe, it, expect, vi, beforeEach } from 'vitest'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { useState } from 'react'
import GitHubTeamGroup from './GitHubTeamGroup'
import { jsonBody } from '../../test/apiResponse'
import type { GitHubGroup } from './teamConfig'
import type { GitHubTeamOption } from '../../lib/githubTeams'

const PLATFORM: GitHubGroup = {
  github_team_id: '11',
  github_org_id: '9',
  org_login: 'acme',
  team_slug: 'platform',
}

const option = (
  id: string,
  slug: string,
  over: Partial<GitHubTeamOption> = {},
): GitHubTeamOption => ({
  github_team_id: id,
  github_org_id: '9',
  org_login: 'acme',
  team_slug: slug,
  name: slug,
  listed: true,
  ...over,
})

interface Page {
  items: GitHubTeamOption[]
  next_page_token?: string
  total_count?: number
  credentials_missing?: boolean
}

/** stub answers the saved mappings and the list route; `list` sees each list
 *  request's body and answers the page for it. */
function stub(saved: GitHubGroup[], list: (body: Record<string, unknown>) => Page) {
  const fetchMock = vi.fn((input: unknown, init?: RequestInit) => {
    const path = String(input)
    if (path === '/api/teams/t1/github-groups') {
      return Promise.resolve({
        ok: true,
        status: 200,
        ...jsonBody({ groups: saved, role: 'admin' }),
      })
    }
    if (path === '/api/teams/t1/github-groups/list') {
      const page = list(JSON.parse(String(init?.body)))
      return Promise.resolve({
        ok: true,
        status: 200,
        ...jsonBody({ total_count: page.items.length, next_page_token: '', ...page }),
      })
    }
    return Promise.resolve({ ok: false, status: 404, ...jsonBody({ errors: [] }) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

/** The JSON bodies sent to the list route, in order. */
const listBodies = (fetchMock: ReturnType<typeof stub>) =>
  (fetchMock.mock.calls as unknown as [string, RequestInit | undefined][])
    .filter(([path]) => path === '/api/teams/t1/github-groups/list')
    .map(([, init]) => JSON.parse(String(init?.body)) as Record<string, unknown>)

/** Harness owns the selection the way a container does, seeding it from
 *  onLoaded. */
function Harness({
  includeMembership = false,
  onLoaded,
}: {
  includeMembership?: boolean
  onLoaded?: (groups: GitHubGroup[]) => void
}) {
  const [value, setValue] = useState<GitHubGroup[]>([])
  return (
    <GitHubTeamGroup
      value={value}
      onChange={setValue}
      teamId="t1"
      includeMembership={includeMembership}
      onLoaded={(seed) => {
        setValue(seed)
        onLoaded?.(seed)
      }}
    />
  )
}

// The handle is the row's monospaced line; its badges follow it.
const rowNames = () =>
  screen.getAllByRole('checkbox').map((b) => b.querySelector('.font-mono')?.textContent)

beforeEach(() => vi.restoreAllMocks())

describe('GitHubTeamGroup', () => {
  it('seeds from the saved set and renders the list route’s rows in its order', async () => {
    const onLoaded = vi.fn()
    stub([PLATFORM], () => ({
      // Not the alphabet: the order is the route's, and the page keeps it.
      items: [
        option('12', 'web'),
        option('11', 'platform'),
        option('13', 'gone', { listed: false, name: '' }),
      ],
    }))
    render(<Harness onLoaded={onLoaded} />)

    await waitFor(() => expect(onLoaded).toHaveBeenCalledWith([PLATFORM]))
    expect(rowNames()).toEqual(['acme/web', 'acme/platform', 'acme/gone'])
    // A mapping GitHub no longer lists says so, from the row's own flag.
    const gone = screen.getAllByRole('checkbox')[2]
    expect(within(gone).getByText('not on GitHub')).toBeInTheDocument()
    expect(within(screen.getAllByRole('checkbox')[1]).queryByText('not on GitHub')).toBeNull()
  })

  it('searches on the server and shows its answer whole', async () => {
    const fetchMock = stub([PLATFORM], (body) =>
      body.search
        ? // The server's answer to "plat" carries a team the needle is not
          // in; it is shown, because the answer is the server's.
          { items: [option('11', 'platform'), option('14', 'infra')] }
        : { items: [option('11', 'platform'), option('12', 'web')] },
    )
    render(<Harness />)
    await screen.findByText('acme/web')

    fireEvent.change(screen.getByLabelText('Search GitHub teams'), { target: { value: ' plat ' } })

    await waitFor(() => expect(screen.getByText('acme/infra')).toBeInTheDocument())
    expect(listBodies(fetchMock).at(-1)).toEqual({ search: 'plat', page_size: 200 })
    expect(listBodies(fetchMock).every((b) => !('q' in b))).toBe(true)
    expect(screen.queryByText('acme/web')).not.toBeInTheDocument()
  })

  it('filters the tabs by the unsaved selection, not by a search of its own', async () => {
    stub([PLATFORM], () => ({ items: [option('11', 'platform'), option('12', 'web')] }))
    render(<Harness />)
    await screen.findByText('acme/web')

    fireEvent.click(screen.getByRole('tab', { name: /Selected/ }))
    expect(rowNames()).toEqual(['acme/platform'])
    fireEvent.click(screen.getByRole('tab', { name: /Unselected/ }))
    expect(rowNames()).toEqual(['acme/web'])

    // Checking a team moves it between the tabs without a read.
    fireEvent.click(screen.getByRole('tab', { name: /All/ }))
    fireEvent.click(screen.getByRole('checkbox', { name: /acme\/web/ }))
    fireEvent.click(screen.getByRole('tab', { name: /Selected/ }))
    expect(rowNames()).toEqual(['acme/platform', 'acme/web'])
  })

  it('reads the next page with the server’s token', async () => {
    const fetchMock = stub([], (body) =>
      body.page_token
        ? { items: [option('12', 'web')], total_count: 2 }
        : { items: [option('11', 'platform')], total_count: 2, next_page_token: 'tok-2' },
    )
    render(<Harness />)
    fireEvent.click(await screen.findByRole('button', { name: 'Show 1 more' }))

    await waitFor(() => expect(screen.getByText('acme/web')).toBeInTheDocument())
    expect(listBodies(fetchMock).at(-1)).toEqual({ page_size: 200, page_token: 'tok-2' })
    expect(rowNames()).toEqual(['acme/platform', 'acme/web'])
  })

  it('pre-checks the admin’s own teams with the only_mine filter when nothing is saved', async () => {
    const onLoaded = vi.fn()
    const fetchMock = stub([], (body) =>
      body.only_mine
        ? { items: [option('12', 'web', { mine: true })] }
        : { items: [option('11', 'platform'), option('12', 'web', { mine: true })] },
    )
    render(<Harness includeMembership onLoaded={onLoaded} />)

    await waitFor(() =>
      expect(onLoaded).toHaveBeenCalledWith([
        { github_team_id: '12', github_org_id: '9', org_login: 'acme', team_slug: 'web' },
      ]),
    )
    expect(listBodies(fetchMock)).toContainEqual({ include_membership: true, page_size: 200 })
    expect(listBodies(fetchMock)).toContainEqual({
      include_membership: true,
      only_mine: true,
      page_size: 200,
    })
  })

  it('says a missing credential when the list reports one', async () => {
    stub([], () => ({ items: [], credentials_missing: true }))
    render(<Harness />)
    expect(await screen.findByText(/Reconnect GitHub/)).toBeInTheDocument()
  })
})
