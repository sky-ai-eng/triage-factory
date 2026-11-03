import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, act, waitFor, fireEvent } from '@testing-library/react'
import { MemoryRouter } from 'react-router'

import type { WSEvent } from '../types'
import { jsonBody, listBody } from '../test/apiResponse'

// Live updates arrive through the singleton websocket. Mocking the module hands
// the test the handler the page registered, so events dispatch synchronously
// with no socket in play.
let dispatch: ((event: WSEvent) => void) | null = null
vi.mock('../hooks/useWebSocket', () => ({
  useWebSocket: (handler: (event: WSEvent) => void) => {
    dispatch = handler
  },
  setPresenceView: vi.fn(),
}))

import Repos from './Repos'

const REPO_ID = 'a2c1f0d4-0000-4000-8000-000000000001'
// The pinned team's tracked set, as GET /api/teams/{id}/github-repos answers it.
const TRACKED = { id: REPO_ID, github_id: '1296269', owner: 'acme', repo: 'api' }

/** One registry row as POST /api/repos/list serves it: identity is the id,
 *  the name rides along as a display property. */
function repoRow(over: Record<string, unknown> = {}) {
  return {
    id: REPO_ID,
    slug: 'acme/api',
    owner: 'acme',
    repo: 'api',
    has_readme: true,
    has_claude_md: false,
    has_agents_md: false,
    default_branch: 'main',
    clone_status: 'ok',
    can_edit: true,
    ...over,
  }
}

function stubFetch(rows: unknown[]) {
  return vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input)
    if (url.startsWith('/api/repos/list')) return { ok: true, ...listBody(rows) }
    if (url.includes('/github-repos') && url.startsWith('/api/teams/')) {
      return { ok: true, ...jsonBody({ repos: [TRACKED], role: 'admin' }) }
    }
    if (/^\/api\/orgs\/[^/]+\/settings/.test(url)) {
      return { ok: true, ...jsonBody({ github_base_url: 'https://github.com' }) }
    }
    return { ok: true, ...jsonBody({}) }
  })
}

function renderRepos() {
  return render(
    <MemoryRouter>
      <Repos />
    </MemoryRouter>,
  )
}

describe('Repos page addressing', () => {
  beforeEach(() => {
    dispatch = null
  })

  it('renders the slug as the card title', async () => {
    vi.stubGlobal('fetch', stubFetch([repoRow()]))
    renderRepos()
    expect(await screen.findByText('acme/api')).toBeInTheDocument()
  })

  // The invariant an id-keyed wire buys: a repository renamed between two
  // websocket frames keeps its row id, so the merge goes on matching. Keyed on
  // the name, this event would match no card and the profile text would never
  // arrive — the card silently stops updating for the rest of the session.
  it('merges a repository_updated frame that arrives after a rename', async () => {
    vi.stubGlobal('fetch', stubFetch([repoRow()]))
    renderRepos()
    await screen.findByText('acme/api')

    act(() => {
      dispatch?.({
        type: 'repository_updated',
        data: {
          id: REPO_ID,
          // GitHub renamed the repository since the page loaded; the id is
          // the same row.
          slug: 'acme/api-v2',
          profile_text: 'the profile arrived anyway',
        },
      } as WSEvent)
    })

    await waitFor(() => {
      expect(screen.getByText('the profile arrived anyway')).toBeInTheDocument()
    })
  })

  // The save renders the server's row, not the value it sent. The two agree
  // about base_branch, so the assertion is on a field only the response
  // carries: an optimistic merge of the request would show the card without
  // it, and a refetch of the whole list to find it is the round trip the
  // response exists to spare.
  it('renders the row the base-branch save returned', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.startsWith('/api/repos/list')) return { ok: true, ...listBody([repoRow()]) }
      if (url.includes('/branches/list')) return { ok: true, ...listBody([{ name: 'develop' }]) }
      if (url === `/api/repos/${REPO_ID}` && init?.method === 'PATCH') {
        return {
          ok: true,
          ...jsonBody(
            repoRow({ base_branch: 'develop', profile_text: 'only the response carries this' }),
          ),
        }
      }
      if (url.includes('/github-repos') && url.startsWith('/api/teams/')) {
        return { ok: true, ...jsonBody({ repos: [TRACKED], role: 'admin' }) }
      }
      if (url.startsWith('/api/settings/org')) {
        return { ok: true, ...jsonBody({ github_base_url: 'https://github.com' }) }
      }
      return { ok: true, ...jsonBody({}) }
    })
    vi.stubGlobal('fetch', fetchMock)
    renderRepos()
    await screen.findByText('acme/api')

    fireEvent.click(screen.getByRole('button', { name: /main/ }))
    fireEvent.click(await screen.findByRole('button', { name: 'develop' }))

    expect(await screen.findByText('only the response carries this')).toBeInTheDocument()
  })

  // The branch picker's search is the list route's: the query travels as
  // `search`, and every branch the server answers is offered, including one
  // that does not contain the query — the page never narrows what it holds.
  it('searches branches on the server and offers what it answers', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.startsWith('/api/repos/list')) return { ok: true, ...listBody([repoRow()]) }
      if (url.includes('/branches/list')) {
        return { ok: true, ...listBody([{ name: 'develop' }, { name: 'release/1.0' }]) }
      }
      if (url.includes('/github-repos') && url.startsWith('/api/teams/')) {
        return { ok: true, ...jsonBody({ repos: [TRACKED], role: 'admin' }) }
      }
      if (url.startsWith('/api/settings/org')) {
        return { ok: true, ...jsonBody({ github_base_url: 'https://github.com' }) }
      }
      return { ok: true, ...jsonBody({}) }
    })
    vi.stubGlobal('fetch', fetchMock)
    renderRepos()
    await screen.findByText('acme/api')

    fireEvent.click(screen.getByRole('button', { name: /main/ }))
    fireEvent.change(await screen.findByPlaceholderText('main'), { target: { value: 'rel' } })

    const branchBodies = () =>
      (fetchMock.mock.calls as unknown as [string, RequestInit][])
        .filter(([input]) => input.includes('/branches/list'))
        .map(([, init]) => JSON.parse(String(init.body)))
    await waitFor(() => expect(branchBodies()).toContainEqual({ search: 'rel', page_size: 30 }))
    // `develop` does not contain the query, and is offered anyway: what the
    // server answered for the search is the list.
    expect(await screen.findByRole('button', { name: 'develop' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'release/1.0' })).toBeInTheDocument()
    expect(branchBodies().every((b) => !('q' in b))).toBe(true)
  })

  // The mirror of the above: an event for a repository this page has no row
  // for must not conjure one, and must not land on a different card because
  // some other field happened to match.
  it('ignores a repository_updated frame for an unknown id', async () => {
    vi.stubGlobal('fetch', stubFetch([repoRow()]))
    renderRepos()
    await screen.findByText('acme/api')

    act(() => {
      dispatch?.({
        type: 'repository_updated',
        data: {
          id: 'some-other-repository-id',
          slug: 'acme/api',
          profile_text: 'belongs to a different row',
        },
      } as WSEvent)
    })

    await waitFor(() => {
      expect(screen.queryByText('belongs to a different row')).not.toBeInTheDocument()
    })
  })
})
