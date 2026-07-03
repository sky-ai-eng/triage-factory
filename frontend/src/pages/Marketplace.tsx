import { useState, useEffect, useCallback, useMemo } from 'react'
import { Link } from 'react-router-dom'
import { AnimatePresence, motion } from 'motion/react'
import { Store, ThumbsUp, X } from 'lucide-react'
import { useOrgHref } from '../hooks/useOrgHref'
import { readError } from '../lib/api'
import { toast } from '../components/Toast/toastStore'
import SearchField from '../components/SearchField'
import type { EventType } from '../types'

// Within-org prompt marketplace browse page (TFAC-537). Flat searchable list
// + event-type facets — no folder tree, no free-form tags (TFAC-92's V1
// scoping decision). Multi-mode only: this page is reachable only via the
// MultiRoutes route table (nothing in LocalRoutes) and the backend gates
// every /api/marketplace/* call behind runmode + the org's ship-dark
// marketplace_enabled toggle, so a disabled org simply 404s the list fetch
// below rather than needing a client-side flag check.

// Wire shapes mirror domain.ListingSummary / domain.ListingDetail
// (internal/domain/marketplace.go) — only the fields this page reads.
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

type SortOrder = 'installs' | 'votes' | 'recent'
type KindFilter = '' | 'prompt' | 'blueprint'

const kindLabel = (kind: string) => (kind === 'blueprint' ? 'Blueprint' : 'Prompt')
const kindBadgeClass = (kind: string) =>
  kind === 'blueprint' ? 'bg-accent/10 text-accent' : 'bg-black/[0.04] text-text-tertiary'

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
    fetch('/api/event-types')
      .then((r) => (r.ok ? r.json() : []))
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
      const res = await fetch(`/api/marketplace/listings?${qs}`)
      if (!res.ok) {
        const msg = await readError(res, 'Failed to load marketplace listings')
        setLoadError(msg)
        return
      }
      setListings(await res.json())
    } catch (err) {
      setLoadError(
        `Failed to load marketplace listings: ${err instanceof Error ? err.message : String(err)}`,
      )
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
      const res = await fetch(`/api/marketplace/listings/${encodeURIComponent(id)}`)
      if (!res.ok) {
        toast.error(await readError(res, 'Failed to load listing'))
        setSelectedId(null)
        return
      }
      setDetail(await res.json())
    } catch (err) {
      toast.error(`Failed to load listing: ${err instanceof Error ? err.message : String(err)}`)
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
        const res = await fetch(
          `/api/marketplace/listings/${encodeURIComponent(listing.id)}/vote`,
          {
            method: nextVoted ? 'PUT' : 'DELETE',
          },
        )
        if (!res.ok) {
          toast.error(await readError(res, 'Failed to update your vote'))
          refresh()
        }
      } catch (err) {
        toast.error(
          `Failed to update your vote: ${err instanceof Error ? err.message : String(err)}`,
        )
        refresh()
      }
    },
    [refresh],
  )

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

        <div className="mt-auto pt-3 text-[11px] text-text-tertiary tabular-nums">
          {listing.install_count} install{listing.install_count === 1 ? '' : 's'}
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
}: {
  open: boolean
  detail: ListingDetail | null
  loading: boolean
  catalogById: Map<string, EventType>
  onClose: () => void
  onToggleVote: (listing: ListingSummary) => void
}) {
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
                    <button
                      type="button"
                      disabled
                      title="Copy-to-team install lands in a follow-up ticket (TFAC-538)"
                      className="ml-auto text-[12px] font-medium text-text-tertiary bg-black/[0.03] px-3 py-1.5 rounded-full cursor-not-allowed"
                    >
                      Copy to my team
                    </button>
                  </div>

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
