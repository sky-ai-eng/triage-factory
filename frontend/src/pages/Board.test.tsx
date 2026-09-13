import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import Board from './Board'
import type { Conversation, Task, TeamMember } from '../types'

// The board's lanes, checked against what it actually reads, and its one
// confirmation, checked against when it asks. Three lanes: the task
// lifecycle and nothing else — a held-but-unstarted task sits in Queued
// wearing its assignee mark, so the Queued read does not narrow to the
// unclaimed the way the rail's count does. Every filter field travels in the
// list body, because a lane holds one page and a client-side pass could only
// narrow what it had already fetched. And returning a task to the queue asks
// only when a run is in flight, since stopping it is the one thing the move
// destroys; with nothing running it fires without a word.

const api = vi.hoisted(() => ({
  apiList: vi.fn(),
  apiJSON: vi.fn(),
  apiFetch: vi.fn(),
  apiErrors: vi.fn(() => [] as unknown[]),
}))
vi.mock('../lib/apiClient', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/apiClient')>()
  return { ...actual, ...api }
})

vi.mock('../hooks/useWebSocket', () => ({
  useWebSocket: () => {},
  setPresenceView: () => {},
}))
const QUEUES = {
  queues: {},
  ingestConversation: () => {},
  dropConversation: () => {},
  refresh: () => {},
  resolve: async () => {},
}
vi.mock('../hooks/usePermissionQueues', () => ({ usePermissionQueues: () => QUEUES }))
const TEAMS = { teams: [{ id: 't1', name: 'platform' }], loaded: true }
const TEAM_FILTER: [string[], () => void] = [[], () => {}]
vi.mock('../hooks/useTeams', () => ({
  useTeams: () => TEAMS,
  useTeamFilter: () => TEAM_FILTER,
}))
vi.mock('../hooks/useDeploymentConfig', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../hooks/useDeploymentConfig')>()
  return { ...actual, useTeamMembers: () => ROSTER }
})
/** The viewer, on the roster, so the picker can recognise "claimed by me". */
const ME = {
  user_id: 'u1',
  display_name: 'Aidan',
  github_username: null,
  jira_account_id: null,
  role: 'member',
  is_current_user: true,
} as TeamMember
const ROSTER = { members: [ME], bot: null }
vi.mock('../hooks/useOrgRole', () => ({ useOrgRole: () => ({ isAdmin: false }) }))

/** The task the board paints into In Progress, held by the viewer. */
const HELD: Task = {
  id: 'task-held',
  source: 'github',
  source_id: 'acme/api#761',
  title: 'Harden the sampler test',
  status: 'in_progress',
  claimed_by_user_id: 'u1',
  event_type: 'github:pr:opened',
  created_at: '2026-09-01T11:00:00Z',
} as Task

/** Its conversation, in whichever state a test puts it. */
function conversation(status: string): Conversation {
  return {
    ID: 'c1',
    TaskID: HELD.id,
    Status: status,
    Model: 'claude-opus-5',
    StartedAt: '2026-09-01T12:00:00Z',
    ResultSummary: status === 'completed' ? 'opened a pull request' : '',
  } as Conversation
}

/** Every list body and every facet body the board sent, in call order. */
let listBodies: Array<Record<string, unknown>> = []
let facetBodies: Array<Record<string, unknown>> = []
let held: Conversation

beforeEach(() => {
  listBodies = []
  facetBodies = []
  held = conversation('completed')
  api.apiList.mockReset()
  api.apiJSON.mockReset()
  api.apiFetch.mockReset()

  api.apiList.mockImplementation(async (path: string, body: Record<string, unknown>) => {
    if (path !== '/api/tasks/list') throw new Error('unexpected apiList ' + path)
    listBodies.push(body)
    const statuses = (body.statuses as string[] | undefined) ?? []
    const items = statuses.includes('in_progress') ? [HELD] : []
    return { items, next_page_token: '', total_count: items.length }
  })
  api.apiJSON.mockImplementation(async (path: string, init?: { body?: string }) => {
    if (path === '/api/me') return { id: 'u1' }
    if (path === '/api/tasks/facets') {
      facetBodies.push(JSON.parse(init?.body ?? '{}') as Record<string, unknown>)
      return { event_types: [{ value: 'github:pr:opened', count: 2 }] }
    }
    if (path === '/api/agent/conversations/list') {
      return { runs: { [HELD.id]: [held] }, messages: {} }
    }
    throw new Error('unexpected apiJSON ' + path)
  })
  api.apiFetch.mockResolvedValue(undefined)
})

function renderBoard() {
  return render(
    <MemoryRouter>
      <Board />
    </MemoryRouter>,
  )
}

/** The lane reads after the mount's three, so a test can say what a gesture
 *  asked for and nothing else. */
function laterBodies(): Array<Record<string, unknown>> {
  return listBodies.slice(3)
}

/** Walks the picker on the held card to its unassign row. The row is found by
 *  its sublabel rather than by role and name: the sortable card wrapper is a
 *  `role="button"` too, and once the picker is open its accessible name
 *  contains the row's text. */
async function unassignHeld() {
  fireEvent.click(await screen.findByRole('button', { name: 'You' }))
  const row = screen.getByText('Click to unassign').closest('button')
  if (!row) throw new Error('unassign row not found')
  fireEvent.click(row)
}

describe('the board lanes', () => {
  it('renders Queued, In Progress and Done — neither Claimed nor In Review', async () => {
    renderBoard()
    for (const title of ['Queued', 'In Progress', 'Done']) {
      expect((await screen.findAllByText(title)).length).toBeGreaterThan(0)
    }
    expect(screen.queryAllByText('Claimed')).toHaveLength(0)
    expect(screen.queryAllByText('In Review')).toHaveLength(0)
  })

  it('asks for three lanes, and the Queued lane keeps the tasks people hold', async () => {
    renderBoard()
    await waitFor(() => expect(listBodies.length).toBe(3))
    const asked = listBodies.flatMap((b) => (b.statuses as string[] | undefined) ?? [])
    expect(asked.sort()).toEqual(['done', 'in_progress', 'queued'])
    // A claimed-but-unstarted task sits in Queued wearing its assignee mark,
    // so the lane is every queued task; only the rail's count narrows to the
    // ones still up for grabs.
    const queued = listBodies.find((b) => (b.statuses as string[]).includes('queued'))
    expect(queued).not.toHaveProperty('only_unclaimed')
  })

  it('draws the filter chips from the lane facet, not from the page', async () => {
    renderBoard()
    await waitFor(() => expect(facetBodies.length).toBe(3))
    // The facet takes the lane and none of the reader's narrowing or paging —
    // the server refuses those by name.
    for (const body of facetBodies) {
      expect(body).not.toHaveProperty('page_size')
      expect(body).not.toHaveProperty('search')
      expect(body).not.toHaveProperty('event_types')
      expect(body).not.toHaveProperty('sort_key')
    }
    // Every lane's panel offers the chip — the In Progress page holds one
    // github:pr:opened task, but the Queued and Done pages hold nothing, and
    // their chips are there only because the facet answered for the lane.
    await waitFor(() =>
      expect(document.querySelectorAll('button[title="github:pr:opened"]')).toHaveLength(3),
    )
  })
})

describe('the lane read runs on the server', () => {
  it('sends the search in the lane body, and only that lane re-reads', async () => {
    renderBoard()
    await waitFor(() => expect(listBodies.length).toBe(3))
    fireEvent.change(await screen.findByLabelText('Search In Progress'), {
      target: { value: 'sampler' },
    })
    await waitFor(() =>
      expect(
        laterBodies().some(
          (b) => (b.statuses as string[])[0] === 'in_progress' && b.search === 'sampler',
        ),
      ).toBe(true),
    )
    expect(laterBodies().every((b) => (b.statuses as string[])[0] === 'in_progress')).toBe(true)
  })

  it('sends a picked sort as sort_key and sort_dir', async () => {
    renderBoard()
    await waitFor(() => expect(listBodies.length).toBe(3))
    // The panels sit in lane order, so the second Newest pill is In Progress's.
    const [, inProgressFilter] = screen.getAllByRole('button', { name: 'Sort & filter' })
    fireEvent.click(inProgressFilter)
    fireEvent.click((await screen.findAllByRole('button', { name: 'Newest' }))[1])
    await waitFor(() =>
      expect(
        laterBodies().some(
          (b) =>
            (b.statuses as string[])[0] === 'in_progress' &&
            b.sort_key === 'created' &&
            b.sort_dir === 'desc',
        ),
      ).toBe(true),
    )
  })
})

describe('returning a task to the queue', () => {
  it('asks first while the run is in flight, and stops it on the press', async () => {
    held = conversation('running')
    renderBoard()
    await unassignHeld()

    expect(
      await screen.findByText('This stops the run. Its work stays with the task.'),
    ).toBeInTheDocument()
    expect(api.apiFetch).not.toHaveBeenCalledWith(`/api/tasks/${HELD.id}/requeue`, {
      method: 'POST',
    })

    fireEvent.click(screen.getByRole('button', { name: 'Return to queue' }))
    await waitFor(() =>
      expect(api.apiFetch).toHaveBeenCalledWith(`/api/tasks/${HELD.id}/requeue`, {
        method: 'POST',
      }),
    )
  })

  it('fires without asking when nothing is running', async () => {
    held = conversation('completed')
    renderBoard()
    // The card itself offers no return-to-queue control: the drop (and the
    // picker's unassign, the same door) is the only route.
    await screen.findByText(HELD.title)
    expect(screen.queryByRole('button', { name: 'Return to queue' })).not.toBeInTheDocument()

    await unassignHeld()
    await waitFor(() =>
      expect(api.apiFetch).toHaveBeenCalledWith(`/api/tasks/${HELD.id}/requeue`, {
        method: 'POST',
      }),
    )
    expect(
      screen.queryByText('This stops the run. Its work stays with the task.'),
    ).not.toBeInTheDocument()
  })
})
