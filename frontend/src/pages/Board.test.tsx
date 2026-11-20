import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, fireEvent, act, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import Board from './Board'
import { HttpError } from '../lib/apiClient'
import type { Conversation, Task, TeamBot, TeamMember } from '../types'

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

/** The board's socket handler, so a test can deliver a push. */
const ws = vi.hoisted(() => ({ handler: null as null | ((event: unknown) => void) }))
vi.mock('../hooks/useWebSocket', () => ({
  useWebSocket: (handler: (event: unknown) => void) => {
    ws.handler = handler
  },
  setPresenceView: () => {},
}))
const QUEUES = {
  queues: {},
  ingestConversation: () => {},
  dropConversation: () => {},
  refresh: () => {},
  resolve: vi.fn(async () => {}),
}
vi.mock('../hooks/usePermissionQueues', () => ({ usePermissionQueues: () => QUEUES }))
/** The viewer's teams. One team by default, which is also local mode. */
const ONE_TEAM = {
  teams: [{ id: 't1', name: 'platform', slug: 'platform' }],
  lastActingTeamId: '',
  multi: false,
  loaded: true,
}
const teamsStore = vi.hoisted(() => ({ current: null as unknown }))
const TEAM_FILTER: [string[], () => void] = [[], () => {}]
vi.mock('../hooks/useTeams', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../hooks/useTeams')>()
  return {
    useTeams: () => teamsStore.current,
    useTeamFilter: () => TEAM_FILTER,
    pickerDefault: actual.pickerDefault,
  }
})
/** The blueprint picker, reduced to what the board hands it and one row to
 *  press: what it lists is its own test's subject. */
const promptPicker = vi.hoisted(() => ({
  props: null as null | { teamValue?: string; teamOptions?: string[] },
}))
vi.mock('../components/PromptPicker', () => ({
  default: (props: {
    open: boolean
    teamValue?: string
    teamOptions?: string[]
    onSelect: (id: string) => void
  }) => {
    if (!props.open) return null
    promptPicker.props = { teamValue: props.teamValue, teamOptions: props.teamOptions }
    return <button onClick={() => props.onSelect('bp-1')}>Run the blueprint</button>
  },
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
const ROSTER: { members: TeamMember[]; bot: TeamBot | null } = {
  members: [ME],
  bot: null,
}
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
  version: 4,
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
  version: 2,
} as Task

/** A task a person dismissed, for the tests that put one in Done. */
const DISMISSED: Task = {
  id: 'task-dismissed',
  source: 'github',
  source_id: 'acme/api#763',
  title: 'Chase the flaky upload',
  status: 'dismissed',
  event_type: 'github:pr:opened',
  created_at: '2026-09-01T09:00:00Z',
  version: 6,
} as Task

/** Its conversation, in whichever state a test puts it. */
function conversation(status: string): Conversation {
  return {
    id: 'c1',
    task_id: HELD.id,
    status,
    model: 'claude-opus-5',
    started_at: '2026-09-01T12:00:00Z',
    result_summary: status === 'completed' ? 'opened a pull request' : '',
  } as Conversation
}

/** Every list body and every facet body the board sent, in call order. */
let listBodies: Array<Record<string, unknown>> = []
let facetBodies: Array<Record<string, unknown>> = []
/** Every task write the board sent, in call order: its path and its body. */
let writes: Array<{ path: string; body: Record<string, unknown> }> = []
let held: Conversation
/** The closed tasks the Done lane answers with, and their conversations. */
let closed: Task[]
let closedRuns: Record<string, Conversation[]>
/** The can_write every listed task carries; undefined leaves the key off. */
let taskCanWrite: boolean | undefined
/** The teams a task's detail read lists for a claim or a delegate. */
let claimTeams: string[]

beforeEach(() => {
  listBodies = []
  facetBodies = []
  writes = []
  held = conversation('completed')
  closed = []
  closedRuns = {}
  taskCanWrite = undefined
  claimTeams = ['t1']
  teamsStore.current = ONE_TEAM
  ROSTER.bot = null
  promptPicker.props = null
  QUEUES.resolve.mockClear()
  api.apiList.mockReset()
  api.apiJSON.mockReset()
  api.apiFetch.mockReset()

  api.apiList.mockImplementation(async (path: string, body: Record<string, unknown>) => {
    if (path !== '/api/tasks/list') throw new Error('unexpected apiList ' + path)
    listBodies.push(body)
    const statuses = (body.statuses as string[] | undefined) ?? []
    const lane = statuses.includes('in_progress')
      ? [HELD]
      : statuses.includes('queued')
        ? [FREE]
        : closed
    const items = lane.map((t) =>
      taskCanWrite === undefined ? t : { ...t, can_write: taskCanWrite },
    )
    return { items, next_page_token: '', total_count: items.length }
  })
  api.apiJSON.mockImplementation(
    async (path: string, init?: { method?: string; body?: string }) => {
      if (path === '/api/me') return { id: 'u1' }
      if (path === '/api/tasks/facets') {
        facetBodies.push(JSON.parse(init?.body ?? '{}') as Record<string, unknown>)
        return { event_types: [{ value: 'github:pr:opened', count: 2 }] }
      }
      if (path === '/api/agent/conversations/list') {
        return { runs: { [HELD.id]: [held], ...closedRuns }, messages: {} }
      }
      // A task write answers the task with the version it left, one past the
      // version the board read, whatever the write named.
      const task = [HELD, FREE].find((t) => path.startsWith(`/api/tasks/${t.id}`))
      // The detail read a claim or a delegate makes first, for its teams.
      if (task && !init?.method) return { ...task, claim_team_ids: claimTeams }
      if (task) {
        writes.push({ path, body: JSON.parse(init?.body ?? '{}') as Record<string, unknown> })
        return { ...task, version: task.version + 1 }
      }
      throw new Error('unexpected apiJSON ' + path)
    },
  )
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

/** Every task write's path, in call order. */
function taskCalls(): string[] {
  return writes.map((w) => w.path)
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

/** Holds every lane read until the test releases it. `held[i]` answers the
 *  i-th read sent, as the unheld stub would have. */
function holdLaneReads(): Array<() => void> {
  const answer = api.apiList.getMockImplementation()!
  const held: Array<() => void> = []
  api.apiList.mockImplementation(
    (path: string, body: Record<string, unknown>) =>
      new Promise((resolve, reject) => {
        held.push(() => {
          answer(path, body).then(resolve, reject)
        })
      }),
  )
  return held
}

describe('the board while it loads', () => {
  it('paints its lanes with placeholder cards in each, until the reads land', async () => {
    const held = holdLaneReads()
    renderBoard()
    await screen.findByLabelText('Search Queued')
    expect(screen.getByRole('status')).toHaveTextContent('Loading board…')
    const lanes = document.querySelectorAll('.bc-col')
    expect(lanes).toHaveLength(3)
    for (const lane of lanes) {
      expect(lane.querySelectorAll('[data-skeleton]').length).toBeGreaterThan(0)
    }

    await act(async () => held.forEach((answer) => answer()))
    await screen.findByText(FREE.title)
    expect(document.querySelectorAll('[data-skeleton]')).toHaveLength(0)
    expect(screen.queryByRole('status')).toBeNull()
  })

  it('opens its lanes as the viewer left them, so none collapses after it paints', async () => {
    localStorage.setItem('sky330.board.collapsed.v1.u1', JSON.stringify({ done: true }))
    try {
      const answer = api.apiJSON.getMockImplementation()!
      let me: () => void = () => {}
      const meHeld = new Promise<void>((resolve) => (me = resolve))
      api.apiJSON.mockImplementation(async (path: string, init?: unknown) => {
        if (path === '/api/me') await meHeld
        return answer(path, init)
      })
      renderBoard()
      await waitFor(() => expect(listBodies.length).toBe(3))
      expect(screen.queryByLabelText('Search Queued')).toBeNull()

      await act(async () => me())
      // The first paint with lanes in it already has Done collapsed.
      await screen.findByLabelText('Search Queued')
      expect(screen.queryByLabelText('Search Done')).toBeNull()
      expect(screen.getByRole('button', { name: 'Expand Done' })).toBeInTheDocument()
    } finally {
      localStorage.removeItem('sky330.board.collapsed.v1.u1')
    }
  })

  it('paints no lanes before the teams load, so a viewer on no team never sees one', async () => {
    teamsStore.current = { ...ONE_TEAM, teams: [], loaded: false }
    const { rerender } = renderBoard()
    await waitFor(() => expect(api.apiJSON).toHaveBeenCalledWith('/api/me'))
    await waitFor(() => expect(listBodies.length).toBe(3))
    expect(screen.queryByLabelText('Search Queued')).toBeNull()

    teamsStore.current = ONE_TEAM
    rerender(
      <MemoryRouter>
        <Board />
      </MemoryRouter>,
    )
    await screen.findByText(FREE.title)
  })

  it('schedules no frames while a viewer on no team sees the empty state', async () => {
    teamsStore.current = { ...ONE_TEAM, teams: [], loaded: true }
    const frames = vi.spyOn(window, 'requestAnimationFrame')
    try {
      renderBoard()
      await screen.findByText(/not on a team yet/)
      await waitFor(() => expect(listBodies.length).toBe(3))
      await act(async () => new Promise((resolve) => setTimeout(resolve, 100)))
      expect(frames).not.toHaveBeenCalled()
    } finally {
      frames.mockRestore()
    }
  })

  it("keeps a lane's placeholders while a newer read stands in for its first", async () => {
    const held = holdLaneReads()
    renderBoard()
    await screen.findByLabelText('Search Queued')
    const queued = document.querySelectorAll('.bc-col')[0] as HTMLElement
    fireEvent.click(within(queued).getByRole('button', { name: 'Sort & filter' }))
    fireEvent.click(within(queued).getByRole('button', { name: 'Show snoozed' }))
    await waitFor(() => expect(held).toHaveLength(4))

    // The mount reads land; Queued's is discarded, since the snoozed read
    // replaced it, and that one is still out.
    await act(async () => held.slice(0, 3).forEach((answer) => answer()))
    await screen.findByText(HELD.title)
    expect(within(queued).queryByText('Queue is empty')).toBeNull()
    expect(queued.querySelectorAll('[data-skeleton]').length).toBeGreaterThan(0)

    await act(async () => held[3]())
    await within(queued).findByText(FREE.title)
    expect(queued.querySelectorAll('[data-skeleton]')).toHaveLength(0)
  })

  it('says a lane could not load when its first read fails, not that it is empty', async () => {
    const answer = api.apiList.getMockImplementation()!
    api.apiList.mockImplementation(async (path: string, body: Record<string, unknown>) => {
      if ((body.statuses as string[]).includes('queued')) {
        listBodies.push(body)
        throw new HttpError(500, '')
      }
      return answer(path, body)
    })
    renderBoard()
    const queued = (await screen.findByLabelText('Search Queued')).closest('.bc-col') as HTMLElement
    expect(await within(queued).findByRole('alert')).toHaveTextContent('Could not load the queue.')
    expect(within(queued).queryByText('Queue is empty')).toBeNull()
    expect(queued.querySelectorAll('[data-skeleton]')).toHaveLength(0)
    // The lanes whose reads landed paint as usual.
    await screen.findByText(HELD.title)
  })

  it('keeps the cards a lane holds when a refresh fails', async () => {
    renderBoard()
    const queued = (await screen.findByLabelText('Search Queued')).closest('.bc-col') as HTMLElement
    await within(queued).findByText(FREE.title)

    api.apiList.mockRejectedValue(new HttpError(500, ''))
    await act(async () => ws.handler?.({ type: 'tasks_updated' }))
    await waitFor(() => expect(api.apiList.mock.calls.length).toBeGreaterThanOrEqual(6))
    await act(async () => {})
    expect(within(queued).getByText(FREE.title)).toBeInTheDocument()
    expect(within(queued).queryByRole('alert')).toBeNull()
  })
})

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
    expect(asked.sort()).toEqual(['dismissed', 'done', 'in_progress', 'queued'])
    // Done is every closed task, completed or dismissed, inside its window.
    const done = listBodies.find((b) => (b.statuses as string[]).includes('done'))
    expect(done?.statuses).toEqual(['done', 'dismissed'])
    expect(done?.closed_since).toEqual(expect.any(String))
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

describe('the lane kind filter', () => {
  // The facet stub answers every lane with two kinds, so each lane's panel
  // has a choice to offer.
  function facetTwoKinds() {
    const base = api.apiJSON.getMockImplementation()!
    api.apiJSON.mockImplementation(async (path: string, init?: { body?: string }) => {
      if (path === '/api/tasks/facets') {
        facetBodies.push(JSON.parse(init?.body ?? '{}') as Record<string, unknown>)
        return {
          event_types: [{ value: 'github:pr:opened', count: 2 }],
          entity_kinds: [
            { value: 'pr', count: 1 },
            { value: 'vulnerable_package', count: 1 },
          ],
        }
      }
      return base(path, init)
    })
  }

  it('offers no kind choice for a lane of one kind', async () => {
    renderBoard()
    await screen.findByText(FREE.title)
    fireEvent.click(screen.getAllByRole('button', { name: 'Sort & filter' })[1])
    expect(await screen.findAllByRole('button', { name: 'Newest' })).not.toHaveLength(0)
    expect(screen.queryByRole('button', { name: 'Vulnerable packages' })).not.toBeInTheDocument()
  })

  it('sends a picked kind as entity_kinds on the lane and its facet', async () => {
    facetTwoKinds()
    renderBoard()
    await screen.findByText(FREE.title)
    fireEvent.click(screen.getAllByRole('button', { name: 'Sort & filter' })[1])
    // The panels sit in lane order, so the second pill is In Progress's.
    fireEvent.click((await screen.findAllByRole('button', { name: 'Vulnerable packages' }))[1])
    const picked = (b: Record<string, unknown>) =>
      (b.statuses as string[])[0] === 'in_progress' &&
      JSON.stringify(b.entity_kinds) === JSON.stringify(['vulnerable_package'])
    await waitFor(() => expect(laterBodies().some(picked)).toBe(true))
    await waitFor(() => expect(facetBodies.some(picked)).toBe(true))
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
    // The filter panels render with the lanes, once every lane has answered.
    await screen.findByText(FREE.title)
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

/** The conversations the board's cards link to, one entry per distinct id. */
function runLinks(): string[] {
  const ids = Array.from(document.querySelectorAll('a[href*="/runs/"]')).map(
    (a) => (a.getAttribute('href') ?? '').split('/runs/')[1],
  )
  return [...new Set(ids)]
}

describe('the conversation a card shows', () => {
  it("keeps the run a person took over when their claim's conversation has not run", async () => {
    // The list answers newest first, and the claim's conversation is newer
    // than the run it took the task from.
    const claimed = {
      ...conversation('open'),
      id: 'c-claim',
      started_at: '2026-09-01T13:00:00Z',
      type: 'interactive',
      has_run: false,
    } as Conversation
    api.apiJSON.mockImplementation(async (path: string) => {
      if (path === '/api/me') return { id: 'u1' }
      if (path === '/api/tasks/facets') return { event_types: [] }
      if (path === '/api/agent/conversations/list') {
        return { runs: { [HELD.id]: [claimed, held] }, messages: {} }
      }
      throw new Error('unexpected apiJSON ' + path)
    })
    renderBoard()
    await screen.findByText(HELD.title)

    expect(runLinks()).toEqual([held.id])
  })

  it("shows a claim's conversation once it has run", async () => {
    const claimed = {
      ...conversation('open'),
      id: 'c-claim',
      started_at: '2026-09-01T13:00:00Z',
      type: 'interactive',
      has_run: true,
    } as Conversation
    api.apiJSON.mockImplementation(async (path: string) => {
      if (path === '/api/me') return { id: 'u1' }
      if (path === '/api/tasks/facets') return { event_types: [] }
      if (path === '/api/agent/conversations/list') {
        return { runs: { [HELD.id]: [claimed, held] }, messages: {} }
      }
      throw new Error('unexpected apiJSON ' + path)
    })
    renderBoard()
    await screen.findByText(HELD.title)

    expect(runLinks()).toEqual(['c-claim'])
  })

  it('offers "Start chat" only to the holder, and only while their conversation has not run', async () => {
    // The holder took the task over: the card keeps the agent's run, and the
    // row leads to the conversation the claim opened.
    async function startChatHref(holder: string, hasRun: boolean): Promise<string | null> {
      const task = { ...HELD, claimed_by_user_id: holder, conversation_id: 'c-claim' }
      const claimed = {
        ...conversation('open'),
        id: 'c-claim',
        started_at: '2026-09-01T13:00:00Z',
        type: 'interactive',
        has_run: hasRun,
      } as Conversation
      api.apiList.mockImplementation(async (_path: string, body: Record<string, unknown>) => {
        const statuses = (body.statuses as string[] | undefined) ?? []
        const items = statuses.includes('in_progress') ? [task] : []
        return { items, next_page_token: '', total_count: items.length }
      })
      api.apiJSON.mockImplementation(async (path: string) => {
        if (path === '/api/me') return { id: 'u1' }
        if (path === '/api/tasks/facets') return { event_types: [] }
        if (path === '/api/agent/conversations/list') {
          return { runs: { [HELD.id]: [claimed, held] }, messages: {} }
        }
        throw new Error('unexpected apiJSON ' + path)
      })
      const view = renderBoard()
      await screen.findByText(HELD.title)
      // The viewer's identity lands after the first paint.
      await waitFor(() => expect(api.apiJSON).toHaveBeenCalledWith('/api/me'))
      await act(async () => {})
      const href = screen.queryByRole('link', { name: 'Start chat' })?.getAttribute('href') ?? null
      view.unmount()
      return href
    }

    expect(await startChatHref('u1', false)).toMatch(/\/runs\/c-claim$/)
    expect(await startChatHref('u2', false)).toBeNull()
    expect(await startChatHref('u1', true)).toBeNull()
  })

  it('keeps "Start chat" on the live chat when an ended one that never ran is pushed', async () => {
    // A chat that failed before its model spoke, and the replacement opened
    // for its holder: the failure's push must not take the offer from it.
    const task = { ...HELD, conversation_id: 'c-new' }
    const live = {
      ...conversation('open'),
      id: 'c-new',
      started_at: '2026-09-01T13:00:00Z',
      type: 'interactive',
      has_run: false,
    } as Conversation
    const failed = {
      ...conversation('failed'),
      id: 'c-failed',
      started_at: '2026-09-01T12:30:00Z',
      type: 'interactive',
      has_run: false,
      ended_at: '2026-09-01T12:59:00Z',
      ended_reason: 'failed',
    } as Conversation
    api.apiList.mockImplementation(async (_path: string, body: Record<string, unknown>) => {
      const statuses = (body.statuses as string[] | undefined) ?? []
      const items = statuses.includes('in_progress') ? [task] : []
      return { items, next_page_token: '', total_count: items.length }
    })
    api.apiJSON.mockImplementation(async (path: string) => {
      if (path === '/api/me') return { id: 'u1' }
      if (path === '/api/tasks/facets') return { event_types: [] }
      if (path === '/api/agent/conversations/list') {
        return { runs: { [HELD.id]: [live, held] }, messages: {} }
      }
      if (path === '/api/agent/conversations/c-failed') return failed
      throw new Error('unexpected apiJSON ' + path)
    })
    renderBoard()
    await screen.findByRole('link', { name: 'Start chat' })

    await act(async () => {
      ws.handler?.({
        type: 'conversation_update',
        conversation_id: 'c-failed',
        data: { status: 'failed' },
      })
    })
    expect(screen.getByRole('link', { name: 'Start chat' })).toHaveAttribute(
      'href',
      expect.stringMatching(/\/runs\/c-new$/),
    )
  })
})

describe('starting a queued task', () => {
  it('makes the drag one call: the claim is what moves the card', async () => {
    renderBoard()
    await screen.findByText(FREE.title)

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
    // It names the version the lane read.
    expect(writes[0].body).toEqual({ version: FREE.version })
    // The card left Queued, so every lane is stale — the picker refetches all
    // three rather than patching the one it thinks it moved.
    await waitFor(() => expect(laterBodies().length).toBeGreaterThanOrEqual(3))
  })

  it('names the version its last write returned on the next one', async () => {
    renderBoard()
    await screen.findByText(FREE.title)

    await act(async () => {
      await dnd.drop!(FREE.id, 'in_progress')
    })
    // The lanes still list the version the first write moved past; the claim
    // answered one past it, and that is what the next write sends.
    const picker = await openPicker('Unassigned')
    fireEvent.click(picker.getByRole('option', { name: ME.display_name }))
    await waitFor(() => expect(writes.length).toBe(2))
    expect(writes.map((w) => w.body.version)).toEqual([FREE.version, FREE.version + 1])
  })
})

describe('the team a claim or a delegate lands on', () => {
  function onTwoTeams() {
    teamsStore.current = {
      teams: [
        { id: 't1', name: 'platform', slug: 'platform' },
        { id: 't2', name: 'data', slug: 'data' },
      ],
      lastActingTeamId: '',
      multi: true,
      loaded: true,
    }
  }

  it('sends the one team that can take the task, without asking', async () => {
    onTwoTeams()
    claimTeams = ['t2']
    renderBoard()
    await screen.findByText(FREE.title)

    await act(async () => {
      await dnd.drop!(FREE.id, 'in_progress')
    })

    expect(writes).toEqual([
      { path: `/api/tasks/${FREE.id}/claim`, body: { team_id: 't2', version: FREE.version } },
    ])
    expect(screen.queryByText('Which team takes this task?')).not.toBeInTheDocument()
  })

  it('asks which team when two can take the task, and claims for the one chosen', async () => {
    onTwoTeams()
    claimTeams = ['t1', 't2']
    renderBoard()
    await screen.findByText(FREE.title)

    await act(async () => {
      await dnd.drop!(FREE.id, 'in_progress')
    })
    // The drop wrote nothing: the claim waits on the answer.
    await screen.findByText('Which team takes this task?')
    expect(writes).toEqual([])

    fireEvent.click(screen.getByRole('radio', { name: 'data' }))
    fireEvent.click(screen.getByRole('button', { name: 'Claim' }))

    await waitFor(() =>
      expect(writes).toEqual([
        { path: `/api/tasks/${FREE.id}/claim`, body: { team_id: 't2', version: FREE.version } },
      ]),
    )
  })

  it('opens the blueprint picker on the team the task lands on, and delegates for it', async () => {
    onTwoTeams()
    claimTeams = ['t2']
    ROSTER.bot = { agent_id: 'a1', display_name: 'Tess', enabled: true, model: '', autonomy: null }
    renderBoard()
    await waitFor(() => expect(listBodies.length).toBe(3))

    const picker = await openPicker('Unassigned')
    fireEvent.click(picker.getByRole('option', { name: /Tess/ }))

    // One candidate: the picker lists that team's blueprints and offers no
    // team to choose.
    fireEvent.click(await screen.findByRole('button', { name: 'Run the blueprint' }))
    expect(promptPicker.props).toEqual({ teamValue: 't2', teamOptions: undefined })
    await waitFor(() =>
      expect(writes).toEqual([
        {
          path: `/api/tasks/${FREE.id}/delegate`,
          body: { blueprint_id: 'bp-1', team_id: 't2', version: FREE.version },
        },
      ]),
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
    expect(taskCalls()).toEqual([])

    fireEvent.click(screen.getByRole('button', { name: 'Return to queue' }))
    await waitFor(() =>
      expect(writes).toEqual([
        { path: `/api/tasks/${HELD.id}/requeue`, body: { version: HELD.version } },
      ]),
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
      expect(writes).toEqual([
        { path: `/api/tasks/${HELD.id}/requeue`, body: { version: HELD.version } },
      ]),
    )
    expect(
      screen.queryByText('This stops the run. Its work stays with the task.'),
    ).not.toBeInTheDocument()
  })
})

describe('ending a task', () => {
  it('completes a task whose run left deliverables without asking', async () => {
    held = {
      ...conversation('completed'),
      has_unresolved_deliverables: true,
      pending_deliverable_ids: ['d1', 'd2'],
      unresolved_pr_count: 1,
      unresolved_review_count: 1,
    } as Conversation
    renderBoard()
    await screen.findByText(HELD.title)

    await act(async () => {
      await dnd.drop!(HELD.id, 'done')
    })

    // One call, the task's own close: no dialog stands in front of it, and no
    // deliverable route is reached, because a close leaves them all as they are.
    expect(writes).toEqual([
      { path: `/api/tasks/${HELD.id}`, body: { status: 'done', version: HELD.version } },
    ])
    const reached = [...api.apiFetch.mock.calls, ...api.apiJSON.mock.calls].map((c) => String(c[0]))
    expect(reached.some((p) => p.startsWith('/api/deliverables/'))).toBe(false)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('dismisses a queued task without asking', async () => {
    renderBoard()
    await screen.findByText(FREE.title)

    await act(async () => {
      await dnd.drop!(FREE.id, 'done')
    })

    expect(writes).toEqual([
      { path: `/api/tasks/${FREE.id}`, body: { status: 'dismissed', version: FREE.version } },
    ])
  })
})

describe('a permission prompt on a card', () => {
  afterEach(() => {
    ;(QUEUES as { queues: Record<string, unknown[]> }).queues = {}
  })

  function parkOnPrompt(canWrite: boolean) {
    held = { ...conversation('running'), can_write: canWrite }
    ;(QUEUES as { queues: Record<string, unknown[]> }).queues = {
      [held.id]: [{ tool_call_id: 'toolu_1', tool_name: 'Bash', input: { command: 'make test' } }],
    }
  }

  it('offers Allow and Deny to a caller who may answer', async () => {
    parkOnPrompt(true)
    renderBoard()
    expect(await screen.findByText('make test')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Allow' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Deny' })).toBeInTheDocument()
  })

  it('shows the ask without the verbs to a caller who may not', async () => {
    parkOnPrompt(false)
    renderBoard()
    expect(await screen.findByText('make test')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Allow' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Deny' })).not.toBeInTheDocument()
  })
})

// A close ends no run, so a card in Done can carry one that is working or
// waiting on an answer, and it shows that run the way an In Progress card does.
describe('a closed task in Done', () => {
  afterEach(() => {
    ;(QUEUES as { queues: Record<string, unknown[]> }).queues = {}
  })

  function card(title: string) {
    return screen.getByText(title).closest('[aria-roledescription="sortable"]') as HTMLElement
  }

  function running(): Conversation {
    return {
      ...conversation('running'),
      id: 'c-dismissed',
      task_id: DISMISSED.id,
      current_action: 'Reading the upload test',
      can_write: true,
    }
  }

  it('renders a dismissed task with no run as dismissed: not queued, and not completed', async () => {
    closed = [
      DISMISSED,
      { ...DISMISSED, id: 'task-completed', title: 'Pin the base image', status: 'done' },
    ]
    renderBoard()
    expect(await screen.findByText(DISMISSED.title)).toBeInTheDocument()
    expect(card(DISMISSED.title).querySelector('[data-mark]')).toHaveAttribute(
      'data-mark',
      'dismissed',
    )
    expect(card('Pin the base image').querySelector('[data-mark]')).toHaveAttribute(
      'data-mark',
      'done',
    )
  })

  it("draws the dismissal over a dismissed task's finished run", async () => {
    closed = [DISMISSED]
    closedRuns = {
      [DISMISSED.id]: [
        {
          ...conversation('open'),
          id: 'c-dismissed',
          task_id: DISMISSED.id,
          completed_at: '2026-09-01T12:04:00Z',
          duration_ms: 240_000,
        },
      ],
    }
    renderBoard()
    await screen.findByText(DISMISSED.title)
    expect(
      await within(card(DISMISSED.title)).findByRole('link', { name: /^Ran for / }),
    ).toBeInTheDocument()
    expect(card(DISMISSED.title).querySelector('[data-mark]')).toHaveAttribute(
      'data-mark',
      'dismissed',
    )
  })

  it("shows a dismissed task's working run and what it is doing", async () => {
    closed = [DISMISSED]
    closedRuns = { [DISMISSED.id]: [running()] }
    renderBoard()
    expect(await screen.findByText('Reading the upload test')).toBeInTheDocument()
    expect(card(DISMISSED.title).querySelector('[data-mark]')).toHaveAttribute(
      'data-mark',
      'crates',
    )
  })

  it('answers a pending permission with Allow and with Deny', async () => {
    const run = running()
    closed = [DISMISSED]
    closedRuns = { [DISMISSED.id]: [run] }
    ;(QUEUES as { queues: Record<string, unknown[]> }).queues = {
      [run.id]: [{ tool_call_id: 'toolu_9', tool_name: 'Bash', input: { command: 'make lint' } }],
    }
    renderBoard()
    expect(await screen.findByText('make lint')).toBeInTheDocument()

    await act(async () => {
      fireEvent.click(within(card(DISMISSED.title)).getByRole('button', { name: 'Allow' }))
    })
    await act(async () => {
      fireEvent.click(within(card(DISMISSED.title)).getByRole('button', { name: 'Deny' }))
    })
    expect(QUEUES.resolve.mock.calls).toEqual([
      [run.id, 'toolu_9', { behavior: 'allow' }],
      [run.id, 'toolu_9', { behavior: 'deny' }],
    ])
  })
})

describe('a card the caller may not write', () => {
  /** The sortable wrapper the card's drag rides on. */
  function dragHandle(title: string) {
    return screen.getByText(title).closest('[aria-roledescription="sortable"]')
  }

  it('keeps the assignee mark and drops the picker and the drag', async () => {
    taskCanWrite = false
    renderBoard()
    expect(await screen.findByText(FREE.title)).toBeInTheDocument()
    expect(screen.getByText(HELD.title)).toBeInTheDocument()

    // The marks still say who holds what; neither opens a picker.
    expect(screen.getByTitle('Unassigned')).toBeInTheDocument()
    expect(screen.getByTitle('Aidan')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Unassigned' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Assigned to Aidan' })).not.toBeInTheDocument()

    expect(dragHandle(FREE.title)).toHaveAttribute('aria-disabled', 'true')
    expect(dragHandle(HELD.title)).toHaveAttribute('aria-disabled', 'true')
  })

  it('claims and requeues nothing when its card is dropped', async () => {
    taskCanWrite = false
    renderBoard()
    await screen.findByText(FREE.title)

    await act(async () => {
      await dnd.drop!(FREE.id, 'in_progress')
      await dnd.drop!(HELD.id, 'queued')
    })
    expect(taskCalls()).toEqual([])
    expect(
      screen.queryByText('This stops the run. Its work stays with the task.'),
    ).not.toBeInTheDocument()
  })

  it('offers the picker and the drag to a writer', async () => {
    taskCanWrite = true
    renderBoard()
    expect(await screen.findByRole('button', { name: 'Unassigned' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Assigned to Aidan' })).toBeInTheDocument()
    expect(dragHandle(FREE.title)).toHaveAttribute('aria-disabled', 'false')

    await act(async () => {
      await dnd.drop!(FREE.id, 'in_progress')
    })
    expect(taskCalls()).toEqual([`/api/tasks/${FREE.id}/claim`])
  })
})
