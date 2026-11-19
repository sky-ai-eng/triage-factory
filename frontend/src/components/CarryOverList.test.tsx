// The carry-over deck names a ticket by its entity_id in every per-row state
// and in the POST that applies them, because an issue that moves between the
// read and the write keeps its entity but not its key.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

import CarryOverList from './CarryOverList'
import { invalidateTeams } from '../hooks/useTeams'
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

const DECK = jsonBody({ status: 'ready', assigned: [TICKET], available: [] })

// The teams list is a module-level cache, so each test starts it empty.
beforeEach(() => invalidateTeams())

// stubFetch answers each path from a table, failing loudly on one it lacks,
// so a deck read under the wrong team, or under none, cannot pass.
function stubFetch(table: Record<string, () => object>) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, _init?: RequestInit) => {
    const answer = table[String(input)]
    if (!answer) throw new Error(`unexpected fetch: ${String(input)}`)
    return answer()
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

const ok = (body: object) => ({ ok: true, status: 200, ...body })

// save mounts the deck with TICKET prefilled to queue, presses Save, and
// answers the queue POST with queueResult.
async function save(queueResult: unknown, onSave = vi.fn()) {
  const fetchMock = stubFetch({
    '/api/teams/list': () => ok(listBody([{ id: 't1', name: 'Team' }])),
    '/api/jira/stock?team_id=t1': () => ok(DECK),
    '/api/jira/stock/queue': () => ok(jsonBody(queueResult)),
  })
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

  it('reads the deck for the team Save claims into, not one the server picks', async () => {
    // The cached list says t2 is the last acting team. A read naming no team
    // would leave the server to pick from its own record, which can have
    // moved since, and the deck would list a backlog Save does not claim into.
    const onSave = vi.fn()
    const fetchMock = stubFetch({
      '/api/teams/list': () =>
        ok(
          listBody([
            { id: 't1', name: 'Platform' },
            { id: 't2', name: 'Web', is_last_acting: true },
          ]),
        ),
      '/api/jira/stock?team_id=t2': () => ok(DECK),
      '/api/jira/stock/queue': () =>
        ok(jsonBody({ applied: 1, failed: 0, results: [{ entity_id: 'e-1', ok: true }] })),
    })
    render(<CarryOverList onSave={onSave} onSkip={vi.fn()} onBack={vi.fn()} />)

    await screen.findByText('Carry me over')
    const button = screen.getByRole('button', { name: 'Save' })
    await waitFor(() => expect(button).toBeEnabled())
    await userEvent.setup().click(button)
    await waitFor(() => expect(onSave).toHaveBeenCalled())

    const stockReads = fetchMock.mock.calls
      .map(([url]) => String(url))
      .filter((url) => url.startsWith('/api/jira/stock?') || url === '/api/jira/stock')
    expect(stockReads).toEqual(['/api/jira/stock?team_id=t2'])
    const [, init] = fetchMock.mock.calls.find(([url]) => url === '/api/jira/stock/queue')!
    expect(JSON.parse(String(init?.body))).toEqual({ entity_ids: ['e-1'], team_id: 't2' })
  })

  it('says the teams could not load, and reads the deck once Retry loads them', async () => {
    let teamReads = 0
    stubFetch({
      '/api/teams/list': () =>
        ++teamReads === 1
          ? { ok: false, status: 500, ...jsonBody({}) }
          : ok(listBody([{ id: 't1', name: 'Team' }])),
      '/api/jira/stock?team_id=t1': () => ok(DECK),
    })
    render(<CarryOverList onSave={vi.fn()} onSkip={vi.fn()} onBack={vi.fn()} />)

    expect(await screen.findByText('Could not load teams.')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
    expect(await screen.findByText('Carry me over')).toBeInTheDocument()
    expect(teamReads).toBe(2)
  })
})
