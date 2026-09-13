import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, fireEvent, act, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import Board from './Board'
import type { Conversation, Task, TeamMember } from '../types'

// The board's lanes, checked against what it actually reads; the two gestures
// that move a card into In Progress; and its one confirmation, checked against
// when it asks. Three lanes: the task lifecycle and nothing else — and the
// Queued lane holds nobody's work, because assigning a task is what starts it,
// so the lane needs no `only_unclaimed` narrowing to be the pickable set.
// Every filter field travels in the list body, because a lane holds one page
// and a client-side pass could only narrow what it had already fetched. And
// returning a task to the queue asks only when a run is in flight, since
// stopping it is the one thing the move destroys; with nothing running it
// fires without a word.

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

/** The drop, reached without a pointer. Only DndContext is replaced — with a
 *  passthrough that hands the board's own onDragEnd to the test — so the
 *  columns still register as drop targets and the cards still render. */
const dnd = vi.hoisted(() => ({ drop: null as null | ((taskId: string, col: string) => unknown) }))
vi.mock('@dnd-kit/core', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@dnd-kit/core')>()
  return {
    ...actual,
    DndContext: ({
      children,
      onDragEnd,
    }: {
      children: React.ReactNode
      onDragEnd: (e: unknown) => unknown
    }) => {
      dnd.drop = (taskId, col) => onDragEnd({ active: { id: taskId }, over: { id: col } })
      return children
    },
  }
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

/** The task the board paints into Queued: nobody holds it, because nobody can
 *  hold a queued task. */
const FREE: Task = {
  id: 'task-free',
  source: 'github',
  source_id: 'acme/api#762',
  title: 'Retire the legacy shim',
  status: 'queued',
  event_type: 'github:pr:opened',
  created_at: '2026-09-01T10:00:00Z',
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
    const items = statuses.includes('in_progress')
      ? [HELD]
      : statuses.includes('queued')
        ? [FREE]
        : []
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
  dnd.drop = null
})

afterEach(() => {
  dnd.drop = null
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

/** Opens one card's assignee picker and returns a scope over it — every card
 *  on the board has one, and their rows carry the same names, so a row has to
 *  be chosen inside the picker that owns it. The mark names its holder, and
 *  the rows are its listbox's options. */
async function openPicker(markLabel: string) {
  const mark = await screen.findByRole('button', { name: markLabel })
  fireEvent.click(mark)
  return within(mark.parentElement as HTMLElement)
}

/** Walks the picker on the held card to its Unassign row. */
async function unassignHeld() {
  const picker = await openPicker('Assigned to Aidan')
  fireEvent.click(picker.getByRole('option', { name: 'Unassign' }))
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

  it('asks for three lanes, and the Queued lane asks for the whole queue', async () => {
    renderBoard()
    await waitFor(() => expect(listBodies.length).toBe(3))
    const asked = listBodies.flatMap((b) => (b.statuses as string[] | undefined) ?? [])
    expect(asked.sort()).toEqual(['done', 'in_progress', 'queued'])
    // The queue holds nobody's work — assigning a task lands it in In Progress
    // — so the lane needs no claim narrowing to be the pickable set.
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

describe('starting a queued task', () => {
  /** Every /api/tasks/… path the board asked for, in call order. */
  function taskCalls(): string[] {
    return api.apiFetch.mock.calls
      .map((c) => String(c[0]))
      .filter((p) => p.startsWith('/api/tasks/'))
  }

  it('makes the drag one call: the claim is what moves the card', async () => {
    renderBoard()
    await waitFor(() => expect(listBodies.length).toBe(3))

    await act(async () => {
      await dnd.drop!(FREE.id, 'in_progress')
    })

    // One call, and it is the claim — no second write staging the row, because
    // the server lands it in progress in the same UPDATE that takes the claim.
    expect(taskCalls()).toEqual([`/api/tasks/${FREE.id}/claim`])
  })

  it('claims from the picker on a Queued card, then re-reads every lane', async () => {
    renderBoard()
    await waitFor(() => expect(listBodies.length).toBe(3))

    const picker = await openPicker('Unassigned')
    fireEvent.click(picker.getByRole('option', { name: ME.display_name }))

    await waitFor(() => expect(taskCalls()).toEqual([`/api/tasks/${FREE.id}/claim`]))
    // The card left Queued, so every lane is stale — the picker refetches all
    // three rather than patching the one it thinks it moved.
    await waitFor(() => expect(laterBodies().length).toBeGreaterThanOrEqual(3))
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
