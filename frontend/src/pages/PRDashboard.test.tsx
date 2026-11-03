import { describe, it, expect, vi, beforeEach } from 'vitest'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import PRDashboard from './PRDashboard'
import type { PRSummary } from './PRDashboard'
import { jsonBody, listBody } from '../test/apiResponse'

vi.mock('../hooks/useWebSocket', () => ({ useWebSocket: () => {} }))

const pr = (n: number, over: Partial<PRSummary> = {}): PRSummary => ({
  entity_id: `ent-${n}`,
  number: n,
  title: `PR ${n}`,
  repo: 'acme/widget',
  author: 'octocat',
  state: 'open',
  draft: false,
  labels: [],
  created_at: '2026-10-01T00:00:00Z',
  updated_at: '2026-10-01T00:00:00Z',
  html_url: `https://github.com/acme/widget/pull/${n}`,
  ...over,
})

const STATS = {
  merged: 0,
  closed: 0,
  awaiting: 0,
  draft: 0,
  reviews_given: 0,
  reviews_received: 0,
  merged_over_time: [],
}

type Body = Record<string, unknown>

// stubBoard answers each column's list read by its body, recording every
// list body. The rows it answers are deliberately ones a client-side filter or
// sort would move or drop: the server decides what a column holds and in what
// order, so the board renders exactly what it is given.
function stubBoard(answer: (body: Body) => PRSummary[]) {
  const bodies: Body[] = []
  const fetchMock = vi.fn().mockImplementation((url: string, init?: RequestInit) => {
    if (String(url) === '/api/dashboard/stats') {
      return Promise.resolve({ ok: true, ...jsonBody(STATS) })
    }
    const body = JSON.parse(String(init?.body ?? '{}')) as Body
    bodies.push(body)
    const rows = answer(body)
    return Promise.resolve({ ok: true, ...listBody(rows, '', rows.length + 10) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return bodies
}

const isColumn = (b: Body, states: string[], draft?: boolean) =>
  JSON.stringify(b.states) === JSON.stringify(states) && b.draft === draft

// column finds a column by its heading and returns its titles in order.
function columnTitles(heading: string): string[] {
  const h = screen.getByRole('heading', { name: new RegExp(`^${heading}`) })
  const col = h.parentElement as HTMLElement
  return within(col)
    .queryAllByText(/^PR \d+$/)
    .map((el) => el.textContent ?? '')
}

describe('PRDashboard', () => {
  beforeEach(() => {
    sessionStorage.clear()
    vi.restoreAllMocks()
  })

  it('reads each column as its own list, newest activity first, and renders what each answers', async () => {
    const bodies = stubBoard((b) => {
      if (isColumn(b, ['open'], false)) {
        // Older first, and one marked draft: neither is the board's to fix.
        return [
          pr(1, { updated_at: '2026-01-01T00:00:00Z' }),
          pr(2, { draft: true, updated_at: '2026-10-01T00:00:00Z' }),
        ]
      }
      if (isColumn(b, ['open'], true)) return [pr(3, { draft: false })]
      if (isColumn(b, ['merged'])) return [pr(4, { state: 'closed' })]
      if (isColumn(b, ['closed'])) return [pr(5, { state: 'merged' })]
      return []
    })

    render(<PRDashboard />)

    await waitFor(() => expect(columnTitles('Ready for review')).toEqual(['PR 1', 'PR 2']))
    expect(columnTitles('Drafts')).toEqual(['PR 3'])
    expect(columnTitles('Recently merged')).toEqual(['PR 4'])
    expect(columnTitles('Closed')).toEqual(['PR 5'])

    // Four reads, one per column, each carrying its narrowing and the
    // server's order.
    expect(bodies).toHaveLength(4)
    expect(bodies).toEqual(
      expect.arrayContaining([
        { states: ['open'], draft: false, sort_key: 'updated_at', page_size: 50 },
        { states: ['open'], draft: true, sort_key: 'updated_at', page_size: 50 },
        { states: ['merged'], sort_key: 'updated_at', page_size: 50 },
        { states: ['closed'], sort_key: 'updated_at', page_size: 50 },
      ]),
    )
    // A column's count is its list's total, not the rows it holds.
    const ready = screen.getByRole('heading', { name: /^Ready for review/ })
    expect(ready.textContent).toContain('12')
  })

  it("pages one column under that column's own filters", async () => {
    const bodies: Body[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation((url: string, init?: RequestInit) => {
        if (String(url) === '/api/dashboard/stats') {
          return Promise.resolve({ ok: true, ...jsonBody(STATS) })
        }
        const body = JSON.parse(String(init?.body ?? '{}')) as Body
        bodies.push(body)
        if (!isColumn(body, ['open'], true)) return Promise.resolve({ ok: true, ...listBody([]) })
        return Promise.resolve({
          ok: true,
          ...(body.page_token === 'drafts-2'
            ? listBody([pr(21, { draft: true })], '', 2)
            : listBody([pr(20, { draft: true })], 'drafts-2', 2)),
        })
      }),
    )

    render(<PRDashboard />)
    fireEvent.click(await screen.findByRole('button', { name: 'Load more drafts' }))

    await waitFor(() => expect(columnTitles('Drafts')).toEqual(['PR 20', 'PR 21']))
    expect(bodies).toContainEqual({
      states: ['open'],
      draft: true,
      sort_key: 'updated_at',
      page_size: 50,
      page_token: 'drafts-2',
    })
    expect(screen.queryByRole('button', { name: /^Load more/ })).toBeNull()
  })
})
