import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'

import type { FleetInstances, FleetSandboxes, FleetSandboxSeries } from '../types'

// The console is operator + FeatureFleet gated; both are mocked on so the page
// renders instead of bouncing home. The gate itself is covered server-side
// (ee/fleet's denial-parity test) — these tests are about the breakdown.
vi.mock('../hooks/useEntitlements', () => ({
  FeatureFleet: 'fleet',
  useEntitlements: () => ({ has: (f: string) => f === 'fleet', loaded: true }),
}))
vi.mock('../contexts/AuthContext', () => ({
  useOptionalAuth: () => ({ me: { is_operator: true } }),
}))

import Fleet from './Fleet'

// --- fixtures -------------------------------------------------------------

const INSTANCES: FleetInstances = {
  generated_at: '2026-07-30T12:00:00Z',
  stale_after_seconds: 30,
  instances: [
    {
      id: 'executor-alpha-01',
      role: 'executor',
      version: 'v9',
      boot_epoch: 3,
      started_at: '2026-07-30T10:00:00Z',
      last_heartbeat_at: '2026-07-30T12:00:00Z',
      heartbeat_age_seconds: 2,
      stale: false,
      draining: false,
      max_runs: 8,
      active_runs: 1,
    },
    {
      id: 'control-pod-01',
      role: 'control',
      version: 'v9',
      boot_epoch: 1,
      started_at: '2026-07-30T10:00:00Z',
      last_heartbeat_at: '2026-07-30T12:00:00Z',
      heartbeat_age_seconds: 1,
      stale: false,
      draining: false,
    },
  ],
}

const MEASURED_CLAIM = 'claim-measured-0001'
const UNMEASURED_CLAIM = 'claim-unmeasured-02'

const SANDBOXES: FleetSandboxes = {
  generated_at: '2026-07-30T12:00:00Z',
  instance_id: 'executor-alpha-01',
  limit: 25,
  sandboxes: [
    {
      id: MEASURED_CLAIM,
      conversation_id: 'run-aaaaaaaa-bbbb',
      org_id: 'org-11111111-2222',
      claimed_at: '2026-07-30T11:00:00Z',
      released_at: '2026-07-30T11:02:00Z',
      live: false,
      duration_seconds: 120,
      peak_mem_mb: 2048,
      // 60 core-seconds over 120s of wall clock: 1.00 core-minutes, 0.50 cores.
      cpu_usec: 60_000_000,
      status: 'failed',
      failure_kind: 'memory_limit',
      outcome: 'failed',
    },
    {
      id: UNMEASURED_CLAIM,
      conversation_id: 'run-cccccccc-dddd',
      org_id: 'org-33333333-4444',
      claimed_at: '2026-07-30T11:30:00Z',
      live: true,
      duration_seconds: 1800,
      status: 'running',
    },
  ],
}

const EMPTY_SANDBOXES: FleetSandboxes = {
  generated_at: '2026-07-30T12:00:00Z',
  instance_id: 'control-pod-01',
  limit: 25,
  sandboxes: [],
}

const SERIES: FleetSandboxSeries = {
  generated_at: '2026-07-30T12:00:00Z',
  claim_id: MEASURED_CLAIM,
  samples: [
    { at: '2026-07-30T11:00:00Z', mem_current_mb: 120, cpu_usec_cum: 0 },
    { at: '2026-07-30T11:01:00Z', mem_current_mb: 900, cpu_usec_cum: 30_000_000 },
  ],
}

const EMPTY_SERIES: FleetSandboxSeries = {
  generated_at: '2026-07-30T12:00:00Z',
  claim_id: UNMEASURED_CLAIM,
  samples: [],
}

// --- harness --------------------------------------------------------------

// requests records every URL the page fetches, so "no requests fire for
// collapsed rows" is asserted directly rather than inferred from the DOM.
let requests: string[] = []

function stubFetch() {
  return vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input)
    requests.push(url)
    const json = (b: unknown) => new Response(JSON.stringify(b), { status: 200 })
    if (url.includes('/api/fleet/instances?')) return json(INSTANCES)
    if (url.includes('/api/fleet/instances/executor-alpha-01/sandboxes')) return json(SANDBOXES)
    if (url.includes('/api/fleet/instances/control-pod-01/sandboxes')) return json(EMPTY_SANDBOXES)
    if (url.includes(`/api/fleet/claims/${MEASURED_CLAIM}/series`)) return json(SERIES)
    if (url.includes(`/api/fleet/claims/${UNMEASURED_CLAIM}/series`)) return json(EMPTY_SERIES)
    if (url.includes('/api/fleet/overview')) {
      return json({
        generated_at: '2026-07-30T12:00:00Z',
        stale_after_seconds: 30,
        totals: {
          instances: 2,
          executors: 1,
          control: 1,
          draining: 0,
          gated: 0,
          stale: 0,
          capacity_max: 8,
          active_runs: 1,
        },
        queue: { depth: 0, oldest_wait_seconds: 0 },
        runs: { window_hours: 24, total: 0, active: 0, completed: 0, failed: 0, failure_kinds: [] },
        versions: [{ version: 'v9', count: 2 }],
      })
    }
    if (url.includes('/api/fleet/timeseries')) {
      return json({ generated_at: '2026-07-30T12:00:00Z', window_hours: 24, samples: [] })
    }
    if (url.includes('/api/fleet/backlog')) {
      return json({
        generated_at: '2026-07-30T12:00:00Z',
        depth: 0,
        oldest_wait_seconds: 0,
        by_org: [],
      })
    }
    return new Response('{}', { status: 404 })
  })
}

async function renderFleet() {
  render(
    <MemoryRouter>
      <Fleet />
    </MemoryRouter>,
  )
  // The machine cards land once the instances read resolves.
  await screen.findByRole('button', { name: 'sandboxes on executor-alpha-01' })
}

const sandboxUrls = () => requests.filter((u) => u.includes('/sandboxes'))
const seriesUrls = () => requests.filter((u) => u.includes('/series'))

beforeEach(() => {
  requests = []
  vi.stubGlobal('fetch', stubFetch())
})

// --- tests ----------------------------------------------------------------

describe('Fleet sandbox breakdown', () => {
  it('fetches nothing for collapsed rows', async () => {
    await renderFleet()
    // The page has settled its own reads; not one of them is a breakdown.
    await waitFor(() => expect(requests.length).toBeGreaterThan(0))
    expect(sandboxUrls()).toEqual([])
    expect(seriesUrls()).toEqual([])
    expect(screen.queryByText(/most recent/)).not.toBeInTheDocument()
  })

  it('shows an executor’s recent sandboxes when expanded', async () => {
    const user = userEvent.setup()
    await renderFleet()
    await user.click(screen.getByRole('button', { name: 'sandboxes on executor-alpha-01' }))

    await screen.findByText('2 most recent')
    expect(sandboxUrls()).toHaveLength(1)
    expect(sandboxUrls()[0]).toContain('/api/fleet/instances/executor-alpha-01/sandboxes')
    // Still no series request: the second level is its own expand.
    expect(seriesUrls()).toEqual([])

    // Scoped to the breakdown table — the machine cards above carry their own
    // "live" chips and readouts.
    const table = within(screen.getByRole('table'))
    // The measured claim renders its actuals: 2GB peak, 1.00 core-minutes,
    // 0.50 average cores over a 2m engagement.
    expect(table.getByText('2.0GB')).toBeInTheDocument()
    expect(table.getByText('1.00')).toBeInTheDocument()
    expect(table.getByText('0.50')).toBeInTheDocument()
    expect(table.getByText('2.0m')).toBeInTheDocument()
    expect(table.getByText(/failed · memory limit/)).toBeInTheDocument()

    // The live, unmeasured claim is listed all the same, with dashes where
    // nothing was measured — absent means "not measured", never zero.
    expect(table.getByText('live')).toBeInTheDocument()
    expect(table.getAllByText('—')).toHaveLength(3)
  })

  it('collapsing stops the breakdown from refetching', async () => {
    const user = userEvent.setup()
    await renderFleet()
    const toggle = screen.getByRole('button', { name: 'sandboxes on executor-alpha-01' })
    await user.click(toggle)
    await screen.findByText('2 most recent')
    expect(sandboxUrls()).toHaveLength(1)

    await user.click(toggle)
    await waitFor(() => expect(screen.queryByText('2 most recent')).not.toBeInTheDocument())
    // Unmounted, so its polling interval is gone too — the count is frozen.
    expect(sandboxUrls()).toHaveLength(1)
  })

  it('renders a sandbox’s series on the second expand', async () => {
    const user = userEvent.setup()
    await renderFleet()
    await user.click(screen.getByRole('button', { name: 'sandboxes on executor-alpha-01' }))
    await screen.findByText('2 most recent')

    await user.click(
      screen.getByRole('button', {
        name: `usage series for sandbox ${MEASURED_CLAIM.slice(0, 8)}…${MEASURED_CLAIM.slice(-3)}`,
      }),
    )
    await screen.findByText(/chart is the sampled shape/)
    expect(seriesUrls()).toHaveLength(1)
    expect(seriesUrls()[0]).toContain(`/api/fleet/claims/${MEASURED_CLAIM}/series`)
    expect(screen.getByText('Memory · sampled')).toBeInTheDocument()
    expect(screen.getByText('CPU · derived from cumulative')).toBeInTheDocument()
  })

  it('renders a claim with no samples chartless, not as an error', async () => {
    const user = userEvent.setup()
    await renderFleet()
    await user.click(screen.getByRole('button', { name: 'sandboxes on executor-alpha-01' }))
    await screen.findByText('2 most recent')

    await user.click(
      screen.getByRole('button', {
        name: `usage series for sandbox ${UNMEASURED_CLAIM.slice(0, 8)}…${UNMEASURED_CLAIM.slice(-3)}`,
      }),
    )
    await screen.findByText('no samples for this sandbox')
    // The row itself survives — a chartless sandbox is not a failed one.
    expect(screen.getByText('2 most recent')).toBeInTheDocument()
    expect(screen.queryByText(/chart is the sampled shape/)).not.toBeInTheDocument()
  })

  it('shows an empty state for an instance with no sandboxes', async () => {
    const user = userEvent.setup()
    await renderFleet()
    await user.click(screen.getByRole('button', { name: 'sandboxes on control-pod-01' }))
    await screen.findByText('no sandboxes on this instance')
    expect(sandboxUrls()[0]).toContain('/api/fleet/instances/control-pod-01/sandboxes')
  })
})
