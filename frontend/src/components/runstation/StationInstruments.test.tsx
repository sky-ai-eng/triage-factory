import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { TelemetryRail } from './StationInstruments'
import { stationState } from './stationStyle'
import { QUEUE_DWELL_VISIBLE_MS } from '../../lib/conversationStatus'
import type { Conversation, Message } from '../../types'

const T0 = new Date('2026-06-25T00:00:00Z').getTime()
const iso = (offsetMs: number) => new Date(T0 + offsetMs).toISOString()

const conversation = (over: Partial<Conversation>): Conversation =>
  ({
    ID: 'r1',
    TaskID: 't1',
    Status: 'running',
    Model: 'claude-opus-4-8',
    StartedAt: iso(0),
    ResultSummary: '',
    ...over,
  }) as Conversation

function renderRail(over: Partial<Conversation>, now: number = T0 + 60_000) {
  const r = conversation(over)
  render(<TelemetryRail conversation={r} messages={[]} state={stationState(r)} now={now} />)
}

// The rail's settled queued readout keys on the shared visibility threshold
// (QUEUE_DWELL_VISIBLE_MS), so every surface that reads a dwell agrees on
// which short waits are worth a readout.
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

  it('always shows the live wait while the conversation is still queued, even below the threshold', () => {
    renderRail({ Status: 'queued', QueuedAt: iso(0) }, T0 + 2000)
    expect(screen.getByText('queued')).toBeInTheDocument()
    expect(screen.getByText('2s')).toBeInTheDocument()
  })
})

// The token readouts come off the conversation row — the SUM the detail read
// already computes, the same numbers the usage dashboard reports — rather
// than a walk of the held transcript. The transcript here carries different
// counts on purpose: a rail that still summed it would show those instead.
describe('TelemetryRail token readouts', () => {
  const held: Message[] = [
    {
      id: 1,
      conversation_id: 'r1',
      role: 'assistant',
      content: 'working',
      subtype: '',
      created_at: iso(0),
      input_tokens: 7,
      output_tokens: 7,
      cache_read_tokens: 7,
      cache_creation_tokens: 7,
    },
  ]

  function renderTokens(over: Partial<Conversation>) {
    const r = conversation(over)
    render(
      <TelemetryRail conversation={r} messages={held} state={stationState(r)} now={T0 + 60_000} />,
    )
  }

  it('reads the conversation row’s sums', () => {
    renderTokens({
      input_tokens: 12_300,
      output_tokens: 450,
      cache_read_tokens: 98_000,
      cache_creation_tokens: 2100,
    })
    expect(screen.getByText('12.3k')).toBeInTheDocument()
    expect(screen.getByText('450')).toBeInTheDocument()
    expect(screen.getByText('cache·r 98k')).toBeInTheDocument()
    expect(screen.getByText('cache·w 2.1k')).toBeInTheDocument()
  })

  it('shows zeros for a conversation that never streamed usage, and hides the cache line', () => {
    renderTokens({ input_tokens: 0, output_tokens: 0 })
    expect(screen.getAllByText('0')).toHaveLength(2)
    expect(screen.queryByText(/cache·/)).not.toBeInTheDocument()
  })
})

// The idle readout is the live claim's activity stamp against the rail's own
// clock. The stamp sits a minute after StartedAt so the idle value (2m 0s)
// never matches the running readout (3m 0s).
describe('TelemetryRail idle readout', () => {
  const at3m = T0 + 180_000

  it('shows the idle duration and the operation in flight while running', () => {
    renderRail({ claim_last_activity_at: iso(60_000), claim_current_op: 'tool:bash' }, at3m)
    expect(screen.getByText('idle')).toBeInTheDocument()
    expect(screen.getByText('2m 0s · tool:bash')).toBeInTheDocument()
  })

  it('shows the idle duration alone when nothing is in flight', () => {
    renderRail({ claim_last_activity_at: iso(60_000) }, at3m)
    expect(screen.getByText('idle')).toBeInTheDocument()
    expect(screen.getByText('2m 0s')).toBeInTheDocument()
  })

  it('shows while the engagement is still setting up', () => {
    renderRail(
      { Status: 'cloning', claim_last_activity_at: iso(60_000), claim_current_op: 'clone' },
      at3m,
    )
    expect(screen.getByText('idle')).toBeInTheDocument()
    expect(screen.getByText('2m 0s · clone')).toBeInTheDocument()
  })

  it('is hidden once the engagement is not live', () => {
    renderRail(
      { Status: 'open', claim_last_activity_at: iso(60_000), claim_current_op: 'tool:bash' },
      at3m,
    )
    expect(screen.queryByText('idle')).not.toBeInTheDocument()
    expect(screen.queryByText(/tool:bash/)).not.toBeInTheDocument()
  })

  it('is hidden when the claim has not stamped any activity', () => {
    renderRail({ claim_current_op: 'provider' }, at3m)
    expect(screen.queryByText('idle')).not.toBeInTheDocument()
    expect(screen.queryByText(/provider/)).not.toBeInTheDocument()
  })
})
