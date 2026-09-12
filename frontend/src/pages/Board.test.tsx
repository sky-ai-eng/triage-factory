import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import Board from './Board'
import type { Conversation, Task } from '../types'

// The board's lanes, checked against what it actually reads. `in_review` left
// the task status vocabulary, so the lane it named cannot fill and a drag into
// it would be a refused PATCH — the board asks for four lanes and offers four
// drop targets. And returning a task to the queue no longer resolves its
// artifacts, so the confirmation that promised it is gone from that path while
// the one on completing the task stays.

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
const ROSTER = { members: [] as never[], bot: null }
vi.mock('../hooks/useOrgRole', () => ({ useOrgRole: () => ({ isAdmin: false }) }))

/** The task the board paints into In Progress, carrying a draft PR. */
const CARRYING: Task = {
  id: 'task-carrying',
  source: 'github',
  source_id: 'acme/api#761',
  title: 'Harden the sampler test',
  status: 'in_progress',
  claimed_by_user_id: 'u1',
} as Task

/** Its conversation: settled, still holding one unresolved draft PR. */
const CONVERSATION: Conversation = {
  ID: 'c1',
  TaskID: CARRYING.id,
  Status: 'completed',
  Model: 'claude-opus-5',
  StartedAt: '2026-09-01T12:00:00Z',
  ResultSummary: 'opened a pull request',
  has_unresolved_artifacts: true,
  unresolved_pr_count: 1,
  unresolved_review_count: 0,
  pending_artifact_ids: ['a1'],
} as Conversation

/** Every list body the board sent, in call order. */
let listBodies: Array<Record<string, unknown>> = []

beforeEach(() => {
  listBodies = []
  api.apiList.mockReset()
  api.apiJSON.mockReset()
  api.apiFetch.mockReset()

  api.apiList.mockImplementation(async (path: string, body: Record<string, unknown>) => {
    if (path !== '/api/tasks/list') throw new Error('unexpected apiList ' + path)
    listBodies.push(body)
    const statuses = (body.statuses as string[] | undefined) ?? []
    const items = statuses.includes('in_progress') ? [CARRYING] : []
    return { items, next_page_token: '', total_count: items.length }
  })
  api.apiJSON.mockImplementation(async (path: string) => {
    if (path === '/api/agent/conversations/list') {
      return { runs: { [CARRYING.id]: [CONVERSATION] }, messages: {} }
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

describe('the board lanes', () => {
  it('renders Queued, Claimed, In Progress and Done — and no In Review', async () => {
    renderBoard()
    // Two lanes render collapsed to a rail, whose label is its own node, so
    // a lane can legitimately name itself twice.
    for (const title of ['Queued', 'Claimed', 'In Progress', 'Done']) {
      expect((await screen.findAllByText(title)).length).toBeGreaterThan(0)
    }
    expect(screen.queryAllByText('In Review')).toHaveLength(0)
    expect(screen.queryByText('Nothing in review')).not.toBeInTheDocument()
  })

  it('asks for four lanes, none of them in_review', async () => {
    renderBoard()
    await waitFor(() => expect(listBodies.length).toBe(4))
    const asked = listBodies.flatMap((b) => (b.statuses as string[] | undefined) ?? [])
    expect(asked).not.toContain('in_review')
    // One read per lane, and the lane vocabulary is what the server still
    // accepts: the queue projection ('queued' narrowed by only_unclaimed),
    // the claim axis, and the two lifecycle lanes that remain.
    expect(asked.sort()).toEqual(['claimed', 'done', 'in_progress', 'queued'])
  })
})

describe('returning a task to the queue', () => {
  it('fires straight away even when the task carries unresolved artifacts', async () => {
    renderBoard()

    const requeue = await screen.findByRole('button', { name: 'Return to queue' })
    fireEvent.click(requeue)

    await waitFor(() =>
      expect(api.apiFetch).toHaveBeenCalledWith(`/api/tasks/${CARRYING.id}/requeue`, {
        method: 'POST',
      }),
    )
    // The resolve-all dialog gates completing the task, not requeueing it —
    // completing is what ends a task and resolves what it holds, while a
    // requeue is meant to be undone by re-claiming. See the TFAC-990 note at
    // the requeue drag for the window where the server has not caught up.
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    expect(screen.queryByText('Resolve open artifacts?')).not.toBeInTheDocument()
  })
})
