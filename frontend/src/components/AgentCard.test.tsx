import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import AgentCard from './AgentCard'
import { QUEUE_DWELL_VISIBLE_MS } from '../lib/runStatus'
import type { Conversation, Task } from '../types'

// useOrgHref pulls in deployment-config + org-context fetches we don't need
// here; the identity resolver keeps the card's Links/hrefs simple.
vi.mock('../hooks/useOrgHref', () => ({ useOrgHref: () => (p: string) => p }))

const task: Task = {
  id: 't1',
  title: 'Fix the flaky test',
  source: 'github',
  source_id: 'org/repo#18',
  source_url: 'https://gh/pr/18',
  entity_kind: 'pr',
  event_type: 'ci_check_failed',
} as unknown as Task

const run = (over: Partial<Conversation>): Conversation =>
  ({
    ID: 'r1',
    TaskID: 't1',
    Status: 'completed',
    Model: 'claude-opus-4-8',
    StartedAt: '2026-06-25T00:00:00Z',
    ResultSummary: 'Done.',
    ...over,
  }) as Conversation

function renderCard(over: Partial<Conversation>, onOpenArtifact = vi.fn()) {
  render(
    <MemoryRouter>
      <AgentCard task={task} run={run(over)} onOpenArtifact={onOpenArtifact} />
    </MemoryRouter>,
  )
  return onOpenArtifact
}

describe('AgentCard artifacts affordance', () => {
  beforeEach(() => vi.restoreAllMocks())
  afterEach(() => vi.unstubAllGlobals())

  it('hides the affordance when the run produced no artifacts', () => {
    renderCard({ artifact_count: 0 })
    expect(screen.queryByRole('button', { name: /artifact/ })).not.toBeInTheDocument()
  })

  it('shows the count and opens a popover that lists the run’s artifacts', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        json: () =>
          Promise.resolve([
            {
              id: 'pr1',
              kind: 'pull_request',
              provider: 'github',
              state: 'draft',
              target: 'org/repo#18',
              external_id: '18',
              url: 'https://gh/pr',
              details: null,
              created_at: '2026-06-25T00:00:00Z',
            },
          ]),
      }),
    )
    renderCard({ artifact_count: 2 })

    // Footer shows "2 artifacts →"; the list is NOT fetched until the popover opens.
    const trigger = screen.getByRole('button', { name: /2 artifacts/ })
    expect(globalThis.fetch).not.toHaveBeenCalled()

    fireEvent.click(trigger)

    // Popover lazy-fetches and renders the list.
    expect(await screen.findByText('org/repo#18')).toBeInTheDocument()
    expect(globalThis.fetch).toHaveBeenCalledWith('/api/agent/conversations/r1/artifacts')
  })

  it('forwards a PR row to onOpenArtifact and closes the popover', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        json: () =>
          Promise.resolve([
            {
              id: 'pr1',
              kind: 'pull_request',
              provider: 'github',
              state: 'draft',
              target: 'org/repo#18',
              external_id: '18',
              url: 'https://gh/pr',
              details: null,
              created_at: '2026-06-25T00:00:00Z',
            },
          ]),
      }),
    )
    const onOpenArtifact = renderCard({ artifact_count: 1 })

    const trigger = screen.getByRole('button', { name: /1 artifact\b/ })
    fireEvent.click(trigger)
    expect(trigger).toHaveAttribute('aria-expanded', 'true')
    fireEvent.click(await screen.findByRole('button', { name: /Open pull request/ }))

    // The adapter delegates to onOpenArtifact (overlay opens)…
    expect(onOpenArtifact).toHaveBeenCalledWith('pr', 'pr1')
    // …and closes the popover. (Asserted via the trigger's aria-expanded rather
    // than the content unmounting: under jsdom Radix's Presence keeps the
    // content node alive waiting on the never-firing exit animation, but the
    // controlled open state — and thus the trigger — flips immediately.)
    await waitFor(() => expect(trigger).toHaveAttribute('aria-expanded', 'false'))
  })

  it('uses the singular noun for a single artifact', () => {
    renderCard({ artifact_count: 1 })
    expect(screen.getByRole('button', { name: /1 artifact\b/ })).toBeInTheDocument()
  })
})

describe('AgentCard failure-kind rendering', () => {
  it('renders the memory-limit verdict + knob copy for a summaryless killed run', () => {
    // failRun writes no ResultSummary — the kind alone must surface the block.
    renderCard({ Status: 'failed', ResultSummary: '', FailureKind: 'memory_limit' })
    expect(screen.getByText('Killed: memory limit')).toBeInTheDocument()
    expect(screen.getByText(/TF_RUN_MEMORY_LIMIT_MB/)).toBeInTheDocument()
  })

  it('keeps the generic Failed verdict for an unclassified failure with a summary', () => {
    renderCard({
      Status: 'failed',
      ResultSummary: 'agent did not return a valid completion envelope',
    })
    expect(screen.getByText('Failed')).toBeInTheDocument()
    expect(screen.queryByText(/memory limit/i)).not.toBeInTheDocument()
  })
})

describe('AgentCard queued rendering', () => {
  it('names the wait — queued for a slot — instead of showing a dead card', () => {
    renderCard({ Status: 'queued' })
    expect(screen.getByText(/queued — starts when a run slot frees up/)).toBeInTheDocument()
    // The tooltip names the knob, so a stalled-looking burst of delegations
    // traces back to the concurrency cap without reading the docs.
    expect(screen.getByTitle(/TF_MAX_CONCURRENT_RUNS/)).toBeInTheDocument()
  })

  it('keeps the queued notice off active and terminal cards', () => {
    renderCard({ Status: 'running' })
    expect(screen.queryByText(/run slot/)).not.toBeInTheDocument()
  })
})

describe('AgentCard queue-dwell footer', () => {
  it('shows how long a started run waited in the queue', () => {
    renderCard({
      Status: 'running',
      QueuedAt: '2026-06-25T00:00:00Z',
      ClaimedAt: '2026-06-25T00:06:00Z',
    })
    expect(screen.getByText(/queued 6m 0s/)).toBeInTheDocument()
  })

  it('stays quiet for ordinary dispatch latency below the shared threshold', () => {
    // Pin the edge just under QUEUE_DWELL_VISIBLE_MS — this dwell sat in the
    // 1–5s window where the card and the telemetry rail once disagreed.
    renderCard({
      Status: 'running',
      QueuedAt: '2026-06-25T00:00:00.000Z',
      ClaimedAt: new Date(
        new Date('2026-06-25T00:00:00.000Z').getTime() + QUEUE_DWELL_VISIBLE_MS - 1,
      ).toISOString(),
    })
    expect(screen.queryByText(/^queued /)).not.toBeInTheDocument()
  })

  it('hides the dwell for legacy rows where it is unknowable', () => {
    renderCard({ Status: 'completed', DurationMs: 120000 })
    expect(screen.queryByText(/^queued /)).not.toBeInTheDocument()
  })
})
