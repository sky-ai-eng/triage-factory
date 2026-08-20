import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import ProjectEntitiesPanel from './ProjectEntitiesPanel'
import ProjectBackfillModal from './ProjectBackfillModal'
import { listBody } from '../test/apiResponse'

// Both surfaces read a project's rows through a `POST /…/list` route. They were
// left on the retired bare-array GETs when those became list reads, which no
// type or lint could see — a path is a string — so what these tests pin is the
// path each one actually sends, and that the modal walks past the first page.

vi.mock('../hooks/useWebSocket', () => ({ useWebSocket: () => {} }))
vi.mock('./Toast/toastStore', () => ({ toast: { error: vi.fn(), info: vi.fn() } }))

/** The paths a render asked for, in order. */
let paths: string[] = []

function stubFetch(answer: (path: string, page: number) => unknown) {
  const seen = new Map<string, number>()
  vi.stubGlobal(
    'fetch',
    vi.fn().mockImplementation((path: string) => {
      paths.push(path)
      const n = (seen.get(path) ?? 0) + 1
      seen.set(path, n)
      return Promise.resolve({ ok: true, status: 200, ...(answer(path, n) as object) })
    }),
  )
}

function candidate(id: string) {
  return {
    id,
    source: 'github',
    source_id: id,
    kind: 'pull_request',
    title: `title ${id}`,
    url: '',
    state: 'open',
    current_project_id: '',
    current_project_name: '',
  }
}

beforeEach(() => {
  paths = []
})
afterEach(() => {
  vi.unstubAllGlobals()
  vi.clearAllMocks()
})

describe('ProjectEntitiesPanel', () => {
  it('reads the entities list route and renders the page', async () => {
    stubFetch(() =>
      listBody([
        {
          id: 'e1',
          source: 'github',
          source_id: 'acme/api#7',
          kind: 'pull_request',
          title: 'a pull request',
          url: '',
          state: 'open',
          last_polled_at: null,
          created_at: '2026-08-20T00:00:00Z',
        },
      ]),
    )

    render(<ProjectEntitiesPanel projectId="p1" />)

    await waitFor(() => expect(screen.getByText('acme/api#7')).toBeInTheDocument())
    expect(paths).toEqual(['/api/projects/p1/entities/list'])
  })
})

describe('ProjectBackfillModal', () => {
  it('reads the candidates list route and walks every page', async () => {
    // Two pages: the modal partitions the whole set client-side and selects
    // across it, so stopping at the first page would hide a real candidate.
    stubFetch((_path, page) =>
      page === 1 ? listBody([candidate('c1')], 'tok', 2) : listBody([candidate('c2')], '', 2),
    )

    render(<ProjectBackfillModal projectId="p1" projectName="Proj" onClose={() => {}} />)

    await waitFor(() => expect(screen.getByText('title c2')).toBeInTheDocument())
    expect(screen.getByText('title c1')).toBeInTheDocument()
    expect(paths).toEqual([
      '/api/projects/p1/backfill-candidates/list',
      '/api/projects/p1/backfill-candidates/list',
    ])
  })
})
