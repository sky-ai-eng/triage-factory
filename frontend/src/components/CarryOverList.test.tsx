// The carry-over deck names a ticket by its entity_id in every per-row state
// and in the POST that applies them, because an issue that moves between the
// read and the write keeps its entity but not its key.
import { describe, it, expect, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

import CarryOverList from './CarryOverList'
import { jsonBody, listBody } from '../test/apiResponse'

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

// save mounts the deck with TICKET prefilled to queue, presses Save, and
// answers the queue POST with queueResult.
async function save(queueResult: unknown, onSave = vi.fn()) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, _init?: RequestInit) => {
    const body = {
      '/api/teams/list': listBody([{ id: 't1', name: 'Team' }]),
      '/api/jira/stock': jsonBody({ status: 'ready', assigned: [TICKET], available: [] }),
      '/api/jira/stock/queue': jsonBody(queueResult),
    }[String(input)]
    if (!body) throw new Error(`unexpected fetch: ${String(input)}`)
    return { ok: true, status: 200, ...body }
  })
  vi.stubGlobal('fetch', fetchMock)
  render(<CarryOverList onSave={onSave} onSkip={vi.fn()} onBack={vi.fn()} />)

  await screen.findByText('Carry me over')
  const button = screen.getByRole('button', { name: 'Save' })
  await waitFor(() => expect(button).toBeEnabled())
  await userEvent.setup().click(button)
  return fetchMock
}

describe('CarryOverList', () => {
  it('applies the selection by entity id, never by issue key', async () => {
    const onSave = vi.fn()
    const fetchMock = await save(
      { applied: 1, failed: 0, results: [{ entity_id: 'e-1', issue_key: 'SKY-1', ok: true }] },
      onSave,
    )

    await waitFor(() => expect(onSave).toHaveBeenCalled())
    const [, init] = fetchMock.mock.calls.find(([url]) => url === '/api/jira/stock/queue')!
    expect(JSON.parse(String(init?.body))).toEqual({ entity_ids: ['e-1'], team_id: 't1' })
  })

  it('shows a failed row’s error on the ticket its entity id names', async () => {
    // The issue moved since the read, so the row answers under its new key.
    await save({
      applied: 0,
      failed: 1,
      results: [
        {
          entity_id: 'e-1',
          issue_key: 'OPS-9',
          ok: false,
          errors: [{ reason: 'CONFLICT', message: 'ticket already has an active task' }],
        },
      ],
    })

    expect(await screen.findByText('ticket already has an active task')).toBeInTheDocument()
  })
})
