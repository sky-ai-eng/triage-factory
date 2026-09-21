import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor, within } from '@testing-library/react'

import { toast } from '../../components/Toast/toastStore'
import ParkedWorkPanel, { ParkedWorkBadge } from './ParkedWorkPanel'
import {
  useParkedWork,
  type WorkItem,
  type WorkKind,
  type WorkDepth,
} from '../../hooks/useParkedWork'

const ORG = '00000000-0000-0000-0000-000000000001'
const BASE = `/api/orgs/${ORG}/work`

// The panel is a pure view over the hook, so the tests drive the real hook
// against a mocked fetch — that way the wire contract (the catalogue, the
// depth node, the list envelope, the control bodies, the refresh after a
// control) is under test too, not just the rendering.
function Harness() {
  const state = useParkedWork(ORG, true)
  return (
    <>
      <div data-testid="badge">
        <ParkedWorkBadge state={state} />
      </div>
      <ParkedWorkPanel state={state} />
    </>
  )
}

let kinds: WorkKind[]
let rows: WorkItem[]
let depthOverride: Record<string, Partial<WorkDepth>>
// Kinds whose depth node answers 500, standing in for a transient fault on
// one queue while the others are healthy.
let failingKinds: Set<string>
let listBodies: { kind: string; body: Record<string, unknown> }[]
let redriveBodies: { kind: string; ids: number[] }[]
let cancelBodies: { kind: string; ids: number[]; reason: string }[]
// How many of the next redrive's ids the server reports as actually moved.
// null means "all of them" — the ordinary case.
let movedOverride: number | null

function kind(name: string, over: Partial<WorkKind> = {}): WorkKind {
  return {
    kind: name,
    label: name === 'event_queue' ? 'Event routing' : name,
    controls: { redrive: true, supersede: false, cancel: true },
    objective: { oldest_ready_age_seconds: 60 },
    ...over,
  }
}

function row(over: Partial<WorkItem> = {}): WorkItem {
  return {
    kind: 'event_queue',
    id: 1,
    status: 'parked',
    attempt: 5,
    max_attempts: 5,
    last_outcome: 'transient',
    last_error: 'route: upsert task: db down (after 5 attempts)',
    first_enqueued_at: new Date(Date.now() - 3 * 60 * 60 * 1000).toISOString(),
    created_at: new Date(Date.now() - 3 * 60 * 60 * 1000).toISOString(),
    done_at: new Date().toISOString(),
    subject: {
      label: 'owner/repo#18',
      detail: 'Fix the flaky test',
      fields: { event_type: 'github:pr:ci_check_failed' },
    },
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

function depthFor(name: string): WorkDepth {
  const mine = rows.filter((r) => r.kind === name)
  return {
    ready: mine.filter((r) => r.status === 'ready').length,
    leased: mine.filter((r) => r.status === 'leased').length,
    parked: mine.filter((r) => r.status === 'parked').length,
    deferred: 0,
    oldest_ready_age_seconds: 0,
    oldest_deferred_age_seconds: 0,
    ...(depthOverride[name] ?? {}),
  }
}

function installFetch() {
  const fetchMock = vi.fn(async (input: unknown, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : String(input)
    const [path] = url.split('?')
    const method = init?.method ?? 'GET'

    if (path === BASE && method === 'GET') {
      return jsonResponse({ kinds })
    }
    const m = path.match(
      new RegExp(`^${BASE}/([^/]+)/(depth|items/list|items/redrive|items/cancel)$`),
    )
    if (!m) throw new Error(`unexpected fetch ${method} ${path}`)
    const [, name, op] = m
    if (op === 'depth' && method === 'GET') {
      if (failingKinds.has(name)) {
        return {
          ok: false,
          status: 500,
          json: async () => ({}),
          text: async () => '{"errors":[{"reason":"INTERNAL","message":"measure failed"}]}',
          clone() {
            return this as unknown as Response
          },
        } as unknown as Response
      }
      return jsonResponse(depthFor(name))
    }
    if (op === 'items/list' && method === 'POST') {
      const body = JSON.parse(init!.body as string) as Record<string, unknown>
      listBodies.push({ kind: name, body })
      const status = body.status as string
      const mine = rows.filter((r) => r.kind === name && r.status === status)
      return jsonResponse({ items: mine, next_page_token: '', total_count: mine.length })
    }
    if (op === 'items/redrive' && method === 'POST') {
      const body = JSON.parse(init!.body as string) as { ids: number[] }
      redriveBodies.push({ kind: name, ids: body.ids })
      const moved = movedOverride ?? body.ids.length
      // Mirror the backend: only rows that were still parked leave the set.
      const gone = new Set(body.ids.slice(0, moved))
      rows = rows.filter((r) => !(r.kind === name && gone.has(r.id)))
      return jsonResponse({ redriven: moved })
    }
    if (op === 'items/cancel' && method === 'POST') {
      const body = JSON.parse(init!.body as string) as { ids: number[]; reason: string }
      cancelBodies.push({ kind: name, ...body })
      // A request is recorded, not a status change: the row stays, marked.
      rows = rows.map((r) =>
        r.kind === name && body.ids.includes(r.id)
          ? {
              ...r,
              cancel: { requested_at: new Date().toISOString(), by: 'me', reason: body.reason },
            }
          : r,
      )
      return jsonResponse({ requested: body.ids.length })
    }
    throw new Error(`unexpected fetch ${method} ${path}`)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

beforeEach(() => {
  kinds = [kind('event_queue')]
  rows = []
  depthOverride = {}
  failingKinds = new Set()
  listBodies = []
  redriveBodies = []
  cancelBodies = []
  movedOverride = null
  installFetch()
})

describe('ParkedWorkPanel', () => {
  it('says nothing is parked when the list is empty — the healthy state', async () => {
    render(<Harness />)
    expect(await screen.findByText(/nothing parked/i)).toBeInTheDocument()
    expect(screen.getByTestId('badge')).toHaveTextContent('None parked')
  })

  it('renders a parked row with its subject, outcome, attempts, age and error', async () => {
    rows = [row()]
    render(<Harness />)

    expect(await screen.findByText('owner/repo#18')).toBeInTheDocument()
    expect(screen.getByText('Fix the flaky test')).toBeInTheDocument()
    // The raw event type, not a prettified label: it's the string an operator
    // matches against logs and trigger config.
    expect(screen.getByText('github:pr:ci_check_failed')).toBeInTheDocument()
    expect(screen.getByText('transient')).toBeInTheDocument()
    expect(screen.getByText('5/5')).toBeInTheDocument()
    expect(screen.getByText('3h ago')).toBeInTheDocument()
    // last_error verbatim — the reason it gave up is the point of the panel.
    expect(screen.getByText('route: upsert task: db down (after 5 attempts)')).toBeInTheDocument()
    // One registered kind: no Kind column.
    expect(screen.queryByRole('columnheader', { name: 'Kind' })).not.toBeInTheDocument()
  })

  it('sums the badge across kinds from the depth nodes, not the page', async () => {
    kinds = [kind('event_queue'), kind('pending_firings', { label: 'Pending firings' })]
    rows = [
      row({ id: 1 }),
      row({ id: 2, kind: 'pending_firings', subject: { label: 'SKY-12', fields: {} } }),
    ]
    // The depth is the org's whole population; the page is what loaded.
    depthOverride = { event_queue: { parked: 40 }, pending_firings: { parked: 2 } }
    render(<Harness />)

    await screen.findByText('owner/repo#18')
    expect(screen.getByTestId('badge')).toHaveTextContent('42')
    // Two kinds: the Kind column appears with each kind's label.
    expect(screen.getByRole('columnheader', { name: 'Kind' })).toBeInTheDocument()
    expect(screen.getByText('Event routing')).toBeInTheDocument()
    expect(screen.getByText('Pending firings')).toBeInTheDocument()
  })

  it("keeps a healthy kind's rows and count when another kind fails to load", async () => {
    kinds = [kind('event_queue'), kind('pending_firings', { label: 'Pending firings' })]
    rows = [
      row({ id: 1 }),
      row({ id: 2, kind: 'pending_firings', subject: { label: 'SKY-12', fields: {} } }),
    ]
    depthOverride = { event_queue: { parked: 40 } }
    failingKinds = new Set(['pending_firings'])
    render(<Harness />)

    // The healthy kind's rows and its badge count survive; the broken kind
    // is named in an error line beside them rather than replacing them.
    expect(await screen.findByText('owner/repo#18')).toBeInTheDocument()
    expect(screen.getByTestId('badge')).toHaveTextContent('40')
    expect(screen.getByRole('alert')).toHaveTextContent(/pending firings/i)
    expect(screen.queryByText('SKY-12')).not.toBeInTheDocument()
  })

  it('a bulk redrive across kinds refreshes once', async () => {
    kinds = [kind('event_queue'), kind('pending_firings', { label: 'Pending firings' })]
    rows = [
      row({ id: 7 }),
      row({ id: 3, kind: 'pending_firings', subject: { label: 'SKY-12', fields: {} } }),
    ]
    render(<Harness />)
    await screen.findByText('owner/repo#18')
    const before = listBodies.length

    fireEvent.click(screen.getByRole('checkbox', { name: /select all parked work/i }))
    fireEvent.click(screen.getByRole('button', { name: /redrive selected \(2\)/i }))

    await waitFor(() => expect(redriveBodies).toHaveLength(2))
    await screen.findByText(/nothing parked/i)
    // One reload after both controls: one list fetch per kind, not one per
    // control per kind.
    expect(listBodies.length - before).toBe(kinds.length)
  })

  it('a redrive on one kind refetches only that kind and keeps the rest', async () => {
    kinds = [kind('event_queue'), kind('pending_firings', { label: 'Pending firings' })]
    rows = [
      row({ id: 7 }),
      row({ id: 3, kind: 'pending_firings', subject: { label: 'SKY-12', fields: {} } }),
    ]
    render(<Harness />)
    await screen.findByText('owner/repo#18')
    const before = listBodies.length

    const rowEl = screen.getByText('owner/repo#18').closest('tr')!
    fireEvent.click(within(rowEl).getByRole('button', { name: /redrive/i }))

    await waitFor(() => expect(screen.queryByText('owner/repo#18')).not.toBeInTheDocument())
    // The other kind's loaded rows stay, without a refetch that would reset
    // its paging.
    expect(screen.getByText('SKY-12')).toBeInTheDocument()
    expect(listBodies.slice(before).map((l) => l.kind)).toEqual(['event_queue'])
  })

  it('still renders a row the kind could not describe', async () => {
    rows = [row({ id: 9, subject: undefined })]
    render(<Harness />)
    expect(await screen.findByText('#9')).toBeInTheDocument()
  })

  it('redrives one row against its kind and drops it from the list', async () => {
    rows = [row({ id: 7 }), row({ id: 8, subject: { label: 'owner/repo#19', fields: {} } })]
    render(<Harness />)
    await screen.findByText('owner/repo#18')

    const rowEl = screen.getByText('owner/repo#18').closest('tr')!
    fireEvent.click(within(rowEl).getByRole('button', { name: /redrive/i }))

    await waitFor(() => expect(redriveBodies).toEqual([{ kind: 'event_queue', ids: [7] }]))
    // The hook refetches after the redrive, so the moved row leaves the table.
    await waitFor(() => expect(screen.queryByText('owner/repo#18')).not.toBeInTheDocument())
    expect(screen.getByText('owner/repo#19')).toBeInTheDocument()
  })

  it('select-all redrives every parked row, one call per kind', async () => {
    kinds = [kind('event_queue'), kind('pending_firings', { label: 'Pending firings' })]
    rows = [
      row({ id: 7 }),
      row({ id: 8, subject: { label: 'owner/repo#19', fields: {} } }),
      row({ id: 3, kind: 'pending_firings', subject: { label: 'SKY-12', fields: {} } }),
    ]
    render(<Harness />)
    await screen.findByText('owner/repo#18')

    fireEvent.click(screen.getByRole('checkbox', { name: /select all parked work/i }))
    fireEvent.click(screen.getByRole('button', { name: /redrive selected \(3\)/i }))

    await waitFor(() => expect(redriveBodies).toHaveLength(2))
    expect(redriveBodies).toEqual(
      expect.arrayContaining([
        { kind: 'event_queue', ids: [7, 8] },
        { kind: 'pending_firings', ids: [3] },
      ]),
    )
    expect(await screen.findByText(/nothing parked/i)).toBeInTheDocument()
  })

  // A stale selection is the normal case for a table you read and then act on,
  // so a partial move has to read as an outcome, not a failure.
  it('reports a partial move honestly when some ids were no longer parked', async () => {
    rows = [row({ id: 7 }), row({ id: 8, subject: { label: 'owner/repo#19', fields: {} } })]
    movedOverride = 1
    const infoSpy = vi.spyOn(toast, 'info')
    render(<Harness />)
    await screen.findByText('owner/repo#18')

    fireEvent.click(screen.getByRole('checkbox', { name: /select all parked work/i }))
    fireEvent.click(screen.getByRole('button', { name: /redrive selected/i }))

    await waitFor(() => expect(redriveBodies).toEqual([{ kind: 'event_queue', ids: [7, 8] }]))
    await waitFor(() =>
      expect(infoSpy).toHaveBeenCalledWith(expect.stringMatching(/redrove 1 of 2/i)),
    )
  })

  it('the status filter refetches every kind from its first page', async () => {
    rows = [
      row({ id: 7 }),
      row({ id: 8, status: 'ready', attempt: 2, subject: { label: 'owner/repo#19', fields: {} } }),
    ]
    render(<Harness />)
    await screen.findByText('owner/repo#18')
    expect(screen.queryByText('owner/repo#19')).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'Ready' }))

    expect(await screen.findByText('owner/repo#19')).toBeInTheDocument()
    expect(screen.queryByText('owner/repo#18')).not.toBeInTheDocument()
    // Each fetch names its status and carries no token from the other filter.
    const last = listBodies[listBodies.length - 1]
    expect(last.body).toEqual({ status: 'ready' })
    expect(listBodies.every((l) => !('page_token' in l.body))).toBe(true)
  })

  it('cancels a ready row with a required reason', async () => {
    rows = [
      row({ id: 8, status: 'ready', attempt: 2, subject: { label: 'owner/repo#19', fields: {} } }),
    ]
    render(<Harness />)
    await screen.findByText(/nothing parked/i)
    fireEvent.click(screen.getByRole('button', { name: 'Ready' }))
    await screen.findByText('owner/repo#19')

    const rowEl = screen.getByText('owner/repo#19').closest('tr')!
    // No redrive on a ready row; cancel opens a form whose submit stays
    // disabled until a reason is typed.
    expect(within(rowEl).queryByRole('button', { name: /^redrive/i })).not.toBeInTheDocument()
    fireEvent.click(within(rowEl).getByRole('button', { name: /cancel…/i }))
    const submit = within(rowEl).getByRole('button', { name: /request cancel/i })
    expect(submit).toBeDisabled()
    fireEvent.change(within(rowEl).getByRole('textbox', { name: /cancellation reason/i }), {
      target: { value: '  poison row  ' },
    })
    expect(submit).toBeEnabled()
    fireEvent.click(submit)

    await waitFor(() =>
      expect(cancelBodies).toEqual([{ kind: 'event_queue', ids: [8], reason: 'poison row' }]),
    )
    // The row is still there — a request, not a status change — and says so.
    expect(await screen.findByText('cancel requested')).toBeInTheDocument()
    expect(screen.getByText('owner/repo#19')).toBeInTheDocument()
  })

  it('offers no cancel on a kind without the control', async () => {
    kinds = [kind('event_queue', { controls: { redrive: true, supersede: false, cancel: false } })]
    rows = [row({ id: 8, status: 'leased', subject: { label: 'owner/repo#19', fields: {} } })]
    render(<Harness />)
    await screen.findByText(/nothing parked/i)
    fireEvent.click(screen.getByRole('button', { name: 'Leased' }))
    await screen.findByText('owner/repo#19')
    expect(screen.queryByRole('button', { name: /cancel…/i })).not.toBeInTheDocument()
  })

  it('disables the bulk button until something is selected', async () => {
    rows = [row({ id: 7 })]
    render(<Harness />)
    await screen.findByText('owner/repo#18')

    const bulk = screen.getByRole('button', { name: /redrive selected/i })
    expect(bulk).toBeDisabled()
    fireEvent.click(screen.getByRole('checkbox', { name: /select event_queue item 7/i }))
    expect(screen.getByRole('button', { name: /redrive selected \(1\)/i })).toBeEnabled()
  })
})
