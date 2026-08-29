import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import Overview from './Overview'
import type { Conversation } from '../types'

// The Overview's claims, checked against what it actually calls: the away line
// anchors to the viewer's own marker and stamps a new one; NEEDS YOU and
// RUNNING are the conversations resource's own filters rendered as rows that
// navigate to their runs; the quiet state is an answer stated in words;
// offline inerts every readout rather than holding stale numbers.

const api = vi.hoisted(() => ({
  apiJSON: vi.fn(),
  apiList: vi.fn(),
}))
vi.mock('../lib/apiClient', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/apiClient')>()
  return { ...actual, ...api }
})

// The page's scope comes from the shell's org · team mark, via outlet context.
vi.mock('../hooks/useShellScope', () => ({
  useShellScope: () => ({ teamId: 't1', teamName: 'platform' }),
}))

// Local-shaped hrefs so the assertions read as paths, not org prefixes.
vi.mock('../hooks/useOrgHref', () => ({ useOrgHref: () => (p: string) => p }))

// The stream: quiet by default, with the connected flag mutable per test.
const ws = vi.hoisted(() => ({ connected: true }))
vi.mock('../hooks/useWebSocket', () => ({
  useWebSocket: () => {},
  useWsConnected: () => ws.connected,
}))

vi.mock('../hooks/useModelCatalog', () => ({
  useModelCatalog: () => ({
    models: [
      { key: 'claude-opus-5', display_name: 'Claude Opus 5', provider: 'anthropic' },
      { key: 'claude-sonnet-5', display_name: 'Claude Sonnet 5', provider: 'anthropic' },
    ],
    loaded: true,
  }),
}))

function conv(over: Partial<Conversation>): Conversation {
  return {
    ID: 'c1',
    TaskID: 't-a',
    Status: 'running',
    Model: 'claude-opus-5',
    StartedAt: new Date(Date.now() - 11 * 60_000).toISOString(),
    ResultSummary: '',
    ...over,
  } as Conversation
}

const TASKS = [
  {
    id: 't-a',
    source: 'github',
    source_id: 'acme/factory-api#761',
    title: 'Harden the sampler test',
    status: 'in_progress',
  },
  {
    id: 't-b',
    source: 'jira',
    source_id: 'SKY-412',
    title: 'Triage the flaking rebalance test',
    status: 'claimed',
  },
]

/** The page's reads, keyed by what each one is. Tests override pieces. */
const answers = vi.hoisted(() => ({
  needs: { runs: {} as Record<string, Conversation[]>, total_count: 0 },
  running: { runs: {} as Record<string, Conversation[]>, total_count: 0 },
  activity: { by_day: [{ date: '2026-08-29', events: 312, tasks: 16, merged: 6, failed: 2 }] },
  usage: {
    total_cost_usd: 41.2,
    by_user: [],
    by_model: [
      { model: 'claude-opus-5', cost: 24.8 },
      { model: 'claude-sonnet-5', cost: 13.1 },
    ],
  },
  seenAt: '2026-08-28T18:40:00Z' as string | null,
}))

beforeEach(() => {
  ws.connected = true
  answers.needs = { runs: {}, total_count: 0 }
  answers.running = { runs: {}, total_count: 0 }
  answers.seenAt = new Date(Date.now() - 26 * 3600_000).toISOString()
  api.apiJSON.mockReset()
  api.apiList.mockReset()
  api.apiJSON.mockImplementation((path: string, options?: { method?: string; body?: string }) => {
    if (path === '/api/me/settings') {
      if (options?.method === 'PATCH') {
        return Promise.resolve({ user_settings: { overview_seen_at: '2026-08-29T12:00:00Z' } })
      }
      return Promise.resolve({ user_settings: { overview_seen_at: answers.seenAt } })
    }
    if (path === '/api/agent/conversations/list') {
      const body = JSON.parse(options?.body ?? '{}') as { attention?: boolean }
      return Promise.resolve(body.attention ? answers.needs : answers.running)
    }
    if (path.includes('/activity?since=')) return Promise.resolve(answers.activity)
    if (path.includes('/usage?since=')) return Promise.resolve(answers.usage)
    return Promise.reject(new Error('unexpected apiJSON ' + path))
  })
  api.apiList.mockImplementation((path: string) => {
    if (path === '/api/tasks/list') {
      return Promise.resolve({ items: TASKS, next_page_token: '', total_count: TASKS.length })
    }
    return Promise.reject(new Error('unexpected apiList ' + path))
  })
})

function mount() {
  return render(
    <MemoryRouter>
      <Overview />
    </MemoryRouter>,
  )
}

describe('the away line', () => {
  it('anchors to the prior marker, sums the window since it, and stamps a new one', async () => {
    mount()
    await waitFor(() =>
      expect(screen.getByText(/You were last here at \d{2}:\d{2} yesterday\./)).toBeInTheDocument(),
    )
    // The tail's window opens at the anchor, so its read fires only after the
    // marker resolves — its own wait, not a ride on the lead's.
    expect(await screen.findByText('6 merged · 2 failed · 296 filtered since')).toBeInTheDocument()
    // The stamp is an explicit PATCH — reads are side-effect-free on this API.
    const patch = api.apiJSON.mock.calls.find(
      ([path, opts]) => path === '/api/me/settings' && opts?.method === 'PATCH',
    )
    expect(patch).toBeTruthy()
    const body = JSON.parse((patch![1] as { body: string }).body) as {
      user_settings: { overview_seen_at: string }
    }
    expect(Number.isFinite(Date.parse(body.user_settings.overview_seen_at))).toBe(true)
  })

  it('anchors to midnight, and says so, before a marker exists', async () => {
    answers.seenAt = null
    mount()
    await waitFor(() =>
      expect(screen.getByText('Your first look since midnight.')).toBeInTheDocument(),
    )
  })
})

describe('NEEDS YOU', () => {
  it('renders the attention set as warm rows that navigate to their runs', async () => {
    answers.needs = {
      runs: {
        't-a': [
          conv({
            ID: 'c-fail',
            Status: 'failed',
            FailureKind: 'memory_limit',
            CompletedAt: new Date(Date.now() - 18 * 3600_000).toISOString(),
          }),
        ],
        't-b': [conv({ ID: 'c-pr', TaskID: 't-b', Status: 'open', unresolved_pr_count: 1 })],
      },
      total_count: 7,
    }
    mount()
    await waitFor(() => expect(screen.getByText('Killed at the memory limit')).toBeInTheDocument())
    expect(screen.getByText('Approve the draft pull request')).toBeInTheDocument()
    // The reference is the source's own spelling — GitHub without the owner.
    expect(screen.getByText('factory-api#761')).toBeInTheDocument()
    expect(screen.getByText('SKY-412')).toBeInTheDocument()
    // Every row's one target is its run.
    const row = screen.getByText('Approve the draft pull request').closest('a')
    expect(row).toHaveAttribute('href', '/runs/c-pr')
    // Two shown of seven: the rest are the Board's.
    expect(screen.getByText('+5 more on the board')).toBeInTheDocument()
  })

  it('states the quiet answer in words when nothing needs you', async () => {
    answers.needs = { runs: {}, total_count: 0 }
    mount()
    await waitFor(() => expect(screen.getByText('Nothing needs you.')).toBeInTheDocument())
    // The answer is a sentence, not a band of figures — the day's numbers
    // live in the ring and the convergence, not here.
    expect(screen.queryByText('OPEN PRS')).not.toBeInTheDocument()
    expect(screen.queryByText('FILTERED')).not.toBeInTheDocument()
  })
})

describe('RUNNING', () => {
  it('leads with the agent action, shimmers only the working row, and marks the queued wait', async () => {
    answers.running = {
      runs: {
        't-a': [
          conv({
            ID: 'c-work',
            current_action: 'Replaying 6 commits onto origin/main',
            ClaimedAt: new Date(Date.now() - 4 * 60_000).toISOString(),
          }),
        ],
        't-b': [
          conv({
            ID: 'c-q',
            TaskID: 't-b',
            Status: 'queued',
            QueuedAt: new Date(Date.now() - 60_000).toISOString(),
            queue_position: 3,
          }),
        ],
      },
      total_count: 4,
    }
    mount()
    const act = await screen.findByText('Replaying 6 commits onto origin/main')
    expect(act.className).toContain('rr-shim')
    // The queued run's prose names the work; the wait is the mark.
    const queued = screen.getByText('Triage the flaking rebalance test')
    expect(queued.className).not.toContain('rr-shim')
    expect(screen.getByTitle('3 ahead in the queue')).toBeInTheDocument()
    // Two rendered of four: the tail defers to the Board.
    expect(screen.getByText('+2 more on the board')).toBeInTheDocument()
  })

  it('splits the day spend by model with the catalog names', async () => {
    mount()
    await waitFor(() => expect(screen.getByText('Claude Opus 5')).toBeInTheDocument())
    expect(screen.getByText('Claude Sonnet 5')).toBeInTheDocument()
    expect(screen.getByText('$24.80')).toBeInTheDocument()
    expect(screen.getByText('$13.10')).toBeInTheDocument()
    // The hole shows the day's WHOLE figure — scoped, because the quiet band
    // (this fixture has nothing needing you) states the same figure.
    const hole = document.querySelector('.ov-hole') as HTMLElement
    expect(within(hole).getByText('$41.20')).toBeInTheDocument()
  })
})

describe('offline', () => {
  it('inerts every readout instead of holding the last number', async () => {
    ws.connected = false
    mount()
    await waitFor(() =>
      expect(
        screen.getByText('readouts are inert until the connection returns'),
      ).toBeInTheDocument(),
    )
    // The hole reads --, not a stale figure; the fan keeps its outcome names
    // with zeroed counts rather than vanishing.
    expect(screen.queryByText('$41.20')).not.toBeInTheDocument()
    expect(screen.getByText('-- events triaged')).toBeInTheDocument()
    expect(screen.getByText('filtered by rules')).toBeInTheDocument()
  })
})
