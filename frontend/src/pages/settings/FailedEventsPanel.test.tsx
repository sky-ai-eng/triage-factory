import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor, within } from '@testing-library/react'

import { toast } from '../../components/Toast/toastStore'
import FailedEventsPanel from './FailedEventsPanel'
import { useFailedEvents, type FailedEvent } from '../../hooks/useFailedEvents'

// The panel is a pure view over the hook, so the tests drive the real hook
// against a mocked fetch — that way the wire contract (the {events} envelope,
// the requeue body, the refresh after a requeue) is under test too, not just
// the rendering.
function Harness() {
  const state = useFailedEvents(true)
  return <FailedEventsPanel state={state} />
}

let parked: FailedEvent[]
let requeueBodies: number[][]
// How many of the next requeue's ids the server reports as actually moved.
// null means "all of them" — the ordinary case.
let movedOverride: number | null

function row(over: Partial<FailedEvent> = {}): FailedEvent {
  return {
    id: 1,
    event_type: 'github:pr:ci_check_failed',
    entity_id: 'entity-1',
    entity_source: 'github',
    entity_source_id: 'owner/repo#18',
    entity_title: 'Fix the flaky test',
    attempts: 5,
    last_error: 'route: upsert task: db down (after 5 attempts)',
    enqueued_at: new Date(Date.now() - 3 * 60 * 60 * 1000).toISOString(),
    ...over,
  }
}

function jsonResponse(body: unknown): Response {
  return {
    ok: true,
    status: 200,
    json: async () => body,
    text: async () => JSON.stringify(body),
    clone() {
      return this as unknown as Response
    },
  } as unknown as Response
}

function installFetch() {
  const fetchMock = vi.fn(async (input: unknown, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : String(input)
    const [path] = url.split('?')
    const method = init?.method ?? 'GET'

    if (path === '/api/events/failed/list' && method === 'POST') {
      return jsonResponse({ items: parked, next_page_token: '', total_count: parked.length })
    }
    if (path === '/api/events/failed/requeue' && method === 'POST') {
      const body = JSON.parse(init!.body as string) as { ids: number[] }
      requeueBodies.push(body.ids)
      const moved = movedOverride ?? body.ids.length
      // Mirror the backend: only rows that were still parked leave the list.
      const gone = new Set(body.ids.slice(0, moved))
      parked = parked.filter((e) => !gone.has(e.id))
      return jsonResponse({ requeued: moved })
    }
    throw new Error(`unexpected fetch ${method} ${path}`)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

beforeEach(() => {
  parked = []
  requeueBodies = []
  movedOverride = null
  installFetch()
})

describe('FailedEventsPanel', () => {
  it('says nothing is parked when the list is empty — the healthy state', async () => {
    render(<Harness />)
    expect(await screen.findByText(/nothing parked/i)).toBeInTheDocument()
  })

  it('renders a parked row with its type, entity, age, attempts and error', async () => {
    parked = [row()]
    render(<Harness />)

    // The raw event type, not a prettified label: it's the string an operator
    // matches against logs and trigger config.
    expect(await screen.findByText('github:pr:ci_check_failed')).toBeInTheDocument()
    expect(screen.getByText('owner/repo#18')).toBeInTheDocument()
    expect(screen.getByText('Fix the flaky test')).toBeInTheDocument()
    expect(screen.getByText('3h ago')).toBeInTheDocument()
    // last_error verbatim — the reason it gave up is the point of the panel.
    expect(screen.getByText('route: upsert task: db down (after 5 attempts)')).toBeInTheDocument()
  })

  it('still renders a row whose entity is gone', async () => {
    parked = [row({ entity_id: '', entity_source: '', entity_source_id: '', entity_title: '' })]
    render(<Harness />)
    expect(await screen.findByText('github:pr:ci_check_failed')).toBeInTheDocument()
    expect(screen.getByText('—')).toBeInTheDocument()
  })

  it('requeues one row and drops it from the list', async () => {
    parked = [row({ id: 7 }), row({ id: 8, entity_source_id: 'owner/repo#19' })]
    render(<Harness />)
    await screen.findByText('owner/repo#18')

    const rowEl = screen.getByText('owner/repo#18').closest('tr')!
    fireEvent.click(within(rowEl).getByRole('button', { name: /requeue/i }))

    await waitFor(() => expect(requeueBodies).toEqual([[7]]))
    // The hook refetches after the requeue, so the moved row leaves the table.
    await waitFor(() => expect(screen.queryByText('owner/repo#18')).not.toBeInTheDocument())
    expect(screen.getByText('owner/repo#19')).toBeInTheDocument()
  })

  it('select-all requeues every listed row in one call', async () => {
    parked = [row({ id: 7 }), row({ id: 8, entity_source_id: 'owner/repo#19' })]
    render(<Harness />)
    await screen.findByText('owner/repo#18')

    fireEvent.click(screen.getByRole('checkbox', { name: /select all parked events/i }))
    fireEvent.click(screen.getByRole('button', { name: /requeue selected \(2\)/i }))

    await waitFor(() => expect(requeueBodies).toEqual([[7, 8]]))
    expect(await screen.findByText(/nothing parked/i)).toBeInTheDocument()
  })

  // A stale selection is the normal case for a table you read and then act on,
  // so a partial move has to read as an outcome, not a failure.
  it('reports a partial move honestly when some ids were no longer parked', async () => {
    parked = [row({ id: 7 }), row({ id: 8, entity_source_id: 'owner/repo#19' })]
    movedOverride = 1
    const infoSpy = vi.spyOn(toast, 'info')
    render(<Harness />)
    await screen.findByText('owner/repo#18')

    fireEvent.click(screen.getByRole('checkbox', { name: /select all parked events/i }))
    fireEvent.click(screen.getByRole('button', { name: /requeue selected/i }))

    await waitFor(() => expect(requeueBodies).toEqual([[7, 8]]))
    await waitFor(() =>
      expect(infoSpy).toHaveBeenCalledWith(expect.stringMatching(/requeued 1 of 2/i)),
    )
  })

  it('disables the bulk button until something is selected', async () => {
    parked = [row({ id: 7 })]
    render(<Harness />)
    await screen.findByText('owner/repo#18')

    const bulk = screen.getByRole('button', { name: /requeue selected/i })
    expect(bulk).toBeDisabled()
    fireEvent.click(screen.getByRole('checkbox', { name: /select event 7/i }))
    expect(screen.getByRole('button', { name: /requeue selected \(1\)/i })).toBeEnabled()
  })
})
