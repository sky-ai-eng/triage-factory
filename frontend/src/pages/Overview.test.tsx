import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import Overview from './Overview'
import { HttpError } from '../lib/apiClient'
import type { Conversation } from '../types'

// The Overview's claims, checked against what it actually calls: NEEDS YOU
// and RUNNING are the conversations resource's own filters rendered as rows
// that navigate to their runs; the pile is the team PRs list's count-only
// read; the ring is the usage node's model split with no legend; the quiet
// state is an answer stated in words; offline inerts every readout rather
// than holding stale numbers.

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
  } as unknown,
  usageStatus: 200,
  openPRs: { total_count: 23 },
}))

beforeEach(() => {
  ws.connected = true
  answers.needs = { runs: {}, total_count: 0 }
  answers.running = { runs: {}, total_count: 0 }
  answers.usageStatus = 200
  answers.openPRs = { total_count: 23 }
  api.apiJSON.mockReset()
  api.apiList.mockReset()
  api.apiJSON.mockImplementation(
    async (path: string, options?: { method?: string; body?: string }) => {
      if (path === '/api/agent/conversations/list') {
        const body = JSON.parse(options?.body ?? '{}') as { attention?: boolean }
        return body.attention ? answers.needs : answers.running
      }
      if (path === '/api/teams/t1/prs/list') {
        const body = JSON.parse(options?.body ?? '{}') as { states?: string[]; page_size?: number }
        // The pile is the count-only read: page_size 0 under the open filter.
        expect(body).toEqual({ states: ['open'], page_size: 0 })
        return answers.openPRs
      }
      if (path.includes('/activity?since=')) return answers.activity
      if (path.includes('/usage?since=')) {
        if (answers.usageStatus !== 200) throw new HttpError(answers.usageStatus, 'forbidden')
        return answers.usage
      }
      throw new Error('unexpected apiJSON ' + path)
    },
  )
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

describe('the masthead', () => {
  it('states where you are, with the clock beside it', async () => {
    mount()
    expect(screen.getByText('Triage Factory')).toBeInTheDocument()
    expect(document.querySelector('.ov-mast-clock')!.textContent).toMatch(/^\d{2}:\d{2}$/)
    expect(document.querySelector('.ov-mast-date')!.textContent).toMatch(
      /^[A-Z]{3} \d{1,2} [A-Z]{3}$/,
    )
    // The away line is gone; its slot carries only the offline notice, so a
    // connected page renders nothing there.
    await waitFor(() => expect(api.apiJSON).toHaveBeenCalled())
    expect(document.querySelector('.ov-away')).toBeNull()
    // And nothing reads or stamps a last-seen marker any more.
    expect(
      api.apiJSON.mock.calls.filter(([path]) => String(path).includes('/api/me/settings')),
    ).toHaveLength(0)
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
    // The reference leads the row in its own column, number split for pinning.
    expect(document.querySelector('.rr-lead-ref')).not.toBeNull()
    const refs = [...document.querySelectorAll('.rr-row > .rr-ref')].map((el) => el.textContent)
    expect(refs).toContain('factory-api#761')
    expect(screen.getByText('SKY-412')).toBeInTheDocument()
    // Every row's one target is its run.
    const row = screen.getByText('Approve the draft pull request').closest('a')
    expect(row).toHaveAttribute('href', '/runs/c-pr')
    // Two shown of seven: the rest are the Board's, via the aligned more link.
    const more = screen.getByText('+5 more on the board')
    expect(more.closest('a')).toHaveAttribute('href', '/board')
    expect(more.closest('.rr-more')).not.toBeNull()
  })

  it('states the quiet answer in words when nothing needs you', async () => {
    answers.needs = { runs: {}, total_count: 0 }
    mount()
    await waitFor(() => expect(screen.getByText('Nothing needs you.')).toBeInTheDocument())
    // The answer is a sentence — no figure strip, and no errand to the board
    // with no reason attached.
    expect(screen.queryByText('open the board')).not.toBeInTheDocument()
  })
})

describe('the pile', () => {
  it('draws the standing open-PR backlog from the count-only read, as a link to the PR page', async () => {
    mount()
    await waitFor(() =>
      expect(screen.getByRole('img', { name: '23 open pull requests' })).toBeInTheDocument(),
    )
    expect(document.querySelectorAll('.cp-crate')).toHaveLength(23)
    expect(document.querySelector('a.cp')).toHaveAttribute('href', '/prs')
  })
})

describe('RUNNING', () => {
  it('leads with the agent action, scans only the working row, and marks the queued wait', async () => {
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
    // The sweep is ui/scan — emission, an agent acting.
    expect(act).toHaveAttribute('data-tone')
    // The queued run's prose names the work; the wait is the mark, speaking
    // places-ahead (position 3 in line = 2 ahead).
    const queued = screen.getByText('Triage the flaking rebalance test')
    expect(queued).not.toHaveAttribute('data-tone')
    expect(screen.getByRole('img', { name: '2 runs ahead of this one' })).toBeInTheDocument()
    // Two rendered of four: the tail defers to the Board.
    expect(screen.getByText('+2 more on the board')).toBeInTheDocument()
  })
})

describe('the ring', () => {
  it('shows the model-attributed total in its hole, splitting only on hover — no legend', async () => {
    mount()
    await waitFor(() => expect(document.querySelector('.sr-total')).toHaveTextContent('$37.90'))
    expect(document.querySelector('.sr-cap')).toHaveTextContent('SPENT TODAY')
    expect(document.querySelectorAll('.sr-seg')).toHaveLength(2)
    // No legend anywhere: the model names appear only in the hole, on hover.
    expect(screen.queryByText('OPUS-5')).not.toBeInTheDocument()
    expect(document.querySelector('a.sr')).toHaveAttribute('href', '/usage')
  })

  it('removes the ring entirely when the grant refuses spend — one fewer answer, not a dash forever', async () => {
    answers.usageStatus = 403
    mount()
    await waitFor(() =>
      expect(api.apiJSON.mock.calls.some(([path]) => String(path).includes('/usage?since='))).toBe(
        true,
      ),
    )
    await waitFor(() => expect(document.querySelector('.ov-ringbox')).toBeNull())
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
    // The headline figure is an em dash; the ring reads --; the pile draws the
    // inert pallet; the fan keeps its outcome names with zeroed counts rather
    // than vanishing.
    expect(
      within(document.querySelector('.convtitle') as HTMLElement).getByText('—'),
    ).toBeInTheDocument()
    expect(document.querySelector('.sr-total')).toHaveTextContent('--')
    expect(document.querySelector('.cp-ghost')).not.toBeNull()
    expect(screen.getByText('filtered by rules')).toBeInTheDocument()
  })
})
