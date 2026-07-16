import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { TelemetryRail } from './StationInstruments'
import { stationState } from './stationStyle'
import { QUEUE_DWELL_VISIBLE_MS } from '../../lib/runStatus'
import type { AgentRun } from '../../types'

const T0 = new Date('2026-06-25T00:00:00Z').getTime()
const iso = (offsetMs: number) => new Date(T0 + offsetMs).toISOString()

const run = (over: Partial<AgentRun>): AgentRun =>
  ({
    ID: 'r1',
    TaskID: 't1',
    Status: 'running',
    Model: 'claude-opus-4-8',
    StartedAt: iso(0),
    ResultSummary: '',
    ...over,
  }) as AgentRun

function renderRail(over: Partial<AgentRun>, now: number = T0 + 60_000) {
  const r = run(over)
  render(<TelemetryRail run={r} messages={[]} state={stationState(r)} now={now} />)
}

// The rail's settled queued readout must share AgentCard's visibility
// threshold (QUEUE_DWELL_VISIBLE_MS) — the two surfaces previously used
// different magic numbers and disagreed for short waits.
describe('TelemetryRail queued readout', () => {
  it('hides a settled dwell below the shared threshold (normal dispatch latency)', () => {
    renderRail({
      QueuedAt: iso(0),
      ClaimedAt: iso(QUEUE_DWELL_VISIBLE_MS - 1),
    })
    expect(screen.queryByText('queued')).not.toBeInTheDocument()
  })

  it('shows a settled dwell at the shared threshold', () => {
    renderRail({
      QueuedAt: iso(0),
      ClaimedAt: iso(QUEUE_DWELL_VISIBLE_MS),
    })
    expect(screen.getByText('queued')).toBeInTheDocument()
  })

  it('always shows the live wait while the run is still queued, even below the threshold', () => {
    renderRail({ Status: 'queued', QueuedAt: iso(0) }, T0 + 2000)
    expect(screen.getByText('queued')).toBeInTheDocument()
    expect(screen.getByText('2s')).toBeInTheDocument()
  })
})
