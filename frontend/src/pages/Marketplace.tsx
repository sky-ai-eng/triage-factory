import { useState, useEffect, useCallback, useMemo, useRef } from 'react'
import { Link } from 'react-router'
import { AnimatePresence, motion } from 'motion/react'
import { Store, ThumbsUp, X } from 'lucide-react'
import { useOrgHref } from '../hooks/useOrgHref'
import { useFocusTrap } from '../hooks/useFocusTrap'
import { useTeams, useWriteTeam, noteWrittenTeam } from '../hooks/useTeams'
import { apiFetch, apiJSON, httpErrorMessage } from '../lib/apiClient'
import { toast } from '../components/Toast/toastStore'
import SearchField from '../components/SearchField'
import TeamPicker from '../components/TeamPicker'
import type { EventType } from '../types'

// Within-org prompt marketplace browse page (TFAC-537). Flat searchable list
// + event-type facets — no folder tree, no free-form tags (TFAC-92's V1
// scoping decision). Multi-mode only: this page is reachable only via the
// MultiRoutes route table (nothing in LocalRoutes), and the backend's
// gateMarketplace 404s in local mode only — there is no org-level admin
// toggle for the within-org marketplace (it's always on for every
// multi-mode org). org_settings.marketplace_enabled exists but is reserved
// for the future cross-org marketplace gate (TFAC-92 phase 2 / TFAC-539);
// it has nothing to do with this page's access.

// Wire shapes mirror domain.ListingSummary / domain.ListingDetail
// (internal/domain/marketplace.go) — only the fields this page reads.
interface ListingStats {
  teams_using: number
  total_runs: number
  success_rate?: number
  last_run_at?: string
  computed_at: string
}

interface ListingSummary {
  id: string
  kind: 'prompt' | 'blueprint'
  status: 'published' | 'delisted'
  name: string
  description: string
  publisher_team_id?: string
  publisher_team_name?: string
  current_version: number
  updated_at: string
  event_types: string[]
  vote_count: number
  install_count: number
  viewer_voted: boolean
  // Run-derived usage stats (TFAC-540), absent until the system aggregation
  // job has computed at least one cycle for this listing. Per the
  // no-wrong-fallbacks rule, absence must render nothing — never a
  // synthesized "0 teams · 0 runs" line.
  stats?: ListingStats
}

interface SnapshotStep {
  step_index: number
  name: string
  body: string
  allowed_tools: string
  model: string
  brief: string
}

interface ListingSnapshotWire {
  steps: SnapshotStep[]
}

interface ListingVersionWire {
  version: number
  created_at: string
}

interface ListingDetail extends ListingSummary {
  current_snapshot: ListingSnapshotWire
  versions: ListingVersionWire[]
}

type SortOrder = 'installs' | 'votes' | 'recent' | 'used'
type KindFilter = '' | 'prompt' | 'blueprint'

const kindLabel = (kind: string) => (kind === 'blueprint' ? 'Blueprint' : 'Prompt')
const kindBadgeClass = (kind: string) =>
  kind === 'blueprint' ? 'bg-accent/10 text-accent' : 'bg-black/[0.04] text-text-tertiary'

// formatStatsLine renders the TFAC-540 "N teams · M runs · X% success" line,
// or null when there's nothing worth showing — the no-wrong-fallbacks rule:
// a listing with no run history yet (stats absent, or a computed row with
// total_runs === 0) shows nothing rather than a synthesized "0 runs · 0%".
function formatStatsLine(stats: ListingStats | undefined): string | null {
  if (!stats || stats.total_runs === 0) return null
  const parts = [
    `${stats.teams_using} team${stats.teams_using === 1 ? '' : 's'}`,
    `${stats.total_runs} run${stats.total_runs === 1 ? '' : 's'}`,
  ]
  if (stats.success_rate != null) {
    parts.push(`${Math.round(stats.success_rate * 100)}% success`)
  }
  return parts.join(' · ')
}

export default function Marketplace() {
  const orgHref = useOrgHref()
  const [query, setQuery] = useState('')
  const [debouncedQuery, setDebouncedQuery] = useState('')
  const [eventType, setEventType] = useState('')
  const [kind, setKind] = useState<KindFilter>('')
  const [sort, setSort] = useState<SortOrder>('installs')
  const [catalog, setCatalog] = useState<EventType[]>([])
  const [listings, setListings] = useState<ListingSummary[]>([])
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [detail, setDetail] = useState<ListingDetail | null>(null)
  const [detailLoading, setDetailLoading] = useState(false)

  // Debounce the free-text query so every keystroke doesn't round-trip.
  useEffect(() => {
    const t = setTimeout(() => setDebouncedQuery(query.trim()), 250)
    return () => clearTimeout(t)
  }, [query])

  // Event-type catalog — same source BindingGraph/MarketplacePublishControl
  // use for their event-type pickers, reused here for the facet chips.
  useEffect(() => {
    apiJSON<EventType[]>('/api/event-types')
      .then((data) => {
        if (Array.isArray(data)) setCatalog(data)
      })
      .catch(() => {})
  }, [])

  const catalogById = useMemo(() => {
    const m = new Map<string, EventType>()
    catalog.forEach((et) => m.set(et.id, et))
    return m
  }, [catalog])

  const refresh = useCallback(async () => {
    setLoading(true)
    setLoadError(null)
    try {
      const qs = [
        debouncedQuery && `query=${encodeURIComponent(debouncedQuery)}`,
        eventType && `event_type=${encodeURIComponent(eventType)}`,
        kind && `kind=${kind}`,
        `sort=${sort}`,
      ]
        .filter(Boolean)
        .join('&')
      setListings(await apiJSON<ListingSummary[]>(`/api/marketplace/listings?${qs}`))
    } catch (err) {
      setLoadError(httpErrorMessage(err, 'Could not load the marketplace listings.'))
    } finally {
      setLoading(false)
    }
  }, [debouncedQuery, eventType, kind, sort])

  useEffect(() => {
    refresh()
  }, [refresh])

  const openDetail = useCallback(async (id: string) => {
    setSelectedId(id)
    setDetail(null)
    setDetailLoading(true)
    try {
      setDetail(await apiJSON<ListingDetail>(`/api/marketplace/listings/${encodeURIComponent(id)}`))
    } catch (err) {
      toast.error(httpErrorMessage(err, 'Could not load the listing.'))
      setSelectedId(null)
    } finally {
      setDetailLoading(false)
    }
  }, [])

  const closeDetail = useCallback(() => {
    setSelectedId(null)
    setDetail(null)
  }, [])

  // Optimistic vote toggle: flip locally in both the grid row and the open
  // drawer (if it's the same listing), then reconcile with a refresh on
  // failure. Vote/Unvote are idempotent server-side, so a double-click race
  // is harmless either way.
  const toggleVote = useCallback(
    async (listing: ListingSummary) => {
      const nextVoted = !listing.viewer_voted
      const delta = nextVoted ? 1 : -1
      setListings((prev) =>
        prev.map((l) =>
          l.id === listing.id
            ? { ...l, viewer_voted: nextVoted, vote_count: l.vote_count + delta }
            : l,
        ),
      )
      setDetail((prev) =>
        prev && prev.id === listing.id
          ? { ...prev, viewer_voted: nextVoted, vote_count: prev.vote_count + delta }
          : prev,
      )
      try {
        await apiFetch(`/api/marketplace/listings/${encodeURIComponent(listing.id)}/vote`, {
          method: nextVoted ? 'PUT' : 'DELETE',
        })
      } catch (err) {
        toast.error(httpErrorMessage(err, 'Could not update your vote.'))
        refresh()
      }
    },
    [refresh],
  )

  // Bumps the install count locally (grid row + open drawer) after a
  // successful "copy to my team" — mirrors toggleVote's optimistic-update
  // shape, but there's nothing to reconcile on failure since the count only
  // moves after the POST already succeeded.
  const handleInstalled = useCallback((listingId: string) => {
    setListings((prev) =>
      prev.map((l) => (l.id === listingId ? { ...l, install_count: l.install_count + 1 } : l)),
    )
    setDetail((prev) =>
      prev && prev.id === listingId ? { ...prev, install_count: prev.install_count + 1 } : prev,
    )
  }, [])

  const filtersActive = debouncedQuery !== '' || eventType !== '' || kind !== ''
  const clearFilters = () => {
    setQuery('')
    setDebouncedQuery('')
    setEventType('')
    setKind('')
  }

  const content = loading ? (
    <div className="text-text-tertiary text-[13px]">Loading marketplace…</div>
  ) : loadError ? (
    <ErrorState message={loadError} onRetry={refresh} />
  ) : listings.length === 0 && !filtersActive ? (
    <EmptyState promptsHref={orgHref('/team') + '?tab=prompts'} />
  ) : listings.length === 0 ? (
    <div className="flex flex-col items-center justify-center py-24 text-center">
      <p className="text-[13px] text-text-secondary mb-4">No listings match your filters.</p>
      <button
        type="button"
        onClick={clearFilters}
        className="text-[13px] font-medium text-accent hover:opacity-80 transition-opacity"
      >
        Clear filters
      </button>
    </div>
  ) : (
    <div className="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-3 gap-5">
      {listings.map((l) => (
        <ListingCard
          key={l.id}
          listing={l}
          catalogById={catalogById}
          onOpen={() => openDetail(l.id)}
          onToggleVote={() => toggleVote(l)}
        />
      ))}
    </div>
  )

  return (
    <div className="max-w-6xl mx-auto">
      <header className="mb-6">
        <h1 className="text-2xl font-semibold tracking-tight text-text-primary">Marketplace</h1>
        <p className="text-[13px] text-text-secondary mt-1">
          Browse prompts and blueprints published by other teams in your org — sorted by installs,
          recommendations, or recency.
        </p>
      </header>

      <div className="mb-6 space-y-4">
        <div className="flex flex-col sm:flex-row sm:items-center gap-3">
          <div className="flex-1 max-w-sm">
            <SearchField
              value={query}
              onChange={setQuery}
              placeholder="Search listings…"
              ariaLabel="Search marketplace listings"
            />
          </div>
          <div className="flex items-center gap-2">
            <KindToggle value={kind} onChange={setKind} />
            <select
              value={sort}
              onChange={(e) => setSort(e.target.value as SortOrder)}
              aria-label="Sort listings"
              className="text-[13px] text-text-secondary bg-white/50 border border-border-subtle rounded-full px-3 py-1.5 focus:outline-none focus:ring-2 focus:ring-accent/30 focus:border-accent/40 transition-colors"
            >
              <option value="installs">Most installed</option>
              <option value="votes">Most recommended</option>
              <option value="used">Most used</option>
              <option value="recent">Recently updated</option>
            </select>
          </div>
        </div>

        {catalog.length > 0 && (
          <div className="flex items-center gap-2 overflow-x-auto pb-1">
            <FacetChip active={eventType === ''} onClick={() => setEventType('')}>
              All events
            </FacetChip>
            {catalog.map((et) => (
              <FacetChip
                key={et.id}
                active={eventType === et.id}
                onClick={() => setEventType(et.id)}
              >
                {et.label}
              </FacetChip>
            ))}
          </div>
        )}
      </div>

      {content}

      <ListingDrawer
        open={selectedId !== null}
        detail={detail}
        loading={detailLoading}
        catalogById={catalogById}
        onClose={closeDetail}
        onToggleVote={toggleVote}
        onInstalled={handleInstalled}
      />
    </div>
  )
}

function KindToggle({ value, onChange }: { value: KindFilter; onChange: (v: KindFilter) => void }) {
  const options: { value: KindFilter; label: string }[] = [
    { value: '', label: 'All' },
    { value: 'prompt', label: 'Prompts' },
    { value: 'blueprint', label: 'Blueprints' },
  ]
  return (
    <div className="inline-flex items-center gap-1 bg-black/[0.03] rounded-full p-1">
      {options.map((opt) => (
        <button
          key={opt.value || 'all'}
          type="button"
          onClick={() => onChange(opt.value)}
          aria-pressed={value === opt.value}
          className={`text-[12px] font-medium px-3 py-1 rounded-full transition-colors ${
            value === opt.value
              ? 'bg-white text-text-primary shadow-sm'
              : 'text-text-tertiary hover:text-text-secondary'
          }`}
        >
          {opt.label}
        </button>
      ))}
    </div>
  )
}

function FacetChip({
  active,
  onClick,
  children,
}: {
  active: boolean
  onClick: () => void
  children: React.ReactNode
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={active}
      className={`text-[12px] font-medium px-3 py-1 rounded-full border transition-colors whitespace-nowrap shrink-0 ${
        active
          ? 'bg-accent-soft text-accent border-accent/30'
          : 'bg-white/50 text-text-secondary border-border-subtle hover:bg-black/[0.03]'
      }`}
    >
      {children}
    </button>
  )
}

function ListingCard({
  listing,
  catalogById,
  onOpen,
  onToggleVote,
}: {
  listing: ListingSummary
  catalogById: Map<string, EventType>
  onOpen: () => void
  onToggleVote: () => void
}) {
  const shownEventTypes = listing.event_types.slice(0, 3)
  const extraEventTypes = listing.event_types.length - shownEventTypes.length
  const statsLine = formatStatsLine(listing.stats)

  return (
    <article
      className="
        group relative overflow-hidden rounded-2xl border border-border-glass
        bg-gradient-to-br from-white/70 via-white/50 to-white/35
        p-5 shadow-sm shadow-black/[0.03] backdrop-blur-xl
        transition-[box-shadow,border-color] duration-300
        hover:border-white/90 hover:shadow-md hover:shadow-black/[0.05]
      "
    >
      <button
        type="button"
        onClick={onOpen}
        aria-label={`View ${listing.name}`}
        className="absolute inset-0 z-10 rounded-2xl focus:outline-none focus-visible:ring-2 focus-visible:ring-accent"
      />
      <div className="relative flex flex-col h-full">
        <div className="flex items-center justify-between gap-2">
          <span
            className={`inline-flex items-center text-[9px] font-semibold uppercase tracking-wider px-1.5 py-0.5 rounded ${kindBadgeClass(listing.kind)}`}
          >
            {kindLabel(listing.kind)}
          </span>
          <button
            type="button"
            onClick={(e) => {
              e.stopPropagation()
              onToggleVote()
            }}
            aria-pressed={listing.viewer_voted}
            aria-label={listing.viewer_voted ? 'Remove recommendation' : 'Recommend'}
            className={`relative z-20 inline-flex items-center gap-1 text-[11px] font-medium px-2 py-1 rounded-full border transition-colors tabular-nums ${
              listing.viewer_voted
                ? 'bg-accent-soft text-accent border-accent/30'
                : 'text-text-tertiary border-border-subtle hover:bg-black/[0.03]'
            }`}
          >
            <ThumbsUp size={11} />
            {listing.vote_count}
          </button>
        </div>

        <h3 className="mt-3 text-[14px] font-semibold tracking-tight text-text-primary truncate pr-1">
          {listing.name}
        </h3>
        <p className="text-[11px] text-text-tertiary mt-0.5 truncate">
          {listing.publisher_team_name || 'Unknown team'}
        </p>
        {listing.description && (
          <p className="mt-2 text-[12px] leading-relaxed text-text-secondary line-clamp-2">
            {listing.description}
          </p>
        )}

        <div className="mt-3 flex flex-wrap gap-1">
          {shownEventTypes.map((et) => (
            <span
              key={et}
              className="text-[10px] text-text-tertiary bg-black/[0.03] rounded px-1.5 py-0.5"
            >
              {catalogById.get(et)?.label ?? et}
            </span>
          ))}
          {extraEventTypes > 0 && (
            <span className="text-[10px] text-text-tertiary px-1 py-0.5">+{extraEventTypes}</span>
          )}
        </div>

        <div className="mt-auto pt-3 text-[11px] text-text-tertiary tabular-nums space-y-0.5">
          <div>
            {listing.install_count} install{listing.install_count === 1 ? '' : 's'}
          </div>
          {statsLine && <div>{statsLine}</div>}
        </div>
      </div>
    </article>
  )
}

function ListingDrawer({
  open,
  detail,
  loading,
  catalogById,
  onClose,
  onToggleVote,
  onInstalled,
}: {
  open: boolean
  detail: ListingDetail | null
  loading: boolean
  catalogById: Map<string, EventType>
  onClose: () => void
  onToggleVote: (listing: ListingSummary) => void
  onInstalled: (listingId: string) => void
}) {
  // Trap keyboard focus inside the drawer while open and restore it to the
  // trigger on close (WCAG 2.1.2); initial focus lands on the close button.
  const dialogRef = useRef<HTMLDivElement>(null)
  const closeRef = useRef<HTMLButtonElement>(null)
  useFocusTrap(dialogRef, { active: open, initialFocus: closeRef })

  return (
    <AnimatePresence>
      {open && (
        <>
          <motion.div
            className="fixed inset-0 bg-black/20 backdrop-blur-sm z-[70]"
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            onClick={onClose}
          />
          <motion.div
            ref={dialogRef}
            tabIndex={-1}
            role="dialog"
            aria-modal="true"
            aria-label={detail ? detail.name : 'Listing detail'}
            className="fixed inset-y-0 right-0 z-[70] w-full max-w-xl bg-surface-raised/95 backdrop-blur-2xl border-l border-border-glass shadow-2xl shadow-black/10 flex flex-col"
            initial={{ x: '100%' }}
            animate={{ x: 0 }}
            exit={{ x: '100%' }}
            transition={{ duration: 0.2 }}
          >
            <div className="px-6 pt-5 pb-3 flex items-center justify-between shrink-0 border-b border-border-subtle">
              <h2 className="text-[15px] font-semibold text-text-primary flex items-center gap-2 truncate pr-2">
                <Store size={15} className="text-accent shrink-0" />
                <span className="truncate">{detail?.name ?? 'Loading…'}</span>
              </h2>
              <button
                ref={closeRef}
                type="button"
                onClick={onClose}
                aria-label="Close"
                className="text-text-tertiary hover:text-text-secondary transition-colors shrink-0"
              >
                <X size={18} />
              </button>
            </div>

            <div className="flex-1 overflow-y-auto px-6 py-4 space-y-5 min-h-0">
              {loading && <div className="text-[13px] text-text-tertiary">Loading…</div>}
              {detail && (
                <>
                  <div className="flex items-center gap-2 flex-wrap">
                    <span
                      className={`inline-flex items-center text-[9px] font-semibold uppercase tracking-wider px-1.5 py-0.5 rounded ${kindBadgeClass(detail.kind)}`}
                    >
                      {kindLabel(detail.kind)}
                    </span>
                    <span className="text-[12px] text-text-secondary">
                      {detail.publisher_team_name || 'Unknown team'}
                    </span>
                    <span className="text-[11px] text-text-tertiary">
                      v{detail.current_version}
                    </span>
                  </div>

                  {detail.description && (
                    <p className="text-[13px] text-text-secondary leading-relaxed">
                      {detail.description}
                    </p>
                  )}

                  {detail.event_types.length > 0 && (
                    <div className="flex flex-wrap gap-1.5">
                      {detail.event_types.map((et) => (
                        <span
                          key={et}
                          className="text-[10px] text-text-tertiary bg-black/[0.03] rounded px-1.5 py-0.5"
                        >
                          {catalogById.get(et)?.label ?? et}
                        </span>
                      ))}
                    </div>
                  )}

                  <div className="flex items-center gap-3">
                    <button
                      type="button"
                      onClick={() => onToggleVote(detail)}
                      aria-pressed={detail.viewer_voted}
                      className={`inline-flex items-center gap-1.5 text-[12px] font-medium px-3 py-1.5 rounded-full border transition-colors tabular-nums ${
                        detail.viewer_voted
                          ? 'bg-accent-soft text-accent border-accent/30'
                          : 'text-text-secondary border-border-subtle hover:bg-black/[0.03]'
                      }`}
                    >
                      <ThumbsUp size={12} />
                      {detail.viewer_voted ? 'Recommended' : 'Recommend'} · {detail.vote_count}
                    </button>
                    <span className="text-[11px] text-text-tertiary tabular-nums">
                      {detail.install_count} install{detail.install_count === 1 ? '' : 's'}
                    </span>
                  </div>

                  <ListingStatsBlock stats={detail.stats} />

                  <InstallControl listing={detail} onInstalled={onInstalled} />

                  <div>
                    <h3 className="text-[11px] font-semibold text-text-secondary mb-2 uppercase tracking-wide">
                      Steps
                    </h3>
                    <div className="space-y-3">
                      {detail.current_snapshot.steps.map((step) => (
                        <div
                          key={step.step_index}
                          className="border border-border-subtle rounded-lg p-3 bg-black/[0.02]"
                        >
                          <div className="flex items-center justify-between gap-2 mb-1">
                            <span className="text-[12px] font-medium text-text-primary truncate">
                              {step.name}
                            </span>
                            <span className="text-[10px] text-text-tertiary shrink-0">
                              {step.model || 'default model'}
                            </span>
                          </div>
                          {step.brief && (
                            <p className="text-[11px] text-text-tertiary italic mb-2">
                              {step.brief}
                            </p>
                          )}
                          <pre className="text-[11px] font-mono text-text-secondary whitespace-pre-wrap break-words bg-white/60 rounded p-2 max-h-48 overflow-y-auto">
                            {step.body}
                          </pre>
                          {step.allowed_tools && (
                            <p className="text-[10px] text-text-tertiary mt-1">
                              Tools: {step.allowed_tools}
                            </p>
                          )}
                        </div>
                      ))}
                    </div>
                  </div>

                  {detail.versions.length > 0 && (
                    <div>
                      <h3 className="text-[11px] font-semibold text-text-secondary mb-2 uppercase tracking-wide">
                        Version history
                      </h3>
                      <ul className="space-y-1">
                        {detail.versions
                          .slice()
                          .reverse()
                          .map((v) => (
                            <li
                              key={v.version}
                              className="text-[11px] text-text-tertiary flex items-center justify-between"
                            >
                              <span>v{v.version}</span>
                              <span>{new Date(v.created_at).toLocaleDateString()}</span>
                            </li>
                          ))}
                      </ul>
                    </div>
                  )}
                </>
              )}
            </div>
          </motion.div>
        </>
      )}
    </AnimatePresence>
  )
}

// InstallControl is the "copy to my team" action (TFAC-538): a single-team
// caller installs directly on click; a multi-team caller expands an inline
// <TeamPicker> + confirm step first (useWriteTeam seeds it from their
// last-written team, TeamPicker itself renders nothing below 2 teams). On
// success it replaces the control with a link to the target team's prompts
// workspace plus the advisory next step — attaching a trigger stays an
// explicit action in the trigger editor, never auto-created here.
function InstallControl({
  listing,
  onInstalled,
}: {
  listing: ListingDetail
  onInstalled: (listingId: string) => void
}) {
  const orgHref = useOrgHref()
  const { teams } = useTeams()
  const { team, setTeam, multi, ready } = useWriteTeam()
  const [expanded, setExpanded] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [installedTeamId, setInstalledTeamId] = useState<string | null>(null)

  const install = useCallback(
    async (teamId: string) => {
      setSubmitting(true)
      try {
        await apiFetch(`/api/marketplace/listings/${encodeURIComponent(listing.id)}/install`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ team_id: teamId }),
        })
        if (teamId) noteWrittenTeam(teamId)
        setInstalledTeamId(teamId)
        onInstalled(listing.id)
      } catch (err) {
        toast.error(httpErrorMessage(err, 'Could not copy this to your team.'))
      } finally {
        setSubmitting(false)
      }
    },
    [listing.id, onInstalled],
  )

  if (installedTeamId !== null) {
    const teamName = teams.find((t) => t.id === installedTeamId)?.name
    return (
      <div className="rounded-lg border border-border-subtle bg-black/[0.02] px-3 py-2.5">
        <p className="text-[12px] text-text-secondary">
          Copied{teamName ? ` to ${teamName}` : ''} ·{' '}
          <Link
            // team=<installedTeamId> is TeamPage's one-shot override — without
            // it a multi-team viewer whose device has a different sticky team
            // (localStorage `tf.activeTeam.team`, which otherwise wins over
            // the server's last-acting default) would land on the wrong
            // team's prompts, not the one this install just landed on.
            to={orgHref('/team') + `?tab=prompts&team=${encodeURIComponent(installedTeamId)}`}
            className="text-accent font-medium hover:opacity-80"
          >
            Open prompts workspace
          </Link>
        </p>
        <p className="text-[11px] text-text-tertiary mt-0.5">
          Next: attach it to a trigger from your team&rsquo;s editor.
        </p>
      </div>
    )
  }

  if (expanded && multi) {
    return (
      <div className="flex items-center gap-2">
        <TeamPicker value={team} onChange={setTeam} label="Team" className="flex-1 max-w-[200px]" />
        <button
          type="button"
          onClick={() => install(team)}
          disabled={submitting || !ready || !team}
          className="text-[12px] font-medium text-white bg-accent px-3 py-1.5 rounded-full disabled:opacity-50 hover:opacity-90 transition-all"
        >
          {submitting ? 'Copying…' : 'Confirm copy'}
        </button>
      </div>
    )
  }

  return (
    <button
      type="button"
      onClick={() => (multi ? setExpanded(true) : install(team))}
      disabled={submitting || !ready}
      className="text-[12px] font-medium text-accent bg-accent-soft px-3 py-1.5 rounded-full hover:opacity-90 transition-all disabled:opacity-50"
    >
      {submitting ? 'Copying…' : 'Copy to my team'}
    </button>
  )
}

// formatFreshness renders a coarse "how long ago" hint for computed_at — a
// day-granularity readout matches the aggregation job's own daily-ish TTL,
// so a finer-grained "3 minutes ago" would overstate the freshness.
function formatFreshness(computedAt: string): string {
  const ms = Date.now() - new Date(computedAt).getTime()
  const hours = Math.floor(ms / (1000 * 60 * 60))
  if (hours < 1) return 'updated just now'
  if (hours < 24) return `updated ${hours}h ago`
  const days = Math.floor(hours / 24)
  return `updated ${days}d ago`
}

// ListingStatsBlock renders the detail-view run-derived stats block, or
// nothing when there's no run history yet — the same no-wrong-fallbacks
// rule the browse card's stats line follows.
function ListingStatsBlock({ stats }: { stats: ListingStats | undefined }) {
  const line = formatStatsLine(stats)
  if (!line || !stats) return null
  return (
    <div className="rounded-lg border border-border-subtle bg-black/[0.02] px-3 py-2 text-[12px] text-text-secondary">
      <span className="tabular-nums">{line}</span>
      <span className="text-text-tertiary"> · {formatFreshness(stats.computed_at)}</span>
    </div>
  )
}

function ErrorState({ message, onRetry }: { message: string; onRetry: () => void }) {
  return (
    <div className="flex flex-col items-center justify-center py-24">
      <div className="text-text-secondary text-[13px] max-w-md text-center mb-6">{message}</div>
      <button
        type="button"
        onClick={onRetry}
        className="inline-flex items-center gap-2 rounded-full bg-accent text-white text-[13px] font-medium px-5 py-2.5 transition-all hover:opacity-90"
      >
        Try again
      </button>
    </div>
  )
}

function EmptyState({ promptsHref }: { promptsHref: string }) {
  return (
    <div className="flex flex-col items-center justify-center py-24 text-center">
      <Store size={28} className="text-text-tertiary mb-4" />
      <h2 className="text-[15px] font-semibold text-text-primary mb-1">No listings yet</h2>
      <p className="text-[13px] text-text-secondary max-w-md mb-6">
        Publish a prompt or blueprint from your team&rsquo;s prompts workspace to make it
        discoverable by other teams in this org.
      </p>
      <Link
        to={promptsHref}
        className="inline-flex items-center gap-2 rounded-full bg-accent text-white text-[13px] font-medium px-4 py-2 transition-all hover:opacity-90"
      >
        Go to your team&rsquo;s prompts
      </Link>
    </div>
  )
}
