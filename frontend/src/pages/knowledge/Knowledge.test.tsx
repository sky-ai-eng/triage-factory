import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router'

// The Knowledge page's three flows, and the one thing each of them was easy to
// get wrong.
//
//   LIST — the project-era KB panel issued a GET against a route that only
//   registered POST, so it rendered nothing and said nothing. The read here
//   goes through the POST /list convention, and the test asserts the address.
//
//   UPLOAD — which root a document lands in is its visibility, and the one
//   thing re-uploading cannot undo. The page states it before it happens.
//
//   MOVE — publish and unpublish are the same verb. The test asserts the
//   direction, because a from/to pair is trivially transposable and a
//   transposed one silently unpublishes what someone meant to publish.

const api = vi.hoisted(() => ({
  apiJSON: vi.fn(),
  apiFetch: vi.fn(),
  apiList: vi.fn(),
  apiListAll: vi.fn(),
}))
vi.mock('../../lib/apiClient', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/apiClient')>()
  return { ...actual, ...api }
})

const toasts = vi.hoisted(() => ({
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn(), warning: vi.fn() },
}))
vi.mock('../../components/Toast/toastStore', () => toasts)

const session = vi.hoisted(() => ({ present: true }))
vi.mock('../../contexts/AuthContext', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../contexts/AuthContext')>()
  return { ...actual, useOptionalAuth: () => (session.present ? ({} as never) : null) }
})

const roles = vi.hoisted(() => ({ team: 'admin' }))
vi.mock('../../hooks/useTeamRole', () => ({
  useTeamRole: () => ({ roleForTeam: () => roles.team, loading: false }),
}))

const teamState = vi.hoisted(() => ({
  teams: [{ id: 't1', name: 'platform', slug: 'platform' }] as Array<{
    id: string
    name: string
    slug: string
  }>,
}))
vi.mock('../../hooks/useTeams', () => ({
  useTeams: () => ({
    teams: teamState.teams,
    lastActingTeamId: 't1',
    multi: teamState.teams.length >= 2,
    loading: false,
    loaded: true,
    error: null,
    refresh: vi.fn(),
    createTeam: vi.fn(),
  }),
}))

// The socket is a module-level singleton with a real WebSocket behind it; the
// page only ever subscribes, so a no-op keeps the connection out of the test.
const wsHandlers = vi.hoisted(() => ({ current: [] as Array<(e: unknown) => void> }))
vi.mock('../../hooks/useWebSocket', () => ({
  useWebSocket: (h: (e: unknown) => void) => {
    wsHandlers.current = [h]
  },
}))

import Knowledge from './Knowledge'

const FILES = [
  {
    root: 'private',
    path: 'runbooks/deploy.md',
    name: 'deploy.md',
    size: 1200,
    mod_time: '2026-08-20T00:00:00Z',
  },
  {
    root: 'private',
    path: 'architecture.md',
    name: 'architecture.md',
    size: 340,
    mod_time: '2026-08-21T00:00:00Z',
  },
]

function draw() {
  return render(
    <MemoryRouter>
      <Knowledge />
    </MemoryRouter>,
  )
}

beforeEach(() => {
  vi.clearAllMocks()
  session.present = true
  roles.team = 'admin'
  teamState.teams = [{ id: 't1', name: 'platform', slug: 'platform' }]
  api.apiList.mockResolvedValue({ items: FILES, next_page_token: '', total_count: FILES.length })
  api.apiFetch.mockResolvedValue({
    ok: true,
    status: 200,
    json: () => Promise.resolve({ moved: 1 }),
    text: () => Promise.resolve('# Deploy\n'),
  } as unknown as Response)
})

describe('Knowledge — listing', () => {
  it('reads through POST /list on the team, scoped to the selected root', async () => {
    draw()
    await waitFor(() => expect(api.apiList).toHaveBeenCalled())

    const [path, body] = api.apiList.mock.calls[0]
    expect(path).toBe('/api/teams/t1/kb/list')
    expect(body).toMatchObject({ root: 'private' })

    await waitFor(() => expect(screen.getByText('runbooks/deploy.md')).toBeTruthy())
    expect(screen.getByText('architecture.md')).toBeTruthy()
  })

  it('switching root refetches for the other root', async () => {
    draw()
    await waitFor(() => expect(api.apiList).toHaveBeenCalled())
    api.apiList.mockClear()

    fireEvent.click(screen.getByRole('tab', { name: 'SHARED' }))

    await waitFor(() => expect(api.apiList).toHaveBeenCalled())
    expect(api.apiList.mock.calls[0][1]).toMatchObject({ root: 'shared' })
  })

  it('refetches on a ping for this team and this root, and ignores the rest', async () => {
    draw()
    await waitFor(() => expect(api.apiList).toHaveBeenCalled())
    const handler = wsHandlers.current[0]
    api.apiList.mockClear()

    handler({ type: 'team_knowledge_updated', data: { team_id: 'other', root: 'private' } })
    handler({ type: 'team_knowledge_updated', data: { team_id: 't1', root: 'shared' } })
    handler({ type: 'tasks_updated', data: {} })
    expect(api.apiList).not.toHaveBeenCalled()

    handler({ type: 'team_knowledge_updated', data: { team_id: 't1', root: 'private' } })
    await waitFor(() => expect(api.apiList).toHaveBeenCalled())
  })

  it('offers no team switcher to a one-team member', async () => {
    draw()
    await waitFor(() => expect(api.apiList).toHaveBeenCalled())
    expect(screen.queryByRole('combobox')).toBeFalsy()
  })

  it('offers a team switcher to a multi-team member, and reads the picked team', async () => {
    teamState.teams = [
      { id: 't1', name: 'platform', slug: 'platform' },
      { id: 't2', name: 'growth', slug: 'growth' },
    ]
    draw()
    const picker = await waitFor(() => screen.getByRole('combobox'))
    api.apiList.mockClear()

    fireEvent.change(picker, { target: { value: 't2' } })

    await waitFor(() => expect(api.apiList).toHaveBeenCalled())
    expect(api.apiList.mock.calls[0][0]).toBe('/api/teams/t2/kb/list')
  })
})

describe('Knowledge — upload', () => {
  it('names the visibility the files are about to get, then posts to the team', async () => {
    const { container } = draw()
    await waitFor(() => expect(api.apiList).toHaveBeenCalled())

    const input = container.querySelector('input[type=file]') as HTMLInputElement
    const file = new File(['# notes'], 'notes.md', { type: 'text/markdown' })
    Object.defineProperty(input, 'files', { value: [file] })
    fireEvent.change(input)

    // The dialog states where it lands before it happens — the one thing
    // re-uploading cannot undo.
    await waitFor(() => expect(screen.getByText(/Add to private/)).toBeTruthy())
    expect(screen.getByText(/readable by this team/)).toBeTruthy()

    api.apiFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: () =>
        Promise.resolve({
          files: [
            { root: 'private', path: 'notes.md', name: 'notes.md', size: 7, outcome: 'created' },
          ],
        }),
      text: () => Promise.resolve(''),
    } as unknown as Response)

    fireEvent.click(screen.getByRole('button', { name: 'Add' }))

    await waitFor(() => expect(api.apiFetch).toHaveBeenCalled())
    const [path, opts] = api.apiFetch.mock.calls[0]
    expect(path).toBe('/api/teams/t1/kb/files')
    expect(opts.method).toBe('POST')
    const form = opts.body as FormData
    expect(form.get('root')).toBe('private')
    expect(form.getAll('files').length).toBe(1)
  })

  it('reports a replaced document as replaced rather than as a new one', async () => {
    const { container } = draw()
    await waitFor(() => expect(api.apiList).toHaveBeenCalled())

    const input = container.querySelector('input[type=file]') as HTMLInputElement
    Object.defineProperty(input, 'files', { value: [new File(['x'], 'guide.md')] })
    fireEvent.change(input)
    await waitFor(() => expect(screen.getByText(/Add to private/)).toBeTruthy())

    api.apiFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: () =>
        Promise.resolve({
          files: [
            { root: 'private', path: 'guide.md', name: 'guide.md', size: 1, outcome: 'replaced' },
          ],
        }),
      text: () => Promise.resolve(''),
    } as unknown as Response)
    fireEvent.click(screen.getByRole('button', { name: 'Add' }))

    await waitFor(() => expect(toasts.toast.success).toHaveBeenCalled())
    expect(String(toasts.toast.success.mock.calls[0][0])).toContain('replaced')
  })

  // A replace is the one write on this page with nothing behind it — delete and
  // publish are both undoable for ten seconds, and no root keeps history. So the
  // dialog has to name what goes BEFORE the upload runs; the toast afterwards is
  // a receipt, not a warning.
  it('names the document a pick would replace, before the upload runs', async () => {
    const { container } = draw()
    await waitFor(() => expect(api.apiList).toHaveBeenCalled())

    const input = container.querySelector('input[type=file]') as HTMLInputElement
    Object.defineProperty(input, 'files', { value: [new File(['x'], 'architecture.md')] })
    fireEvent.change(input)

    await waitFor(() => expect(screen.getByText(/Add to private/)).toBeTruthy())
    await waitFor(() =>
      expect(screen.getByText('The existing architecture.md will be replaced.')).toBeTruthy(),
    )
    expect(api.apiFetch).not.toHaveBeenCalled()
  })

  it('counts them instead of naming them when a pick would replace several', async () => {
    api.apiList.mockResolvedValue({
      items: [
        { root: 'private', path: 'a.md', name: 'a.md', size: 1, mod_time: '2026-08-20T00:00:00Z' },
        { root: 'private', path: 'b.md', name: 'b.md', size: 1, mod_time: '2026-08-20T00:00:00Z' },
      ],
      next_page_token: '',
      total_count: 2,
    })
    const { container } = draw()
    await waitFor(() => expect(api.apiList).toHaveBeenCalled())

    const input = container.querySelector('input[type=file]') as HTMLInputElement
    Object.defineProperty(input, 'files', {
      value: [new File(['x'], 'a.md'), new File(['y'], 'b.md'), new File(['z'], 'fresh.md')],
    })
    fireEvent.change(input)

    await waitFor(() => expect(screen.getByText(/Add to private/)).toBeTruthy())
    await waitFor(() => expect(screen.getByText('2 existing files will be replaced.')).toBeTruthy())
  })

  it('says nothing about replacement when the pick collides with nothing', async () => {
    const { container } = draw()
    await waitFor(() => expect(api.apiList).toHaveBeenCalled())

    const input = container.querySelector('input[type=file]') as HTMLInputElement
    Object.defineProperty(input, 'files', { value: [new File(['x'], 'brand-new.md')] })
    fireEvent.change(input)

    await waitFor(() => expect(screen.getByText(/Add to private/)).toBeTruthy())
    // The probe is debounced, so let it run and settle before asserting absence.
    await waitFor(() => expect(api.apiList.mock.calls.length).toBeGreaterThan(1))
    expect(screen.queryByText(/will be replaced/)).toBeFalsy()
  })

  // The folder field decides where the files land, so it decides what they
  // collide with. A probe answered for the empty folder must never be rendered
  // against a typed one.
  it('re-resolves what would be replaced when the folder changes', async () => {
    const { container } = draw()
    await waitFor(() => expect(api.apiList).toHaveBeenCalled())

    const input = container.querySelector('input[type=file]') as HTMLInputElement
    Object.defineProperty(input, 'files', { value: [new File(['x'], 'deploy.md')] })
    fireEvent.change(input)
    await waitFor(() => expect(screen.getByText(/Add to private/)).toBeTruthy())

    // At the root, `deploy.md` collides with nothing — the held document is
    // `runbooks/deploy.md`.
    await waitFor(() => expect(api.apiList.mock.calls.length).toBeGreaterThan(1))
    expect(screen.queryByText(/will be replaced/)).toBeFalsy()

    fireEvent.change(screen.getByPlaceholderText('runbooks/deploys'), {
      target: { value: 'runbooks' },
    })

    await waitFor(() =>
      expect(screen.getByText('The existing runbooks/deploy.md will be replaced.')).toBeTruthy(),
    )
    const probe = api.apiList.mock.calls[api.apiList.mock.calls.length - 1][1]
    expect(probe).toMatchObject({ root: 'private', path_prefix: 'runbooks' })
  })
})

describe('Knowledge — the move verb', () => {
  it('names the verb for the direction the current root implies', async () => {
    const { container } = draw()
    await waitFor(() => expect(screen.getByText('architecture.md')).toBeTruthy())

    // Clicking the row selects it — the document name inside is its own
    // control and stops the click, which is what keeps reading and selecting
    // from being the same gesture.
    const rows = container.querySelectorAll('[data-trow]')
    expect(rows.length).toBeGreaterThan(0)
    fireEvent.click(rows[0])

    // Publishing from private and unpublishing from shared are the same move
    // in opposite directions, so the label is the only thing that tells a
    // reader which one they are about to do.
    await waitFor(() => expect(screen.getByRole('button', { name: /^publish/ })).toBeTruthy())
    expect(screen.queryByRole('button', { name: /^unpublish/ })).toBeFalsy()
  })

  it('names the reverse verb on the shared root', async () => {
    const { container } = draw()
    await waitFor(() => expect(api.apiList).toHaveBeenCalled())
    fireEvent.click(screen.getByRole('tab', { name: 'SHARED' }))
    await waitFor(() => expect(screen.getByText('architecture.md')).toBeTruthy())

    fireEvent.click(container.querySelectorAll('[data-trow]')[0])
    await waitFor(() => expect(screen.getByRole('button', { name: /^unpublish/ })).toBeTruthy())
  })
})

describe('Knowledge — the undo window', () => {
  // The Table holds a verb for ten seconds and then calls whatever onCommit is
  // current. The team switcher stays live the whole time (the undo bar is
  // inline, not modal), so a commit that resolved the team lazily would fire
  // against whichever team was selected when the window closed — silently
  // mutating a same-named document in a team the reader never touched.
  it('commits against the team the row was drawn under, not the one now selected', async () => {
    teamState.teams = [
      { id: 't1', name: 'platform', slug: 'platform' },
      { id: 't2', name: 'growth', slug: 'growth' },
    ]
    vi.useFakeTimers()
    try {
      const { container } = draw()
      await vi.waitFor(() => expect(screen.getByText('architecture.md')).toBeTruthy())

      fireEvent.click(container.querySelectorAll('[data-trow]')[0])
      const publish = await vi.waitFor(() => screen.getByRole('button', { name: /^publish/ }))
      fireEvent.pointerDown(publish)
      fireEvent.pointerUp(publish)

      // Switch teams while the verb is still inside its window.
      fireEvent.change(screen.getByRole('combobox'), { target: { value: 't2' } })
      await vi.advanceTimersByTimeAsync(11000)

      const move = api.apiFetch.mock.calls.find((c) => String(c[0]).endsWith('/kb/move'))
      expect(move, 'the held verb was never sent').toBeTruthy()
      expect(String(move?.[0])).toBe('/api/teams/t1/kb/move')
    } finally {
      vi.useRealTimers()
    }
  })

  it('closes the open document when the team changes', async () => {
    teamState.teams = [
      { id: 't1', name: 'platform', slug: 'platform' },
      { id: 't2', name: 'growth', slug: 'growth' },
    ]
    draw()
    await waitFor(() => expect(screen.getByText('architecture.md')).toBeTruthy())

    fireEvent.click(screen.getByRole('button', { name: 'architecture.md' }))
    await waitFor(() => expect(screen.getByRole('dialog')).toBeTruthy())

    fireEvent.change(screen.getByRole('combobox'), { target: { value: 't2' } })
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeFalsy())
  })
})

describe('Knowledge — roles', () => {
  it('offers a viewer no write verbs at all', async () => {
    session.present = true
    roles.team = 'viewer'
    const { container } = draw()
    await waitFor(() => expect(api.apiList).toHaveBeenCalled())

    expect(screen.queryByRole('button', { name: '+ upload' })).toBeFalsy()
    expect(container.querySelector('input[type=file]')).toBeFalsy()
    // Selection exists for the verbs, so a viewer gets no checkboxes either.
    expect(container.querySelectorAll('[role=checkbox]').length).toBe(0)
  })

  it('gives local mode the write verbs — one person who is every role', async () => {
    session.present = false
    roles.team = ''
    draw()
    await waitFor(() => expect(api.apiList).toHaveBeenCalled())
    expect(screen.getByRole('button', { name: '+ upload' })).toBeTruthy()
  })
})
