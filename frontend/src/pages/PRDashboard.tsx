import { useState, useEffect, useCallback, useRef } from 'react'
import {
  PieChart,
  Pie,
  Cell,
  ResponsiveContainer,
  AreaChart,
  Area,
  XAxis,
  YAxis,
  Tooltip,
} from 'recharts'
import {
  DndContext,
  DragOverlay,
  closestCenter,
  PointerSensor,
  useSensor,
  useSensors,
  type DragStartEvent,
  type DragEndEvent,
  type DragOverEvent,
} from '@dnd-kit/core'
import { SortableContext, verticalListSortingStrategy, useSortable } from '@dnd-kit/sortable'
import { CSS } from '@dnd-kit/utilities'
import PRCard from '../components/PRCard'
import { useWebSocket } from '../hooks/useWebSocket'
import type { WSEvent } from '../types'
import { apiFetch, apiJSON } from '../lib/apiClient'
import type { ListRequest } from '../lib/apiClient'
import { usePagedList } from '../hooks/usePagedList'
import type { PagedList } from '../hooks/usePagedList'

export interface PRSummary {
  /** The pull request's entity id: the key the board holds it by and the id
   *  its per-PR routes take. repo and number are display values. */
  entity_id: string
  number: number
  title: string
  repo: string
  author: string
  state: string
  draft: boolean
  labels: string[]
  created_at: string
  updated_at: string
  html_url: string
}

interface Stats {
  merged: number
  closed: number
  awaiting: number
  draft: number
  reviews_given: number
  reviews_received: number
  merged_over_time: { date: string; count: number }[]
}

type ColumnId = 'ready' | 'draft'

/** The board's four columns. Each is its own read of the list, narrowed and
 *  ordered by the server, so a column's count and its next page are about the
 *  column rather than about a page of the whole list. */
type BoardColumn = ColumnId | 'merged' | 'closed'

const BOARD_COLUMNS: BoardColumn[] = ['ready', 'draft', 'merged', 'closed']

const PR_LIST_PATH = '/api/dashboard/prs/list'

/** COLUMN_FILTERS is each column's list body: the states and draft filters
 *  that say what the column holds, newest GitHub activity first. */
const COLUMN_FILTERS: Record<BoardColumn, ListRequest> = {
  ready: { states: ['open'], draft: false, sort_key: 'updated_at' },
  draft: { states: ['open'], draft: true, sort_key: 'updated_at' },
  merged: { states: ['merged'], sort_key: 'updated_at' },
  closed: { states: ['closed'], sort_key: 'updated_at' },
}

function loadCached<T>(key: string): T | null {
  try {
    const raw = sessionStorage.getItem(key)
    return raw ? JSON.parse(raw) : null
  } catch {
    return null
  }
}

function saveCache(key: string, data: unknown) {
  try {
    sessionStorage.setItem(key, JSON.stringify(data))
  } catch {
    // best effort — sessionStorage may be full or disabled
  }
}

/** cacheKey is where a column's first page is cached for a cold open. */
const cacheKey = (col: BoardColumn) => `pr-dash-prs-${col}`

/** How many PRs one page of a column carries; "Load more" under a column
 *  reads its next page. */
const PR_PAGE_SIZE = 50

const PR_LIST_ERROR = 'Could not load your pull requests.'

export default function PRDashboard() {
  // One list per column. The hooks are called one by one because a hook
  // cannot be called in a loop.
  const ready = usePagedList<PRSummary>(PR_LIST_PATH, PR_LIST_ERROR)
  const draft = usePagedList<PRSummary>(PR_LIST_PATH, PR_LIST_ERROR)
  const merged = usePagedList<PRSummary>(PR_LIST_PATH, PR_LIST_ERROR)
  const closed = usePagedList<PRSummary>(PR_LIST_PATH, PR_LIST_ERROR)
  const columns: Record<BoardColumn, PagedList<PRSummary>> = { ready, draft, merged, closed }
  const [stats, setStats] = useState<Stats | null>(() => loadCached<Stats>('pr-dash-stats'))
  const [loading, setLoading] = useState(true)
  const [activeId, setActiveId] = useState<string | null>(null)
  const [overColumn, setOverColumn] = useState<ColumnId | null>(null)

  // Paint the last session's rows immediately, before the first fetch lands.
  // The hooks own the items, so each column's cache is seeded into its list
  // rather than being a second copy living beside it.
  const seededFromCache = useRef(false)
  if (!seededFromCache.current) {
    seededFromCache.current = true
    for (const col of BOARD_COLUMNS) {
      const cached = loadCached<PRSummary[]>(cacheKey(col))
      if (cached && cached.length > 0) columns[col].setItems(() => cached)
    }
  }

  // Tracks whether we've completed a first fetch. Used to avoid flashing
  // skeletons on every interval refresh — they should only show on cold load.
  // A ref (not state) because flipping it shouldn't trigger a re-render.
  const hasLoadedOnce = useRef(false)
  const { load: loadReady } = ready
  const { load: loadDraft } = draft
  const { load: loadMerged } = merged
  const { load: loadClosed } = closed
  const fetchAll = useCallback(async () => {
    if (!hasLoadedOnce.current) setLoading(true)
    const loads: Record<BoardColumn, typeof loadReady> = {
      ready: loadReady,
      draft: loadDraft,
      merged: loadMerged,
      closed: loadClosed,
    }
    try {
      // keepDepth: this board refreshes on an interval, on tab focus, and on
      // every debounced github:pr:* event, and a refresh that answered page 1
      // alone would throw away every "Load more" the user clicked. Each column
      // rereads as deep as it held.
      const [pages, statsRes] = await Promise.all([
        Promise.all(
          BOARD_COLUMNS.map((col) =>
            loads[col]({ ...COLUMN_FILTERS[col], page_size: PR_PAGE_SIZE }, { keepDepth: true }),
          ),
        ),
        apiJSON<Stats>('/api/dashboard/stats'),
      ])
      setStats(statsRes)
      // Only the first page is cached: the cache exists to paint something
      // instantly on a cold open, and a cold open starts at page 1 anyway.
      BOARD_COLUMNS.forEach((col, i) => {
        const page = pages[i]
        if (page) saveCache(cacheKey(col), page.items.slice(0, PR_PAGE_SIZE))
      })
      saveCache('pr-dash-stats', statsRes)
      hasLoadedOnce.current = true
    } catch (err) {
      // Surface to console so errors aren't silently swallowed.
      // Existing cached data (if any) stays on screen.
      console.error('PRDashboard fetchAll failed:', err)
    } finally {
      setLoading(false)
    }
  }, [loadReady, loadDraft, loadMerged, loadClosed])

  useEffect(() => {
    fetchAll()
  }, [fetchAll])

  useEffect(() => {
    // WS-driven updates cover the common case; keep a longer interval as
    // insurance against a missed message (useWebSocket auto-reconnects but
    // doesn't replay events dropped during a disconnect window). 10m is
    // generous enough to avoid redundant DB reads while still recovering
    // within a user's typical session.
    const interval = setInterval(fetchAll, 600000)
    const handleVis = () => {
      if (document.visibilityState === 'visible') fetchAll()
    }
    document.addEventListener('visibilitychange', handleVis)
    return () => {
      clearInterval(interval)
      document.removeEventListener('visibilitychange', handleVis)
    }
  }, [fetchAll])

  // Debounced refetch on PR-relevant events. A single poll cycle on one PR
  // can emit a burst of events (new commit + ci_check_* + review_requested);
  // coalescing into one refetch keeps the DB quiet without sacrificing
  // perceived responsiveness.
  //
  // Memoized with useCallback — an inline handler would create a fresh
  // function identity every render, causing useWebSocket's latest-ref
  // update effect to re-run on each render (the subscription itself stays
  // stable, but the ref-assignment effect is still a per-render tick).
  // Since fetchAll is stable (its loads are), this handler is effectively
  // created once for the lifetime of the component.
  const wsDebounceRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const handleWSEvent = useCallback(
    (event: WSEvent) => {
      if (event.type !== 'event') return
      const evt = event.data.event_type
      if (!evt || !evt.startsWith('github:pr:')) return
      if (wsDebounceRef.current) clearTimeout(wsDebounceRef.current)
      wsDebounceRef.current = setTimeout(() => {
        wsDebounceRef.current = null
        fetchAll()
      }, 500)
    },
    [fetchAll],
  )
  useWebSocket(handleWSEvent)

  useEffect(() => {
    return () => {
      if (wsDebounceRef.current) clearTimeout(wsDebounceRef.current)
    }
  }, [])

  const readyPRs = ready.items
  const draftPRs = draft.items
  const mergedPRs = merged.items
  const closedPRs = closed.items

  const prKey = (pr: PRSummary) => pr.entity_id
  // The draggable cards, by id: the two open columns.
  const prMap = new Map([...readyPRs, ...draftPRs].map((pr) => [prKey(pr), pr]))

  const getColumn = (id: string): ColumnId | null => {
    const pr = prMap.get(id)
    if (!pr) return null
    return pr.draft ? 'draft' : 'ready'
  }

  // columnCount is what a column's header counts: the server's total for its
  // filters, which covers the pages not loaded yet.
  const columnCount = (col: BoardColumn) => columns[col].total ?? columns[col].items.length

  const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 5 } }))

  const handleDragStart = (e: DragStartEvent) => setActiveId(String(e.active.id))
  const handleDragOver = (e: DragOverEvent) => {
    if (!e.over) {
      setOverColumn(null)
      return
    }
    const overId = String(e.over.id)
    if (overId === 'ready' || overId === 'draft') {
      setOverColumn(overId)
    } else {
      setOverColumn(getColumn(overId))
    }
  }

  // moveCard moves the card being dragged from one open column's held items to
  // the other's, ahead of the refetch that reconciles both with the server.
  const moveCard = (pr: PRSummary, from: ColumnId, to: ColumnId) => {
    const id = prKey(pr)
    columns[from].setItems((prev) => prev.filter((p) => prKey(p) !== id))
    columns[to].setItems((prev) => [{ ...pr, draft: to === 'draft' }, ...prev])
  }

  const handleDragEnd = async (e: DragEndEvent) => {
    setActiveId(null)
    setOverColumn(null)
    if (!e.over) return

    const id = String(e.active.id)
    const pr = prMap.get(id)
    if (!pr) return

    const sourceCol: ColumnId = pr.draft ? 'draft' : 'ready'
    const overId = String(e.over.id)
    const targetCol: ColumnId =
      overId === 'ready' || overId === 'draft' ? overId : getColumn(overId) || sourceCol

    if (sourceCol === targetCol) return

    // Optimistic update
    const makeDraft = targetCol === 'draft'
    moveCard(pr, sourceCol, targetCol)

    // Hit the API
    try {
      await apiFetch(`/api/dashboard/prs/${encodeURIComponent(pr.entity_id)}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ draft: makeDraft }),
      })
    } catch {
      // Revert on failure
      moveCard({ ...pr, draft: makeDraft }, targetCol, sourceCol)
    }

    fetchAll()
  }

  const activePR = activeId ? prMap.get(activeId) : null

  return (
    <div className="max-w-5xl mx-auto flex flex-col" style={{ minHeight: '100%' }}>
      <div className="flex items-center justify-between mb-6">
        <h1 className="text-xl font-semibold text-ink-1">Pull Requests</h1>
        <button
          onClick={fetchAll}
          disabled={loading}
          className="text-ui text-warm hover:text-warm/70 font-medium transition-colors disabled:opacity-50"
        >
          {loading ? 'Refreshing...' : 'Refresh'}
        </button>
      </div>

      {/* Chart strip */}
      <div className="grid grid-cols-4 gap-4 mb-8">
        <ChartCard title="Status">
          {stats ? <StatusDonut stats={stats} /> : <SkeletonDonut />}
        </ChartCard>
        <ChartCard title="Merged (14 days)">
          {stats ? <MergedTimeline data={stats.merged_over_time || []} /> : <SkeletonChart />}
        </ChartCard>
        <ChartCard title="Review balance">
          {stats ? (
            <ReviewBalance given={stats.reviews_given} received={stats.reviews_received} />
          ) : (
            <SkeletonBar />
          )}
        </ChartCard>
        <ChartCard title="30-day totals">
          {stats ? <TotalsSummary stats={stats} /> : <SkeletonTotals />}
        </ChartCard>
      </div>

      {/* PR columns with drag */}
      <DndContext
        sensors={sensors}
        collisionDetection={closestCenter}
        onDragStart={handleDragStart}
        onDragOver={handleDragOver}
        onDragEnd={handleDragEnd}
      >
        <div className="grid grid-cols-2 gap-6 flex-1 min-h-0">
          <DroppableColumn
            id="ready"
            title="Ready for review"
            count={columnCount('ready')}
            isOver={overColumn === 'ready' && getColumn(activeId!) !== 'ready'}
          >
            <SortableContext items={readyPRs.map(prKey)} strategy={verticalListSortingStrategy}>
              {readyPRs.length === 0 ? (
                <p className="text-ui text-ink-3 text-center py-8">No open PRs</p>
              ) : (
                readyPRs.map((pr) => <SortablePRCard key={prKey(pr)} id={prKey(pr)} pr={pr} />)
              )}
            </SortableContext>
            <LoadMore list={ready} label="ready pull requests" />
          </DroppableColumn>

          <DroppableColumn
            id="draft"
            title="Drafts"
            count={columnCount('draft')}
            isOver={overColumn === 'draft' && getColumn(activeId!) !== 'draft'}
          >
            <SortableContext items={draftPRs.map(prKey)} strategy={verticalListSortingStrategy}>
              {draftPRs.length === 0 ? (
                <p className="text-ui text-ink-3 text-center py-8">No drafts</p>
              ) : (
                draftPRs.map((pr) => <SortablePRCard key={prKey(pr)} id={prKey(pr)} pr={pr} />)
              )}
            </SortableContext>
            <LoadMore list={draft} label="drafts" />
          </DroppableColumn>
        </div>

        <DragOverlay>
          {activePR ? (
            <div className="opacity-80 rotate-2">
              <PRCard pr={activePR} />
            </div>
          ) : null}
        </DragOverlay>
      </DndContext>

      {/* Merged & closed — non-draggable history */}
      {(mergedPRs.length > 0 || closedPRs.length > 0) && (
        <div className="grid grid-cols-2 gap-6 mt-6">
          <StaticColumn title="Recently merged" count={columnCount('merged')}>
            {mergedPRs.length === 0 ? (
              <p className="text-ui text-ink-3 text-center py-8">No merged PRs</p>
            ) : (
              mergedPRs.map((pr) => <PRCard key={prKey(pr)} pr={pr} />)
            )}
            <LoadMore list={merged} label="merged pull requests" />
          </StaticColumn>
          <StaticColumn title="Closed" count={columnCount('closed')}>
            {closedPRs.length === 0 ? (
              <p className="text-ui text-ink-3 text-center py-8">No closed PRs</p>
            ) : (
              closedPRs.map((pr) => <PRCard key={prKey(pr)} pr={pr} />)
            )}
            <LoadMore list={closed} label="closed pull requests" />
          </StaticColumn>
        </div>
      )}
    </div>
  )
}

/** LoadMore reads a column's next page, and renders nothing once the column
 *  holds its last one. */
function LoadMore({ list, label }: { list: PagedList<PRSummary>; label: string }) {
  if (!list.hasMore) return null
  return (
    <div className="text-center">
      <button
        onClick={() => void list.loadMore()}
        disabled={list.loading}
        aria-label={`Load more ${label}`}
        className="text-[12px] text-warm hover:text-warm/70 font-medium transition-colors disabled:opacity-50"
      >
        Load more
        {list.total !== null ? ` (${list.items.length} of ${list.total})` : ''}
      </button>
    </div>
  )
}

function SortablePRCard({ id, pr }: { id: string; pr: PRSummary }) {
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({
    id,
  })
  const style = {
    transform: CSS.Transform.toString(transform),
    transition,
    opacity: isDragging ? 0.3 : 1,
  }

  return (
    <div
      ref={setNodeRef}
      style={style}
      className="cursor-grab active:cursor-grabbing"
      {...attributes}
      {...listeners}
    >
      <PRCard pr={pr} />
    </div>
  )
}

function DroppableColumn({
  id,
  title,
  count,
  isOver,
  children,
}: {
  id: string
  title: string
  count: number
  isOver: boolean
  children: React.ReactNode
}) {
  const { setNodeRef } = useSortable({ id, data: { type: 'column' } })

  return (
    <div className="flex flex-col min-h-0">
      <h2 className="text-body font-medium text-ink-2 mb-3 px-1 shrink-0">
        {title}
        <span className="ml-2 text-ink-3 bg-tint-3 rounded-full px-2 py-0.5 text-reported">
          {count}
        </span>
      </h2>
      <div
        ref={setNodeRef}
        className={`rounded-2xl border border-line-1 p-3 space-y-3 flex-1 overflow-y-auto transition-colors ${
          isOver ? 'bg-warm/5 border-warm/20' : 'bg-tint-1'
        }`}
      >
        {children}
      </div>
    </div>
  )
}

function StaticColumn({
  title,
  count,
  children,
}: {
  title: string
  count: number
  children: React.ReactNode
}) {
  return (
    <div className="flex flex-col min-h-0">
      <h2 className="text-body font-medium text-ink-2 mb-3 px-1 shrink-0">
        {title}
        <span className="ml-2 text-ink-3 bg-tint-3 rounded-full px-2 py-0.5 text-reported">
          {count}
        </span>
      </h2>
      <div className="rounded-2xl border border-line-1 p-3 space-y-3 flex-1 overflow-y-auto bg-tint-1 max-h-[400px]">
        {children}
      </div>
    </div>
  )
}

// --- Charts ---

function ChartCard({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="bg-raised backdrop-blur-xl border border-line-1 rounded-2xl p-4 shadow-float shadow-black/[0.03]">
      <h3 className="text-reported font-medium text-ink-3 mb-3">{title}</h3>
      {children}
    </div>
  )
}

const COLORS = {
  merged: 'var(--color-ink-1)',
  closed: 'var(--color-alarm)',
  awaiting: 'var(--color-warm)',
  draft: 'var(--color-ink-3)',
}

function StatusDonut({ stats }: { stats: Stats }) {
  const data = [
    { name: 'Merged', value: stats.merged, color: COLORS.merged },
    { name: 'Closed', value: stats.closed, color: COLORS.closed },
    { name: 'Awaiting', value: stats.awaiting, color: COLORS.awaiting },
    { name: 'Draft', value: stats.draft, color: COLORS.draft },
  ].filter((d) => d.value > 0)

  const total = data.reduce((sum, d) => sum + d.value, 0)
  if (total === 0) return <p className="text-ui text-ink-3 text-center py-4">No data</p>

  return (
    <div className="flex items-center gap-3">
      <div className="w-16 h-16">
        <ResponsiveContainer>
          <PieChart>
            <Pie
              data={data}
              cx="50%"
              cy="50%"
              innerRadius={18}
              outerRadius={30}
              dataKey="value"
              stroke="none"
            >
              {data.map((d, i) => (
                <Cell key={i} fill={d.color} />
              ))}
            </Pie>
          </PieChart>
        </ResponsiveContainer>
      </div>
      <div className="space-y-0.5">
        {data.map((d) => (
          <div key={d.name} className="flex items-center gap-1.5 text-reported">
            <span className="w-1.5 h-1.5 rounded-full" style={{ background: d.color }} />
            <span className="text-ink-3">
              {d.value} {d.name.toLowerCase()}
            </span>
          </div>
        ))}
      </div>
    </div>
  )
}

function MergedTimeline({ data }: { data: { date: string; count: number }[] }) {
  if (data.length === 0) return <p className="text-ui text-ink-3 text-center py-4">No data</p>

  const formatted = data.map((d) => ({
    ...d,
    label: new Date(d.date + 'T00:00:00').toLocaleDateString([], {
      month: 'short',
      day: 'numeric',
    }),
  }))

  return (
    <div className="h-16">
      <ResponsiveContainer>
        <AreaChart data={formatted}>
          <defs>
            <linearGradient id="mergedGrad" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="var(--color-ink-1)" stopOpacity={0.3} />
              <stop offset="100%" stopColor="var(--color-ink-1)" stopOpacity={0} />
            </linearGradient>
          </defs>
          <XAxis dataKey="label" hide />
          <YAxis hide />
          <Tooltip
            contentStyle={{
              background: 'rgba(255,255,255,0.8)',
              backdropFilter: 'blur(12px)',
              border: '1px solid rgba(255,255,255,0.7)',
              borderRadius: '8px',
              fontSize: '11px',
              boxShadow: '0 4px 12px rgba(0,0,0,0.05)',
            }}
            formatter={(value) => {
              const n = typeof value === 'number' ? value : Number(value)
              return [`${n} PR${n !== 1 ? 's' : ''}`, 'Merged']
            }}
            labelFormatter={(label) => String(label)}
          />
          <Area
            type="monotone"
            dataKey="count"
            stroke="var(--color-ink-1)"
            strokeWidth={2}
            fill="url(#mergedGrad)"
          />
        </AreaChart>
      </ResponsiveContainer>
    </div>
  )
}

function ReviewBalance({ given, received }: { given: number; received: number }) {
  const total = given + received
  if (total === 0) return <p className="text-ui text-ink-3 text-center py-4">No reviews yet</p>

  const givenPct = (given / total) * 100
  const net = given - received
  const label = net > 0 ? `+${net} net given` : net < 0 ? `${net} net received` : 'balanced'

  return (
    <div className="space-y-2">
      <div className="flex h-2.5 rounded-full overflow-hidden bg-tint-3">
        <div
          className="h-full rounded-l-full"
          style={{ width: `${givenPct}%`, background: 'var(--color-ink-2)' }}
        />
        <div
          className="h-full rounded-r-full"
          style={{ width: `${100 - givenPct}%`, background: 'var(--color-warm)' }}
        />
      </div>
      <div className="flex justify-between text-reported">
        <span className="text-ink-2">{given} given</span>
        <span className="text-warm">{received} received</span>
      </div>
      <p className="text-reported text-ink-3 text-center">{label}</p>
    </div>
  )
}

function TotalsSummary({ stats }: { stats: Stats }) {
  const total = stats.merged + stats.closed + stats.awaiting + stats.draft
  return (
    <div className="space-y-2">
      <div className="text-center">
        <span className="text-2xl font-semibold text-ink-1">{total}</span>
        <p className="text-reported text-ink-3">total PRs</p>
      </div>
      <div className="grid grid-cols-2 gap-x-3 gap-y-1 text-reported">
        <div className="flex items-center gap-1.5">
          <span className="w-1.5 h-1.5 rounded-full bg-ink-1" />
          <span className="text-ink-3">{stats.merged} merged</span>
        </div>
        <div className="flex items-center gap-1.5">
          <span className="w-1.5 h-1.5 rounded-full bg-alarm" />
          <span className="text-ink-3">{stats.closed} closed</span>
        </div>
        <div className="flex items-center gap-1.5">
          <span className="w-1.5 h-1.5 rounded-full bg-warm" />
          <span className="text-ink-3">{stats.awaiting} open</span>
        </div>
        <div className="flex items-center gap-1.5">
          <span className="w-1.5 h-1.5 rounded-full" style={{ background: 'var(--color-ink-3)' }} />
          <span className="text-ink-3">{stats.draft} draft</span>
        </div>
      </div>
    </div>
  )
}

// --- Skeletons ---
const shimmer = 'animate-pulse bg-tint-3 rounded'

function SkeletonDonut() {
  return (
    <div className="flex items-center gap-3">
      <div className={`w-16 h-16 rounded-full ${shimmer}`} />
      <div className="space-y-1.5 flex-1">
        <div className={`h-2.5 w-16 ${shimmer}`} />
        <div className={`h-2.5 w-12 ${shimmer}`} />
        <div className={`h-2.5 w-14 ${shimmer}`} />
      </div>
    </div>
  )
}

function SkeletonChart() {
  return (
    <div className="h-16 flex items-end gap-1.5 px-1">
      {[40, 60, 35, 80, 55].map((h, i) => (
        <div key={i} className={`flex-1 ${shimmer}`} style={{ height: `${h}%` }} />
      ))}
    </div>
  )
}

function SkeletonBar() {
  return (
    <div className="space-y-2">
      <div className={`h-2.5 w-full rounded-full ${shimmer}`} />
      <div className="flex justify-between">
        <div className={`h-2.5 w-12 ${shimmer}`} />
        <div className={`h-2.5 w-14 ${shimmer}`} />
      </div>
      <div className={`h-2.5 w-16 mx-auto ${shimmer}`} />
    </div>
  )
}

function SkeletonTotals() {
  return (
    <div className="space-y-2">
      <div className="text-center">
        <div className={`h-7 w-10 mx-auto mb-1 ${shimmer}`} />
        <div className={`h-3 w-14 mx-auto ${shimmer}`} />
      </div>
      <div className="grid grid-cols-2 gap-x-3 gap-y-1 text-reported">
        {[1, 2, 3, 4].map((i) => (
          <div key={i} className="flex items-center gap-1.5">
            <span className={`w-1.5 h-1.5 rounded-full ${shimmer}`} />
            <span className={`h-3 w-14 ${shimmer}`} />
          </div>
        ))}
      </div>
    </div>
  )
}
