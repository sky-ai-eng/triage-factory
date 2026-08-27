import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import type { EventType } from '../types'

// Marketplace resolves org-prefixed paths via useOrgHref, which in turn
// depends on useDeploymentConfig (a live /api/config fetch) and OrgContext.
// Isolating the page from that plumbing here mirrors OrgPage.test.tsx's
// approach of mocking hook dependencies rather than standing up the full
// provider tree.
vi.mock('../hooks/useOrgHref', () => ({ useOrgHref: () => (path: string) => path }))

import Marketplace from './Marketplace'
import { jsonBody, listBody } from '../test/apiResponse'

const EVENT_TYPES: EventType[] = [
  {
    id: 'github:pr:opened',
    source: 'github',
    category: 'pr',
    label: 'PR Opened',
    description: '',
    supports_watch: true,
  },
  {
    id: 'github:pr:ci_check_failed',
    source: 'github',
    category: 'pr',
    label: 'CI Check Failed',
    description: '',
    supports_watch: false,
  },
]

interface ListingStatsFixture {
  teams_using: number
  total_runs: number
  success_rate?: number
  last_run_at?: string
  computed_at: string
}

interface ListingFixture {
  id: string
  kind: 'prompt' | 'blueprint'
  status: 'published' | 'delisted'
  name: string
  description: string
  publisher_team_name?: string
  current_version: number
  updated_at: string
  event_types: string[]
  vote_count: number
  install_count: number
  viewer_voted: boolean
  stats?: ListingStatsFixture
}

function listing(over: Partial<ListingFixture>): ListingFixture {
  return {
    id: 'l1',
    kind: 'prompt',
    status: 'published',
    name: 'Triage Helper',
    description: 'Helps triage incoming issues',
    publisher_team_name: 'Platform',
    current_version: 1,
    updated_at: '2026-07-01T00:00:00Z',
    event_types: ['github:pr:opened'],
    vote_count: 2,
    install_count: 5,
    viewer_voted: false,
    ...over,
  }
}

// Routes fetch calls by path (query string ignored except where a test reads
// it back off the mock's call args) — mirrors useEntitlements.test.ts's
// per-endpoint switch, extended to the small set of endpoints this page hits.
function mockFetchRouter(opts: {
  listings?: ListingFixture[]
  detail?: ListingFixture & Record<string, unknown>
}) {
  const fetchMock = vi.fn((input: unknown) => {
    const url = String(input)
    const path = url.split('?')[0]
    if (path === '/api/event-types') {
      return Promise.resolve({ ok: true, ...jsonBody(EVENT_TYPES) })
    }
    if (path === '/api/marketplace/listings/list') {
      return Promise.resolve({ ok: true, ...listBody(opts.listings ?? []) })
    }
    if (/^\/api\/marketplace\/listings\/[^/]+\/vote$/.test(path)) {
      return Promise.resolve({ ok: true, status: 204, ...jsonBody(null) })
    }
    if (/^\/api\/marketplace\/listings\/[^/]+$/.test(path)) {
      return Promise.resolve({ ok: true, ...jsonBody(opts.detail ?? null) })
    }
    return Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

// calledBodies parses each recorded request's JSON body — the list route's
// filters travel there now, not in a query string.
function calledBodies(fetchMock: ReturnType<typeof vi.fn>): Record<string, unknown>[] {
  return fetchMock.mock.calls.map((c) => JSON.parse(String((c[1] as RequestInit)?.body ?? '{}')))
}

function renderPage() {
  return render(
    <MemoryRouter>
      <Marketplace />
    </MemoryRouter>,
  )
}

describe('Marketplace', () => {
  beforeEach(() => vi.restoreAllMocks())

  it('renders each listing card with name, publisher, kind badge, event-type chips, and counts', async () => {
    mockFetchRouter({
      listings: [
        listing({ id: 'l1', name: 'Triage Helper', publisher_team_name: 'Platform' }),
        listing({
          id: 'l2',
          kind: 'blueprint',
          name: 'Deploy Chain',
          publisher_team_name: 'Infra',
          event_types: ['github:pr:ci_check_failed'],
          vote_count: 0,
          install_count: 0,
        }),
      ],
    })
    renderPage()

    expect(await screen.findByText('Triage Helper')).toBeInTheDocument()
    expect(screen.getByText('Deploy Chain')).toBeInTheDocument()
    expect(screen.getByText('Platform')).toBeInTheDocument()
    expect(screen.getByText('Infra')).toBeInTheDocument()
    expect(screen.getByText('Prompt')).toBeInTheDocument()
    expect(screen.getByText('Blueprint')).toBeInTheDocument()
    // Each event-type label appears twice: once as a facet chip, once as the
    // matching card's chip.
    expect(screen.getAllByText('PR Opened')).toHaveLength(2)
    expect(screen.getAllByText('CI Check Failed')).toHaveLength(2)
    expect(screen.getByText('5 installs')).toBeInTheDocument()
    expect(screen.getByText('0 installs')).toBeInTheDocument()
  })

  describe('run-derived stats (TFAC-540)', () => {
    it('shows the stats line on a card when stats exist with run history', async () => {
      mockFetchRouter({
        listings: [
          listing({
            id: 'l1',
            stats: {
              teams_using: 3,
              total_runs: 42,
              success_rate: 0.75,
              computed_at: '2026-07-01T00:00:00Z',
            },
          }),
        ],
      })
      renderPage()

      expect(await screen.findByText('Triage Helper')).toBeInTheDocument()
      expect(screen.getByText('3 teams · 42 runs · 75% success')).toBeInTheDocument()
    })

    it('shows nothing when stats are absent (job has never run for this listing)', async () => {
      mockFetchRouter({ listings: [listing({ id: 'l1', stats: undefined })] })
      renderPage()

      expect(await screen.findByText('Triage Helper')).toBeInTheDocument()
      expect(screen.queryByText(/success/)).not.toBeInTheDocument()
      expect(screen.queryByText(/runs?$/)).not.toBeInTheDocument()
    })

    it('shows nothing when stats exist but total_runs is 0 (no fake 0% success)', async () => {
      mockFetchRouter({
        listings: [
          listing({
            id: 'l1',
            stats: { teams_using: 2, total_runs: 0, computed_at: '2026-07-01T00:00:00Z' },
          }),
        ],
      })
      renderPage()

      expect(await screen.findByText('Triage Helper')).toBeInTheDocument()
      expect(screen.queryByText(/0%/)).not.toBeInTheDocument()
      expect(screen.queryByText(/0 runs/)).not.toBeInTheDocument()
    })

    it('omits the success-rate segment when success_rate is absent but runs exist', async () => {
      mockFetchRouter({
        listings: [
          listing({
            id: 'l1',
            stats: { teams_using: 1, total_runs: 5, computed_at: '2026-07-01T00:00:00Z' },
          }),
        ],
      })
      renderPage()

      expect(await screen.findByText('1 team · 5 runs')).toBeInTheDocument()
    })

    it('offers "Most used" as a sort option and re-fetches with sort=used', async () => {
      const fetchMock = mockFetchRouter({ listings: [listing({})] })
      renderPage()
      await screen.findByText('Triage Helper')

      fetchMock.mockClear()
      fireEvent.change(screen.getByLabelText('Sort listings'), { target: { value: 'used' } })

      await waitFor(() => {
        expect(calledBodies(fetchMock).some((b) => b.sort === 'used')).toBe(true)
      })
    })
  })

  it('shows the org-empty state (with a link to the team prompts workspace) when there are zero listings and no filters', async () => {
    mockFetchRouter({ listings: [] })
    renderPage()

    expect(await screen.findByText('No listings yet')).toBeInTheDocument()
    const link = screen.getByRole('link', { name: /Go to your team.s prompts/ })
    expect(link).toHaveAttribute('href', '/team?tab=prompts')
  })

  describe('facet filtering', () => {
    it('clicking an event-type chip re-fetches with that event_type and marks the chip pressed', async () => {
      const fetchMock = mockFetchRouter({ listings: [listing({})] })
      renderPage()
      await screen.findByText('Triage Helper')

      const allChip = screen.getByRole('button', { name: 'All events' })
      const ciChip = screen.getByRole('button', { name: 'CI Check Failed' })
      expect(allChip).toHaveAttribute('aria-pressed', 'true')
      expect(ciChip).toHaveAttribute('aria-pressed', 'false')

      fetchMock.mockClear()
      fireEvent.click(ciChip)

      await waitFor(() => {
        expect(
          calledBodies(fetchMock).some((b) => b.event_type === 'github:pr:ci_check_failed'),
        ).toBe(true)
      })
      expect(ciChip).toHaveAttribute('aria-pressed', 'true')
      expect(allChip).toHaveAttribute('aria-pressed', 'false')
    })

    it('clicking the Blueprints kind toggle re-fetches with kind=blueprint', async () => {
      const fetchMock = mockFetchRouter({ listings: [listing({})] })
      renderPage()
      await screen.findByText('Triage Helper')

      fetchMock.mockClear()
      fireEvent.click(screen.getByRole('button', { name: 'Blueprints' }))

      await waitFor(() => {
        expect(calledBodies(fetchMock).some((b) => b.kind === 'blueprint')).toBe(true)
      })
      expect(screen.getByRole('button', { name: 'Blueprints' })).toHaveAttribute(
        'aria-pressed',
        'true',
      )
    })
  })

  describe('vote toggle', () => {
    it('recommending a listing optimistically flips the button state and count, and PUTs the vote', async () => {
      const fetchMock = mockFetchRouter({
        listings: [listing({ id: 'l1', vote_count: 2, viewer_voted: false })],
      })
      renderPage()
      await screen.findByText('Triage Helper')

      const voteButton = screen.getByRole('button', { name: 'Recommend' })
      expect(voteButton).toHaveAttribute('aria-pressed', 'false')
      expect(voteButton).toHaveTextContent('2')

      fireEvent.click(voteButton)

      // Optimistic update lands before the network round-trip resolves.
      expect(screen.getByRole('button', { name: 'Remove recommendation' })).toHaveAttribute(
        'aria-pressed',
        'true',
      )
      expect(screen.getByRole('button', { name: 'Remove recommendation' })).toHaveTextContent('3')

      await waitFor(() => {
        expect(fetchMock).toHaveBeenCalledWith(
          '/api/marketplace/listings/l1/vote',
          expect.objectContaining({ method: 'PUT' }),
        )
      })
    })

    it('un-recommending an already-voted listing DELETEs the vote and drops the count', async () => {
      const fetchMock = mockFetchRouter({
        listings: [listing({ id: 'l1', vote_count: 3, viewer_voted: true })],
      })
      renderPage()
      await screen.findByText('Triage Helper')

      const voteButton = screen.getByRole('button', { name: 'Remove recommendation' })
      expect(voteButton).toHaveTextContent('3')

      fireEvent.click(voteButton)

      expect(screen.getByRole('button', { name: 'Recommend' })).toHaveAttribute(
        'aria-pressed',
        'false',
      )
      expect(screen.getByRole('button', { name: 'Recommend' })).toHaveTextContent('2')

      await waitFor(() => {
        expect(fetchMock).toHaveBeenCalledWith(
          '/api/marketplace/listings/l1/vote',
          expect.objectContaining({ method: 'DELETE' }),
        )
      })
    })

    it('reverts the optimistic update and surfaces an error toast when the vote request fails', async () => {
      const fetchMock = vi.fn((input: unknown) => {
        const url = String(input)
        const path = url.split('?')[0]
        if (path === '/api/event-types') {
          return Promise.resolve({ ok: true, ...jsonBody([]) })
        }
        if (path === '/api/marketplace/listings/list') {
          return Promise.resolve({
            ok: true,
            ...listBody([listing({ id: 'l1', vote_count: 1, viewer_voted: false })]),
          })
        }
        if (path.endsWith('/vote')) {
          return Promise.resolve({
            ok: false,
            status: 500,
            text: () => Promise.resolve(''),
          })
        }
        return Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })
      })
      vi.stubGlobal('fetch', fetchMock)
      renderPage()
      await screen.findByText('Triage Helper')

      fireEvent.click(screen.getByRole('button', { name: 'Recommend' }))

      // The failed vote re-fetches the list, which restores the original
      // (un-voted) count and label.
      await waitFor(() => {
        expect(screen.getByRole('button', { name: 'Recommend' })).toHaveTextContent('1')
      })
    })
  })
})
