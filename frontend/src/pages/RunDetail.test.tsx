import { describe, it, expect, vi, beforeEach } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router'
import RunDetail from './RunDetail'
import type { Conversation, Task } from '../types'
import { jsonBody } from '../test/apiResponse'
import { resetModelCatalogForTest } from '../hooks/useModelCatalog'

const stopping = {
  id: 'c1',
  task_id: '',
  status: 'running',
  model: 'claude-opus-5',
  started_at: '2026-09-22T00:00:00Z',
  result_summary: '',
  stop_requested_at: '2026-09-22T00:00:05Z',
} as Conversation

// What the mocked detail hook hands the page; each test sets it.
const detail: { conversation: Conversation; task: Task | null } = {
  conversation: stopping,
  task: null,
}
const refetch = vi.hoisted(() => vi.fn())

vi.mock('../hooks/useConversationDetail', () => ({
  useConversationDetail: () => ({
    conversation: detail.conversation,
    task: detail.task,
    messages: [],
    loading: false,
    notFound: false,
    error: null,
    refetch,
    pendingPermissions: [],
    resolvePermission: () => {},
    softRefresh: () => {},
    hasOlderMessages: false,
    loadingOlderMessages: false,
    loadOlderMessages: async () => {},
  }),
}))

vi.mock('../hooks/useWebSocket', () => ({
  setPresenceView: () => {},
  useWebSocket: () => {},
}))

function renderRunDetail() {
  return render(
    <MemoryRouter initialEntries={['/runs/c1']}>
      <Routes>
        <Route path="/runs/:conversationID" element={<RunDetail />} />
        <Route path="/board" element={<p>The board</p>} />
      </Routes>
    </MemoryRouter>,
  )
}

beforeEach(() => {
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.reject(new Error('no network in this suite'))),
  )
  detail.conversation = stopping
  detail.task = null
  refetch.mockReset()
})

describe('RunDetail stop controls', () => {
  it('keeps the stop control disabled while a requested stop is unsettled', () => {
    renderRunDetail()
    const stop = screen.getByRole('button', { name: /stopping/i })
    expect(stop).toBeDisabled()
  })
})

describe('RunDetail return to queue', () => {
  // A parked run, which is when the station offers the requeue.
  function parkedOn(task: Partial<Task>) {
    detail.conversation = {
      ...stopping,
      task_id: 't1',
      status: 'open',
      stop_requested_at: undefined,
    }
    detail.task = { id: 't1', memory_pending: false, ...task } as Task
  }

  it('offers it to a caller who may write the task, and to a response without the field', () => {
    for (const canWrite of [true, undefined]) {
      cleanup()
      parkedOn(canWrite === undefined ? {} : { can_write: canWrite })
      renderRunDetail()
      expect(screen.getByRole('button', { name: 'Return to queue' })).toBeInTheDocument()
    }
  })

  it('withholds it from a caller who may not', () => {
    parkedOn({ can_write: false })
    renderRunDetail()
    expect(screen.queryByRole('button', { name: 'Return to queue' })).not.toBeInTheDocument()
  })

  it('withholds it from a page whose task did not load, which has no version to name', () => {
    parkedOn({})
    detail.task = null
    renderRunDetail()
    expect(screen.queryByRole('button', { name: 'Return to queue' })).not.toBeInTheDocument()
  })

  it('reloads the task when the server says it changed since the page read it', async () => {
    parkedOn({ version: 3 })
    const fetchMock = vi.fn().mockResolvedValue({
      ok: false,
      status: 409,
      ...jsonBody({
        errors: [{ reason: 'VERSION_CONFLICT', message: 'this task changed', field: 'version' }],
      }),
    })
    vi.stubGlobal('fetch', fetchMock)
    renderRunDetail()

    fireEvent.click(screen.getByRole('button', { name: 'Return to queue' }))
    await waitFor(() => expect(refetch).toHaveBeenCalledTimes(1))
    const requeue = (fetchMock.mock.calls as [string, RequestInit][]).find(
      ([path]) => path === '/api/tasks/t1/requeue',
    )
    expect(requeue && JSON.parse(String(requeue[1].body))).toEqual({ version: 3 })
  })
})

describe('RunDetail send', () => {
  beforeEach(() => resetModelCatalogForTest())

  it('posts the text with the model and effort the composer names', async () => {
    detail.conversation = {
      ...stopping,
      task_id: 't1',
      status: 'open',
      has_run: true,
      stop_requested_at: undefined,
      next_message_model: 'opus',
      next_message_effort: 'high',
    }
    detail.task = { id: 't1', team_id: 'team1', memory_pending: false } as Task
    const fetchMock = vi.fn(async (url: string) => {
      if (url.endsWith('/models'))
        return {
          ok: true,
          ...jsonBody({
            items: [
              {
                key: 'opus',
                display_name: 'Opus 5',
                enabled: true,
                effort_levels: ['low', 'high'],
              },
            ],
          }),
        }
      if (url.endsWith('/settings'))
        return {
          ok: true,
          ...jsonBody({ team_settings: { DefaultModel: 'opus', EnabledModels: null } }),
        }
      return { ok: true, status: 200, ...jsonBody({}) }
    })
    vi.stubGlobal('fetch', fetchMock)
    renderRunDetail()

    await waitFor(() =>
      expect(screen.getByRole('button', { name: /message settings/i })).toHaveAccessibleName(
        /Opus 5 · High/,
      ),
    )
    fireEvent.change(screen.getByLabelText(/message to resume/i), {
      target: { value: 'try the other fixture' },
    })
    fireEvent.click(screen.getByRole('button', { name: 'Send message' }))

    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/agent/conversations/c1/message',
        expect.objectContaining({ method: 'POST' }),
      ),
    )
    const sent = (fetchMock.mock.calls as unknown as [string, RequestInit][]).find(
      ([path]) => path === '/api/agent/conversations/c1/message',
    )
    expect(JSON.parse(String(sent?.[1].body))).toEqual({
      text: 'try the other fixture',
      model: 'opus',
      effort: 'high',
    })
  })
})

describe('RunDetail Escape', () => {
  // A parked run a person may write, which is when the station shows the
  // composer.
  function parked(conversation: Partial<Conversation> = {}) {
    detail.conversation = {
      ...stopping,
      task_id: 't1',
      status: 'open',
      has_run: true,
      stop_requested_at: undefined,
      ...conversation,
    }
    detail.task = { id: 't1', team_id: 'team1', memory_pending: false } as Task
  }

  // Pressed outside any field, wherever focus is, so the page's skip for a
  // press typed in a field never decides a test.
  const escape = () => fireEvent.keyDown(document.body, { key: 'Escape' })

  it('closes the message-settings popover and stays on the page', async () => {
    parked()
    renderRunDetail()
    fireEvent.change(screen.getByLabelText(/message to resume/i), {
      target: { value: 'a draft' },
    })
    fireEvent.click(screen.getByRole('button', { name: /message settings/i }))
    expect(await screen.findByRole('dialog')).toBeInTheDocument()

    escape()

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(screen.queryByText('The board')).not.toBeInTheDocument()
    expect(screen.getByLabelText(/message to resume/i)).toHaveValue('a draft')
  })

  it('closes a deliverable overlay and stays on the page', async () => {
    parked({
      has_unresolved_deliverables: true,
      unresolved_pr_count: 1,
      pending_deliverable_ids: ['d1'],
    })
    renderRunDetail()
    fireEvent.click(screen.getByRole('button', { name: /open pr/i }))
    expect(await screen.findByRole('dialog', { name: 'Draft PR' })).toBeInTheDocument()

    escape()

    await waitFor(() =>
      expect(screen.queryByRole('dialog', { name: 'Draft PR' })).not.toBeInTheDocument(),
    )
    expect(screen.queryByText('The board')).not.toBeInTheDocument()
  })

  it('goes back to the board with nothing open', () => {
    parked()
    renderRunDetail()

    // Marked, so nothing beneath the page acts on the same press.
    expect(escape()).toBe(false)
    expect(screen.getByText('The board')).toBeInTheDocument()
  })
})
