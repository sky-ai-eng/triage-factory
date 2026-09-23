import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router'
import RunDetail from './RunDetail'
import type { Conversation } from '../types'

const conversation = {
  ID: 'c1',
  TaskID: '',
  Status: 'running',
  Model: 'claude-opus-5',
  StartedAt: '2026-09-22T00:00:00Z',
  ResultSummary: '',
  artifact_count: 0,
  stop_requested_at: '2026-09-22T00:00:05Z',
} as Conversation

vi.mock('../hooks/useConversationDetail', () => ({
  useConversationDetail: () => ({
    conversation,
    task: null,
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

describe('RunDetail stop controls', () => {
  beforeEach(() => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.reject(new Error('no network in this suite'))),
    )
  })

  it('keeps the stop control disabled while a requested stop is unsettled', () => {
    render(
      <MemoryRouter initialEntries={['/runs/c1']}>
        <Routes>
          <Route path="/runs/:conversationID" element={<RunDetail />} />
        </Routes>
      </MemoryRouter>,
    )
    const stop = screen.getByRole('button', { name: /stopping/i })
    expect(stop).toBeDisabled()
  })
})
