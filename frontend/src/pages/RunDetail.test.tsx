import { describe, it, expect, vi, beforeEach } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router'
import RunDetail from './RunDetail'
import type { Conversation, Task } from '../types'

const stopping = {
  ID: 'c1',
  TaskID: '',
  Status: 'running',
  Model: 'claude-opus-5',
  StartedAt: '2026-09-22T00:00:00Z',
  ResultSummary: '',
  stop_requested_at: '2026-09-22T00:00:05Z',
} as Conversation

// What the mocked detail hook hands the page; each test sets it.
const detail: { conversation: Conversation; task: Task | null } = {
  conversation: stopping,
  task: null,
}

vi.mock('../hooks/useConversationDetail', () => ({
  useConversationDetail: () => ({
    conversation: detail.conversation,
    task: detail.task,
    messages: [],
    loading: false,
    notFound: false,
    error: null,
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
      TaskID: 't1',
      Status: 'open',
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
})
