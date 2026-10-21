// The carry-over deck's write half. Every per-row piece of state — a
// selection, a failure — and the POST that applies them name a ticket by its
// entity_id, which the deck serves on each ticket; the issue key is what a row
// shows, and an issue that moves between the read and the write keeps its
// entity but not its key.
import { describe, it, expect, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

import CarryOverList from './CarryOverList'
import { jsonBody } from '../test/apiResponse'

const TICKET = {
  entity_id: 'e-1',
  issue_key: 'SKY-1',
  summary: 'Carry me over',
  status: 'To Do',
  project: 'SKY',
  issue_type: 'Task',
  priority: 'Medium',
  url: 'https://jira.example.com/browse/SKY-1',
  bucket: 'assigned',
  prefilled_action: 'queue',
}

function stubFetch(queueResult: { applied: number; failed: number; results: unknown[] }) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, _init?: RequestInit) => {
    const url = String(input)
    if (url === '/api/teams/list') {
      return {
        ok: true,
        status: 200,
        ...jsonBody({ items: [{ id: 't1', name: 'Team' }], next_page_token: '', total_count: 1 }),
      }
    }
    if (url === '/api/jira/stock') {
      return {
        ok: true,
        status: 200,
        ...jsonBody({ status: 'ready', assigned: [TICKET], available: [] }),
      }
    }
    if (url === '/api/jira/stock/queue') {
      return { ok: true, status: 200, ...jsonBody(queueResult) }
    }
    throw new Error(`unexpected fetch: ${url}`)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

function queueBody(fetchMock: ReturnType<typeof stubFetch>): Record<string, unknown> {
  const call = fetchMock.mock.calls.find(([url]) => String(url) === '/api/jira/stock/queue')
  expect(call).toBeTruthy()
  return JSON.parse(String(call![1]?.body))
}

describe('CarryOverList', () => {
  it('applies the selection by entity id, never by issue key', async () => {
    const user = userEvent.setup()
    const fetchMock = stubFetch({
      applied: 1,
      failed: 0,
      results: [{ entity_id: 'e-1', issue_key: 'SKY-1', ok: true }],
    })
    const onSave = vi.fn()
    render(<CarryOverList onSave={onSave} onSkip={vi.fn()} onBack={vi.fn()} />)

    expect(await screen.findByText('Carry me over')).toBeInTheDocument()
    const save = screen.getByRole('button', { name: 'Save' })
    await waitFor(() => expect(save).toBeEnabled())
    await user.click(save)

    await waitFor(() => expect(onSave).toHaveBeenCalled())
    const body = queueBody(fetchMock)
    expect(body.entity_ids).toEqual(['e-1'])
    expect(body).not.toHaveProperty('issue_keys')
  })

  it('shows a failed row’s error on the ticket its entity id names', async () => {
    const user = userEvent.setup()
    stubFetch({
      applied: 0,
      failed: 1,
      // The issue moved since the read: the row answers under the key it has
      // now, and the deck still finds its ticket by the entity id.
      results: [
        {
          entity_id: 'e-1',
          issue_key: 'OPS-9',
          ok: false,
          errors: [{ reason: 'CONFLICT', message: 'ticket already has an active task' }],
        },
      ],
    })
    render(<CarryOverList onSave={vi.fn()} onSkip={vi.fn()} onBack={vi.fn()} />)

    expect(await screen.findByText('Carry me over')).toBeInTheDocument()
    const save = screen.getByRole('button', { name: 'Save' })
    await waitFor(() => expect(save).toBeEnabled())
    await user.click(save)

    expect(await screen.findByText('ticket already has an active task')).toBeInTheDocument()
  })
})
