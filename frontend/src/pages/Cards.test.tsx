import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import Cards from './Cards'
import type { Task } from '../types'
import { HttpError } from '../lib/apiClient'

// The deck's writes name the version they act on: a swipe sends the version
// the deck read, and the undo toast's requeue sends the version the swipe's
// write returned, so an undo of a task that changed since the swipe is
// refused instead of requeueing over the change.

const api = vi.hoisted(() => ({ apiList: vi.fn(), apiJSON: vi.fn() }))
vi.mock('../lib/apiClient', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/apiClient')>()
  return { ...actual, ...api }
})
const toast = vi.hoisted(() => ({ failed: vi.fn(), error: vi.fn() }))
vi.mock('../components/Toast/toastStore', () => ({ toast }))
vi.mock('../hooks/useWebSocket', () => ({ useWebSocket: () => {} }))
const TEAM_FILTER: [string[], () => void] = [[], () => {}]
/** The viewer's teams. One team by default, which is also local mode. */
const teamsStore = vi.hoisted(() => ({
  current: {
    teams: [{ id: 't1', name: 'platform', slug: 'platform' }],
    lastActingTeamId: '',
    multi: false,
    loaded: true,
  },
}))
vi.mock('../hooks/useTeams', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../hooks/useTeams')>()
  return {
    useTeamFilter: () => TEAM_FILTER,
    useTeams: () => teamsStore.current,
    pickerDefault: actual.pickerDefault,
  }
})
vi.mock('../components/TeamScopeSelect', () => ({ default: () => null }))
vi.mock('../components/TaskRulesPanel', () => ({ default: () => null }))
vi.mock('../components/NotifyRulesPanel', () => ({ default: () => null }))
vi.mock('../components/PromptPicker', () => ({ default: () => null }))

const CARD: Task = {
  id: 'task-top',
  source: 'github',
  source_id: 'acme/api#762',
  title: 'Retire the legacy shim',
  status: 'queued',
  event_type: 'github:pr:opened',
  created_at: '2026-09-01T10:00:00Z',
  version: 2,
} as Task

/** The card behind it, so the deck still holds one once the top card is
 *  swiped away: an empty deck renders no undo toast. */
const NEXT: Task = { ...CARD, id: 'task-next', title: 'Pin the base image', version: 9 }

/** Every task write the deck sent, in call order. */
let writes: Array<{ path: string; method?: string; body: Record<string, unknown> }> = []
/** What the requeue answers: the task, or a refusal to throw. */
let requeueAnswer: () => unknown
/** The teams the top card's detail read lists for a claim. */
let claimTeams: string[]

beforeEach(() => {
  writes = []
  claimTeams = ['t1']
  teamsStore.current = {
    teams: [{ id: 't1', name: 'platform', slug: 'platform' }],
    lastActingTeamId: '',
    multi: false,
    loaded: true,
  }
  toast.failed.mockReset()
  toast.error.mockReset()
  requeueAnswer = () => ({ ...CARD, version: 4 })
  api.apiList.mockReset()
  api.apiJSON.mockReset()
  api.apiList.mockResolvedValue({ items: [CARD, NEXT], next_page_token: '', total_count: 2 })
  api.apiJSON.mockImplementation(
    async (path: string, init?: { method?: string; body?: string }) => {
      // The page's other reads (its config, among them) are not what this is
      // about.
      if (!path.startsWith('/api/tasks/')) return {}
      // The detail read a claim or delegate makes first, for its teams.
      if (!init?.method) return { ...CARD, claim_team_ids: claimTeams }
      writes.push({
        path,
        method: init?.method,
        body: JSON.parse(init?.body ?? '{}') as Record<string, unknown>,
      })
      if (path === `/api/tasks/${CARD.id}/requeue`) return requeueAnswer()
      // The dismiss answers the task as it left it: one version past the read.
      return { ...CARD, status: 'dismissed', version: 3 }
    },
  )
})

async function dismissTopCard() {
  render(
    <MemoryRouter>
      <Cards />
    </MemoryRouter>,
  )
  await screen.findByText(CARD.title)
  // The deck binds its keys in an effect that runs after the render that drew
  // the card, so a key pressed in between reaches the handler bound to the
  // empty deck. Pressing until the dismiss is sent waits for the bound one;
  // the write is recorded synchronously, so it is sent once.
  await waitFor(() => {
    fireEvent.keyDown(window, { key: 'ArrowLeft' })
    expect(writes).toHaveLength(1)
  })
  return screen.findByRole('button', { name: 'Undo' })
}

describe('the undo toast', () => {
  it("requeues with the version the dismiss's write returned", async () => {
    const undo = await dismissTopCard()
    expect(writes[0]).toEqual({
      path: `/api/tasks/${CARD.id}`,
      method: 'PATCH',
      body: { status: 'dismissed', version: CARD.version },
    })

    fireEvent.click(undo)
    await waitFor(() => expect(writes).toHaveLength(2))
    expect(writes[1]).toEqual({
      path: `/api/tasks/${CARD.id}/requeue`,
      method: 'POST',
      body: { version: 3 },
    })
  })

  it('drops the undo when the task changed since the swipe', async () => {
    requeueAnswer = () => {
      throw new HttpError(
        409,
        JSON.stringify({
          errors: [
            {
              reason: 'VERSION_CONFLICT',
              message: 'this task changed since you read it; refetch it and decide again',
              field: 'version',
            },
          ],
        }),
      )
    }
    const undo = await dismissTopCard()

    fireEvent.click(undo)
    await waitFor(() => expect(toast.failed).toHaveBeenCalledTimes(1))
    await waitFor(() =>
      expect(screen.queryByRole('button', { name: 'Undo' })).not.toBeInTheDocument(),
    )
  })
})

// A claim names the team the task lands on. The deck asks which only when more
// than one of the viewer's teams can take the card, and the card stays on top
// until the question is answered.
describe('the claim swipe', () => {
  async function renderDeck() {
    render(
      <MemoryRouter>
        <Cards />
      </MemoryRouter>,
    )
    await screen.findByText(CARD.title)
  }

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

  it('sends no team for a person on one team', async () => {
    await renderDeck()
    await waitFor(() => {
      fireEvent.keyDown(window, { key: 'ArrowRight' })
      expect(writes).toHaveLength(1)
    })
    expect(writes[0]).toEqual({
      path: `/api/tasks/${CARD.id}/claim`,
      method: 'POST',
      body: { version: CARD.version },
    })
  })

  it('sends the one team that can take the card, without asking', async () => {
    onTwoTeams()
    claimTeams = ['t2']
    await renderDeck()
    await waitFor(() => {
      fireEvent.keyDown(window, { key: 'ArrowRight' })
      expect(writes).toHaveLength(1)
    })
    expect(writes[0].body).toEqual({ team_id: 't2', version: CARD.version })
    expect(screen.queryByText('Which team takes this task?')).not.toBeInTheDocument()
  })

  it('asks which team when two can take the card, and claims for the one chosen', async () => {
    onTwoTeams()
    claimTeams = ['t1', 't2']
    await renderDeck()
    await waitFor(() => {
      fireEvent.keyDown(window, { key: 'ArrowRight' })
      expect(screen.getByText('Which team takes this task?')).toBeInTheDocument()
    })
    // Nothing is written, and the card is still the top card.
    expect(writes).toHaveLength(0)
    expect(screen.getByText(CARD.title)).toBeInTheDocument()

    fireEvent.click(screen.getByRole('radio', { name: 'data' }))
    fireEvent.click(screen.getByRole('button', { name: 'Claim' }))
    await waitFor(() => expect(writes).toHaveLength(1))
    expect(writes[0]).toEqual({
      path: `/api/tasks/${CARD.id}/claim`,
      method: 'POST',
      body: { team_id: 't2', version: CARD.version },
    })
  })

  it('writes nothing, and keeps the card, when the question is cancelled', async () => {
    onTwoTeams()
    claimTeams = ['t1', 't2']
    await renderDeck()
    await waitFor(() => {
      fireEvent.keyDown(window, { key: 'ArrowRight' })
      expect(screen.getByText('Which team takes this task?')).toBeInTheDocument()
    })
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))

    await waitFor(() =>
      expect(screen.queryByText('Which team takes this task?')).not.toBeInTheDocument(),
    )
    expect(writes).toHaveLength(0)
    expect(screen.getByText(CARD.title)).toBeInTheDocument()
  })

  it('writes nothing when no team of the viewer’s can take the card', async () => {
    onTwoTeams()
    claimTeams = []
    await renderDeck()
    await waitFor(() => {
      fireEvent.keyDown(window, { key: 'ArrowRight' })
      expect(toast.error).toHaveBeenCalledWith('You have no team this task can land on.')
    })
    expect(writes).toHaveLength(0)
    expect(screen.getByText(CARD.title)).toBeInTheDocument()
  })
})
