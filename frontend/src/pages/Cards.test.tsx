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
vi.mock('../hooks/useTeams', () => ({ useTeamFilter: () => TEAM_FILTER }))
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

beforeEach(() => {
  writes = []
  toast.failed.mockReset()
  requeueAnswer = () => ({ ...CARD, version: 4 })
  api.apiList.mockReset()
  api.apiJSON.mockReset()
  api.apiList.mockResolvedValue({ items: [CARD, NEXT], next_page_token: '', total_count: 2 })
  api.apiJSON.mockImplementation(
    async (path: string, init?: { method?: string; body?: string }) => {
      // The page's other reads (its config, among them) are not what this is
      // about.
      if (!path.startsWith('/api/tasks/')) return {}
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
  fireEvent.keyDown(window, { key: 'ArrowLeft' })
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
