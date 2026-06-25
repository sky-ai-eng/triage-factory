import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import AgentCard from './AgentCard'
import type { AgentRun, Task } from '../types'

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

const run = (over: Partial<AgentRun>): AgentRun =>
  ({
    ID: 'r1',
    TaskID: 't1',
    Status: 'completed',
    Model: 'claude-opus-4-8',
    StartedAt: '2026-06-25T00:00:00Z',
    ResultSummary: 'Done.',
    ...over,
  }) as AgentRun

function renderCard(over: Partial<AgentRun>, onOpenArtifact = vi.fn()) {
  render(
    <MemoryRouter>
      <AgentCard task={task} run={run(over)} messages={[]} onOpenArtifact={onOpenArtifact} />
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
    expect(globalThis.fetch).toHaveBeenCalledWith('/api/agent/runs/r1/artifacts')
  })

  it('uses the singular noun for a single artifact', () => {
    renderCard({ artifact_count: 1 })
    expect(screen.getByRole('button', { name: /1 artifact\b/ })).toBeInTheDocument()
  })
})
