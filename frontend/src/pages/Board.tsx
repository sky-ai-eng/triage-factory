import { memo, useState, useEffect, useLayoutEffect, useCallback, useMemo, useRef } from 'react'
import type { Task, Conversation, Message, WSEvent, TeamMember, TeamBot } from '../types'
import { useWebSocket, setPresenceView } from '../hooks/useWebSocket'
import { usePermissionQueues } from '../hooks/usePermissionQueues'
import {
  isActiveConversation,
  isActiveStatus,
  isLiveRun,
  isPermissionTerminalStatus,
  isTerminalStatus,
} from '../lib/conversationStatus'
import { approvalCounts, hasUnresolvedArtifacts } from '../lib/approval'
import {
  appendToFeed,
  feedFromMessages,
  EMPTY_FEED,
  type ConversationCardFeed,
} from '../lib/conversationFeed'
import type { PendingPermission, PermissionDecisionInput } from '../lib/permissions'
import { useTeams, useTeamFilter } from '../hooks/useTeams'
import { useTeamMembers } from '../hooks/useDeploymentConfig'
import { usableBot } from '../lib/teamRoster'
import { usePagedList } from '../hooks/usePagedList'
import type { PagedList } from '../hooks/usePagedList'
import {
  TASK_FACETS_PATH,
  TASK_LIST_PATH,
  doneListBody,
  facetsBody,
  queuedLaneBody,
  statusListBody,
  type TaskFacetsResponse,
  type TaskListRequest,
} from '../lib/taskList'
import { useOrgRole } from '../hooks/useOrgRole'
import TeamScopeSelect from '../components/TeamScopeSelect'
import ZeroTeamState from '../components/ZeroTeamState'
import AgentCard from '../components/AgentCard'
import TaskCard from '../components/TaskCard'
import PromptPicker from '../components/PromptPicker'
import ReviewOverlay from '../components/ReviewOverlay'
import PendingPROverlay from '../components/PendingPROverlay'
import ResolveAllConfirm from '../components/ResolveAllConfirm'
import AssigneePicker from '../components/board/AssigneePicker'
import RequeueConfirm from '../components/board/RequeueConfirm'
import { toast } from '../components/Toast/toastStore'
import { apiErrors, apiFetch, apiJSON, httpErrorMessage } from '../lib/apiClient'
import BoardColumn, { CollapsedColumn, type LanePaging } from '../components/board/BoardColumn'
import {
  emptyLaneFilter,
  laneFilterFields,
  narrows,
  type LaneFilter,
} from '../components/board/laneFilter'
import { GlassBackdrop } from './setup/glass'
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

// Three lanes on the board: the task lifecycle, and nothing else. A held-but-
// unstarted task sits in Queued wearing its assignee mark rather than in a
// lane of its own, and needs-you lives on the card (its frame, its place at
// the head of the lane) rather than in a column. Column ids double as drop
// targets — keep them lowercase + stable since they're persisted in
// localStorage filter keys.
type ColumnId = 'queued' | 'in_progress' | 'done'

const ALL_COLUMNS: ColumnId[] = ['queued', 'in_progress', 'done']

const COLUMN_TITLES: Record<ColumnId, string> = {
  queued: 'Queued',
  in_progress: 'In Progress',
  done: 'Done',
}

// Lane geometry, used to center the strip on the midpoint of the *open*
// columns. COL_W must match the expanded BoardColumn width (w-[430px]); RAIL_W
// the collapsed CollapsedColumn (w-5 = 20px); GAP the row's gap-6 (24px).
const COL_W = 430
const RAIL_W = 20
const GAP = 24

// How long a lane waits after the last keystroke or pill before asking the
// server the new question. Every filter field is a server read now, so
// without this a search would fire a list read per character.
const FILTER_DEBOUNCE_MS = 250

// Filter persistence: per-user, per-column. Storage key is namespaced
// by the user's id so a re-login (different user on the same browser)
// loads their own filters and doesn't overwrite the prior user's. The
// pre-load default ("anon") is only used briefly during the /api/me
// roundtrip; once currentUserID resolves the effect below re-loads
// against the per-user key.
const FILTER_STORAGE_PREFIX = 'sky330.board.filters.v1'

function filterStorageKey(userID: string): string {
  return `${FILTER_STORAGE_PREFIX}.${userID || 'anon'}`
}

type FilterMap = Record<ColumnId, LaneFilter>

function loadFilters(userID: string): FilterMap {
  try {
    const raw = localStorage.getItem(filterStorageKey(userID))
    if (!raw) return defaultFilters()
    const parsed = JSON.parse(raw) as Partial<FilterMap>
    const out = defaultFilters()
    for (const col of ALL_COLUMNS) {
      if (parsed[col]) {
        out[col] = { ...emptyLaneFilter, ...parsed[col] }
      }
    }
    return out
  } catch {
    return defaultFilters()
  }
}

function defaultFilters(): FilterMap {
  return {
    queued: { ...emptyLaneFilter },
    in_progress: { ...emptyLaneFilter },
    done: { ...emptyLaneFilter },
  }
}

// filterSignature is the lane's query as the server would see it: two
// filters that send the same body are the same question, so a stored filter
// that matches the one already fetched with earns no second read.
function filterSignature(f: LaneFilter): string {
  return JSON.stringify(laneFilterFields(f))
}

// Collapse persistence: same per-user localStorage scheme as filters. Every
// lane starts open — three lanes fit without shrinking anything. (A per-user
// backend UI-prefs store is the eventual home; localStorage holds the line
// until then.)
type CollapseMap = Record<ColumnId, boolean>

const COLLAPSE_STORAGE_PREFIX = 'sky330.board.collapsed.v1'

function collapseStorageKey(userID: string): string {
  return `${COLLAPSE_STORAGE_PREFIX}.${userID || 'anon'}`
}

function defaultCollapsed(): CollapseMap {
  return { queued: false, in_progress: false, done: false }
}

function loadCollapsed(userID: string): CollapseMap {
  try {
    const raw = localStorage.getItem(collapseStorageKey(userID))
    if (!raw) return defaultCollapsed()
    const parsed = JSON.parse(raw) as Partial<CollapseMap>
    const out = defaultCollapsed()
    for (const col of ALL_COLUMNS) {
      if (typeof parsed[col] === 'boolean') out[col] = parsed[col] as boolean
    }
    return out
  } catch {
    return defaultCollapsed()
  }
}

// The two gestures that end a task, each gated behind the resolve-all
// confirmation when the task still carries unresolved artifacts: ending a
// task is what tears them down.
type EndAction = 'complete' | 'dismiss'

export default function Board() {
  // One paged list per column — three filter sets over the one tasks list
  // route. Each holds its own page token, so a deep column pages
  // independently of the others.
  const queuedList = usePagedList<Task>(TASK_LIST_PATH, 'Could not load the queue.')
  const inProgressList = usePagedList<Task>(TASK_LIST_PATH, 'Could not load the in-progress tasks.')
  const doneList = usePagedList<Task>(TASK_LIST_PATH, 'Could not load the finished tasks.')
  const lists: Record<ColumnId, PagedList<Task>> = {
    queued: queuedList,
    in_progress: inProgressList,
    done: doneList,
  }
  const queued = queuedList.items
  const inProgress = inProgressList.items
  const done = doneList.items
  // The loaders are stable across renders; the list objects around them are
  // not. The fetchers close over these rather than the lists so their own
  // identity stays stable — the WS refresh and the mount effect both key off
  // fetchTasks, and a per-render identity would refetch the board on every
  // render.
  const loadQueued = queuedList.load
  const loadInProgress = inProgressList.load
  const loadDone = doneList.load
  const [loading, setLoading] = useState(true)

  // Presence (TFAC-392): the board is an answer-capable surface for permission
  // prompts (it renders + answers them inline), so report it while mounted and
  // fall back to 'other' on unmount so an unattended conversation fast-denies once the
  // operator leaves the board.
  useEffect(() => {
    setPresenceView('board')
    return () => setPresenceView('other')
  }, [])

  // Agent conversation state — conversations render on cards regardless of which column
  // the card is in (a user-claimed in_progress task can also have a
  // conversation; a bot-claimed one definitely has).
  const [conversations, setConversations] = useState<Record<string, Conversation>>({})
  // Per-conversation card feed (running stats + last few ticker lines), keyed by conversation
  // ID. Bounded by construction — the board used to accumulate every conversation's
  // full message array here, growing without limit for the lifetime of the
  // page while each card re-derived its stats from scratch per render.
  const [conversationFeeds, setConversationFeeds] = useState<Record<string, ConversationCardFeed>>(
    {},
  )
  const [chainStepConversations, setChainStepConversations] = useState<
    Record<string, Conversation[]>
  >({})
  const chainStepConversationsRef = useRef(chainStepConversations)
  useEffect(() => {
    chainStepConversationsRef.current = chainStepConversations
  }, [chainStepConversations])

  // Tool-permission prompts for every conversation on the board, keyed by conversation ID. The
  // shared queue core (same one useConversationDetail uses) owns dedup + per-prompt TTL
  // timers; the board ingests `permission_request` and drops a conversation's queue when
  // it finishes or leaves the board. A queuesRef mirrors the map so the
  // leave-the-board reconciliation effect can read it without re-running on
  // every ingest (which would race a just-ingested prompt against its
  // not-yet-seeded conversation).
  const {
    queues: permQueueMap,
    refresh: refreshPermissions,
    resolve: resolvePermission,
    dropConversation: dropPermissionConversation,
  } = usePermissionQueues()
  const queuesRef = useRef(permQueueMap)
  useEffect(() => {
    queuesRef.current = permQueueMap
  }, [permQueueMap])

  // Team roster for the assignee picker, on the team the board is acting as
  // (the same one the write pickers seed to) rather than the org's default.
  // The bot is filtered to the one the delegate handlers would accept: a
  // disabled bot is not an assignable option.
  const { members, bot: rosterBot } = useTeamMembers()
  const bot = useMemo(() => usableBot(rosterBot), [rosterBot])
  const [currentUserID, setCurrentUserID] = useState<string>('')

  // multi-team. teamFilter is the per-page read scope (threaded
  // into every lane fetch as team_id); `teams` backs the row
  // color-coding. Both render their UI only at ≥2 teams. teamFilterRef
  // keeps the fetchers' identity stable while always reading the latest.
  const { teams, loaded: teamsLoaded } = useTeams()
  const { isAdmin: orgIsAdmin } = useOrgRole()
  const [teamFilter, setTeamFilter] = useTeamFilter('board')
  const teamFilterRef = useRef(teamFilter)
  useEffect(() => {
    teamFilterRef.current = teamFilter
  }, [teamFilter])

  // Per-column filter state. Persisted to localStorage under a key
  // namespaced by the current user's id so a re-login (different user
  // on the same browser) loads that user's filters instead of the
  // previous user's. We initialise from the anon key for the brief
  // pre-/api/me window; once currentUserID resolves, the effect below
  // re-loads from the per-user key.
  //
  // filtersOwnerID tracks which user the current filter STATE was
  // loaded for (separate from currentUserID, which is the LATEST
  // user). The save effect gates on the two matching — otherwise the
  // first render after /api/me resolves would write the still-in-
  // state anon defaults to the per-user key before the load effect's
  // setFilters lands, silently clobbering the user's saved filters.
  // React batches the load effect's two setState calls into one
  // re-render, so on the following effect pass owner === current and
  // the save proceeds with the loaded filters.
  const [filters, setFilters] = useState<FilterMap>(() => loadFilters(''))
  const [filtersOwnerID, setFiltersOwnerID] = useState<string>('')
  useEffect(() => {
    if (!currentUserID) return
    setFilters(loadFilters(currentUserID))
    setFiltersOwnerID(currentUserID)
  }, [currentUserID])
  useEffect(() => {
    if (filtersOwnerID !== currentUserID) return
    try {
      localStorage.setItem(filterStorageKey(currentUserID), JSON.stringify(filters))
    } catch {
      // Quota / disabled storage — silently skip; filters work in-memory.
    }
  }, [filters, filtersOwnerID, currentUserID])
  // The fetchers read the filters through a ref so their identity stays
  // stable; the filter effect below is what turns a change into a read.
  const filtersRef = useRef(filters)

  // Collapsed-lane state — same per-user persistence dance as filters.
  const [collapsed, setCollapsed] = useState<CollapseMap>(() => loadCollapsed(''))
  const [collapsedOwnerID, setCollapsedOwnerID] = useState<string>('')
  useEffect(() => {
    if (!currentUserID) return
    setCollapsed(loadCollapsed(currentUserID))
    setCollapsedOwnerID(currentUserID)
  }, [currentUserID])
  useEffect(() => {
    if (collapsedOwnerID !== currentUserID) return
    try {
      localStorage.setItem(collapseStorageKey(currentUserID), JSON.stringify(collapsed))
    } catch {
      // Quota / disabled storage — silently skip; collapse works in-memory.
    }
  }, [collapsed, collapsedOwnerID, currentUserID])
  const toggleCollapse = useCallback(
    (col: ColumnId) => setCollapsed((c) => ({ ...c, [col]: !c[col] })),
    [],
  )

  // Snoozed visibility toggle for the Queued column. Off by default;
  // snoozed tasks are intentionally deferred and don't need to clutter
  // the column. When on, they render at the tail with a "wakes Mar 5"
  // badge (handled by TaskCard's existing SnoozedBadge).
  const [showSnoozed, setShowSnoozed] = useState(false)
  const showSnoozedRef = useRef(showSnoozed)

  // The event types each lane holds under its current query, from the facet
  // read beside each lane read. The chips are drawn from this rather than
  // from the page, so a type on an unfetched page still gets its chip.
  const [facets, setFacets] = useState<Record<ColumnId, string[]>>({
    queued: [],
    in_progress: [],
    done: [],
  })

  // Drag state. The "over column" highlight is owned by BoardColumn
  // itself (via useDroppable's isOver), so we don't need to track it
  // up here anymore.
  const [activeId, setActiveId] = useState<string | null>(null)
  // The column the drag is currently over — resolved at the board level so it
  // counts hovering a *card* inside a column, not just the column's empty area
  // (each card is its own droppable, so the column's own isOver misses them).
  const [activeDropCol, setActiveDropCol] = useState<ColumnId | null>(null)

  // Delegate flow
  const [showPromptPicker, setShowPromptPicker] = useState(false)
  const pendingDelegateTask = useRef<Task | null>(null)
  // Tracks bot-claimed tasks where the delegate conversation failed
  // to fire. Cleared when a conversation for the task lands.
  const [delegateFailures, setDelegateFailures] = useState<Record<string, string>>({})

  // Per-item approval overlay (open one unresolved artifact's editor).
  const [approvalCtx, setApprovalCtx] = useState<{
    conversationID: string
    kind: 'review' | 'pr'
    artifactId: string
  } | null>(null)

  // Resolve-all confirmation (TFAC-384 §4): the two gestures that END a task —
  // drag-to-Done from In Progress (complete) and from Queued (dismiss) —
  // force-resolve every unresolved artifact and cancel a live conversation.
  // When the target task has unresolved artifacts we stash the intended
  // action here and gate the request behind the confirmation modal.
  const [confirmResolve, setConfirmResolve] = useState<{
    taskId: string
    action: EndAction
    prCount: number
    reviewCount: number
    isLive: boolean
  } | null>(null)
  const [confirmBusy, setConfirmBusy] = useState(false)

  // The task awaiting the return-to-queue confirmation, when its run is in
  // flight. Null the rest of the time: a requeue with nothing running fires
  // without asking.
  const [pendingRequeue, setPendingRequeue] = useState<{ taskId: string } | null>(null)

  // Fetches a blueprint run's step structure and pads it into a length-N
  // array of step conversations — synthetic 'pending' placeholders for steps without
  // a conversation yet — the shape the chain rail renders. Pure: returns the array
  // (null on error / empty) and writes no state, so the enrichment pass can
  // resolve many chains in parallel and apply them in one batched
  // setChainStepConversations.
  const fetchChainStepConversations = useCallback(
    async (blueprintRunID: string): Promise<Conversation[] | null> => {
      try {
        const data = await apiJSON<{
          steps?: Array<{ step: { step_index: number }; run?: Conversation | null }>
        }>(`/api/blueprint-runs/${blueprintRunID}`)
        const total = data.steps?.length ?? 0
        if (total === 0) return null
        return Array.from({ length: total }, (_, i) => {
          const existing = data.steps?.[i]?.run
          if (existing) return existing
          // A step with no conversation yet. Its status is empty rather than a
          // made-up name: the classifiers read an unrecognized value as
          // nothing at all, and inventing one would put a word in the
          // conversation vocabulary that no conversation can carry. The
          // `__pending-` id is what marks the row synthetic.
          return {
            ID: `__pending-${blueprintRunID}-${i}`,
            Status: '',
            blueprint_run_id: blueprintRunID,
            blueprint_step_index: i,
          } as unknown as Conversation
        })
      } catch {
        // Network error — leave chain indicator empty for now; the
        // next fetchTasks pass will retry.
        return null
      }
    },
    [],
  )

  // Seeds chainStepConversations for one task from its blueprint run. Thin wrapper
  // over fetchChainStepConversations for the WS handlers (which seed one task at a
  // time); the enrichment pass batches via fetchChainStepConversations directly.
  const seedChainStepConversations = useCallback(
    async (taskID: string, blueprintRunID: string) => {
      const steps = await fetchChainStepConversations(blueprintRunID)
      if (steps) setChainStepConversations((prev) => ({ ...prev, [taskID]: steps }))
    },
    [fetchChainStepConversations],
  )

  // The caller's identity, once at mount. /api/me returns it as `id`, not
  // `user_id` — the picker uses currentUserID to recognize "claimed by me",
  // and without this read every click on Me would take the claim path instead
  // of toggling unclaim. A failure leaves the picker degraded (no "you"
  // highlight) but the board working, which is why it swallows rather than
  // rejects; the roster half degrades the same way inside its own hook.
  useEffect(() => {
    void (async () => {
      const meRes = await apiJSON<unknown>('/api/me').catch(() => null)
      if (meRes && typeof meRes === 'object' && 'id' in meRes) {
        setCurrentUserID((meRes as { id: string }).id ?? '')
      }
    })()
  }, [])

  // Agent conversations for every task on the board — a queued task can be
  // carrying the conversation a requeue handed back with it, along with that
  // conversation's artifacts, so the Queued lane is enriched like the other
  // two. ONE aggregated call returns every task's conversations plus each
  // task's primary-conversation transcript, replacing the old per-task serial
  // loop of 2–3 round-trips each (TFAC-98).
  const enrich = useCallback(
    async (tasks: Task[]) => {
      if (tasks.length === 0) return
      // The window is over CONVERSATIONS, ordered so a task's conversations stay
      // contiguous — so a board page's worth of tasks needs a page large
      // enough to hold all their conversations. The board reads the first page and
      // paints what it has; a card whose conversation fell past the window fills in on
      // the next refresh rather than blocking first paint on a second call.
      const agg = await apiJSON<{
        runs?: Record<string, Conversation[]>
        messages?: Record<string, Message[]>
      }>('/api/agent/conversations/list', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          task_ids: tasks.map((t) => t.id),
          include_messages: true,
          page_size: 200,
        }),
      })
      const conversationsByTask = agg.runs ?? {}
      const messagesByConversation = agg.messages ?? {}

      // Accumulate into plain objects, then commit each map in ONE setState
      // (the old loop fired up to three setState calls per task — a render per
      // iteration). Chains need a second per-chain fetch for their blueprint
      // step structure; resolve those concurrently rather than serially.
      const nextConversations: Record<string, Conversation> = {}
      const nextFeeds: Record<string, ConversationCardFeed> = {}
      const chainSeeds: Array<Promise<{ taskID: string; steps: Conversation[] } | null>> = []

      for (const task of tasks) {
        const taskConversations = conversationsByTask[task.id]
        if (!taskConversations || taskConversations.length === 0) continue
        const latestConversation = taskConversations[0]
        const blueprintRunID = latestConversation.blueprint_run_id
        if (blueprintRunID) {
          const stepConversations = taskConversations
            .filter((r) => r.blueprint_run_id === blueprintRunID)
            .sort((a, b) => (a.blueprint_step_index ?? 0) - (b.blueprint_step_index ?? 0))
          const activeStep =
            stepConversations.find((r) => isActiveStatus(r.Status)) ??
            stepConversations[stepConversations.length - 1]
          nextConversations[task.id] = activeStep
          // messagesByConversation is keyed by each task's PRIMARY (newest-started) conversation.
          // For a sequential chain the most recently started step IS the active
          // step, so activeStep.ID === taskConversations[0].ID and this lookup hits. If they
          // ever differ, the WS `message` handler seeds the active step
          // shortly — keep this keyed by activeStep.ID so it stays aligned.
          const msgs = messagesByConversation[activeStep.ID]
          if (msgs) nextFeeds[activeStep.ID] = feedFromMessages(msgs)
          chainSeeds.push(
            fetchChainStepConversations(blueprintRunID).then((steps) =>
              steps ? { taskID: task.id, steps } : null,
            ),
          )
        } else {
          nextConversations[task.id] = latestConversation
          const msgs = messagesByConversation[latestConversation.ID]
          if (msgs) nextFeeds[latestConversation.ID] = feedFromMessages(msgs)
        }
      }

      setConversations((prev) => ({ ...prev, ...nextConversations }))
      // A fetched feed replaces the incrementally-built one wholesale — the
      // server transcript is authoritative, so any WS/fetch race drift
      // self-corrects here.
      setConversationFeeds((prev) => ({ ...prev, ...nextFeeds }))

      // Apply all chain step rails in one batch once their blueprint fetches
      // settle (concurrent above), so the whole refresh costs ~3 renders, not
      // one per task.
      if (chainSeeds.length > 0) {
        const seeded = (await Promise.all(chainSeeds)).filter(
          (s): s is { taskID: string; steps: Conversation[] } => s !== null,
        )
        if (seeded.length > 0) {
          setChainStepConversations((prev) => {
            const next = { ...prev }
            for (const { taskID, steps } of seeded) next[taskID] = steps
            return next
          })
        }
      }
    },
    [fetchChainStepConversations],
  )

  // The list body for one lane: the lane itself (its statuses, the per-page
  // team scope, Done's seven-day window, the snoozed toggle) plus the
  // reader's own narrowing of it, every field of which the server applies.
  const laneBody = useCallback((col: ColumnId): TaskListRequest => {
    const tf = teamFilterRef.current
    const fields = laneFilterFields(filtersRef.current[col])
    switch (col) {
      case 'queued':
        return queuedLaneBody(tf, showSnoozedRef.current, fields)
      case 'in_progress':
        return statusListBody('in_progress', tf, fields)
      case 'done':
        return doneListBody(tf, fields)
    }
  }, [])

  const loaderFor = useCallback(
    (col: ColumnId) =>
      col === 'queued' ? loadQueued : col === 'in_progress' ? loadInProgress : loadDone,
    [loadQueued, loadInProgress, loadDone],
  )

  // The filter signature each lane was last fetched under, so a filter change
  // that asks the same question the lane already answered — the per-user
  // filters landing after mount, most often as the defaults — costs nothing.
  const fetchedRef = useRef<Record<ColumnId, string>>({ queued: '', in_progress: '', done: '' })

  // One lane's read: its page and, beside it, its facet. A lane that fails
  // keeps whatever it was showing rather than taking the others down with it
  // — the board is three independent reads, not one. The hook holds each
  // lane's items, so a failed refresh paints the previous page instead of
  // blanking the lane; a failed facet read keeps the previous chip set, since
  // the chips are a menu and a stale menu beats an empty one.
  const fetchLane = useCallback(
    async (col: ColumnId): Promise<Task[]> => {
      // The signature is taken with the body, not after the read: the
      // per-user filters can land while the mount read is in flight, and the
      // record has to say what was sent, not what is current.
      const signature = filterSignature(filtersRef.current[col])
      const body = laneBody(col)
      const [page] = await Promise.all([
        loaderFor(col)(body),
        apiJSON<TaskFacetsResponse>(TASK_FACETS_PATH, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(facetsBody(body)),
        })
          .then((res) => {
            const values = (res.event_types ?? []).map((f) => f.value).sort()
            setFacets((prev) => ({ ...prev, [col]: values }))
          })
          .catch(() => {}),
      ])
      fetchedRef.current[col] = signature
      return page?.items ?? []
    },
    [laneBody, loaderFor],
  )

  // Derive the three column lists from three filter sets over the tasks list
  // route. Done carries its seven-day window explicitly — the server applies
  // none of its own.
  const fetchTasks = useCallback(async () => {
    try {
      const [queuedItems, inProgressItems, doneItems] = await Promise.all(
        ALL_COLUMNS.map((col) => fetchLane(col)),
      )

      // Paint the board as soon as the three columns are in state. The agent-conversation
      // enrichment below fills cards progressively and must not hold the
      // spinner — it used to: setLoading sat in `finally` after the whole serial
      // loop, so first paint waited on every per-task round-trip (TFAC-98).
      setLoading(false)

      await enrich([...queuedItems, ...inProgressItems, ...doneItems])
    } catch {
      // Network error — keep stale data
    } finally {
      setLoading(false)
    }
  }, [fetchLane, enrich])

  // One lane's refresh, for a change that is that lane's alone: its filter,
  // or the Queued lane's snoozed toggle.
  const refetchLane = useCallback(
    async (col: ColumnId) => {
      try {
        await enrich(await fetchLane(col))
      } catch {
        // Network error — keep stale data
      }
    },
    [fetchLane, enrich],
  )

  useEffect(() => {
    fetchTasks()
  }, [fetchTasks])

  // A filter change is a NEW QUERY, so the lane it belongs to goes back to
  // its first page: the rows it was holding were the answer to a different
  // question. Debounced, because a search is typed; per lane, because the
  // other two lanes were not asked anything.
  const filterRefetchTimer = useRef<number | null>(null)
  useEffect(() => {
    filtersRef.current = filters
    // The mount fetch reads the ref itself; until it has run there is nothing
    // to compare against.
    if (loading) return
    const stale = ALL_COLUMNS.filter(
      (col) => filterSignature(filters[col]) !== fetchedRef.current[col],
    )
    if (stale.length === 0) return
    if (filterRefetchTimer.current != null) window.clearTimeout(filterRefetchTimer.current)
    filterRefetchTimer.current = window.setTimeout(() => {
      filterRefetchTimer.current = null
      for (const col of stale) void refetchLane(col)
    }, FILTER_DEBOUNCE_MS)
  }, [filters, loading, refetchLane])
  useEffect(
    () => () => {
      if (filterRefetchTimer.current != null) window.clearTimeout(filterRefetchTimer.current)
    },
    [],
  )

  // The snoozed toggle widens the Queued lane's own read, so it refreshes
  // that lane alone.
  const showSnoozedDidMount = useRef(false)
  useEffect(() => {
    showSnoozedRef.current = showSnoozed
    if (!showSnoozedDidMount.current) {
      showSnoozedDidMount.current = true
      return
    }
    void refetchLane('queued')
  }, [showSnoozed, refetchLane])

  // Debounced board refresh for websocket-driven refetches. A poll cycle or a
  // scoring pass lands as a burst of task_updated / scoring_completed events,
  // and each used to fire its own three-column refetch. Trailing debounce: each
  // event pushes the fetch out another FETCH_DEBOUNCE_MS so the whole burst
  // costs one round-trip after it ends — with a FETCH_MAX_WAIT_MS fence from
  // the first deferred event, so a sustained event stream can't starve the
  // refresh indefinitely. User-initiated mutations (drag, picker, delegate)
  // still call fetchTasks() directly so their repaint isn't delayed.
  const FETCH_DEBOUNCE_MS = 300
  const FETCH_MAX_WAIT_MS = 1500
  const fetchTasksRef = useRef(fetchTasks)
  useEffect(() => {
    fetchTasksRef.current = fetchTasks
  }, [fetchTasks])
  const fetchDebounceTimer = useRef<number | null>(null)
  const fetchDebounceDeadline = useRef(0)
  const scheduleFetchTasks = useCallback(() => {
    const now = Date.now()
    if (fetchDebounceTimer.current == null) {
      fetchDebounceDeadline.current = now + FETCH_MAX_WAIT_MS
    } else {
      window.clearTimeout(fetchDebounceTimer.current)
    }
    const delay = Math.min(FETCH_DEBOUNCE_MS, Math.max(0, fetchDebounceDeadline.current - now))
    fetchDebounceTimer.current = window.setTimeout(() => {
      fetchDebounceTimer.current = null
      fetchTasksRef.current()
    }, delay)
  }, [])
  useEffect(
    () => () => {
      if (fetchDebounceTimer.current != null) window.clearTimeout(fetchDebounceTimer.current)
    },
    [],
  )

  // Re-fetch all columns when the team filter changes — the fetchers read
  // the latest filter via its ref, so this picks up the new scope.
  const teamFilterDidMount = useRef(false)
  useEffect(() => {
    if (!teamFilterDidMount.current) {
      teamFilterDidMount.current = true
      return
    }
    fetchTasks()
  }, [teamFilter, fetchTasks])

  // WS listener — covers conversation_update (the existing path) and the
  // new task_updated / task_claimed events that fire from
  // every claim/status mutation. task_updated is the catch-all for
  // "this card may have moved columns; refetch."
  useWebSocket(
    useCallback(
      (event: WSEvent) => {
        if (event.type === 'conversation_update') {
          const conversationID = event.conversation_id
          const status = event.data.status ?? ''
          let matched = false
          setConversations((prev) => {
            const updated = { ...prev }
            for (const [taskId, conversation] of Object.entries(updated)) {
              if (conversation.ID === conversationID) {
                matched = true
                // failure_kind rides the failed-status event so the card's
                // kind-specific copy lands with the flip, not only after the
                // full-conversation refetch below settles.
                updated[taskId] = {
                  ...conversation,
                  Status: status,
                  ...(event.data.failure_kind ? { FailureKind: event.data.failure_kind } : {}),
                }
                break
              }
            }
            return updated
          })
          apiJSON<Conversation>(`/api/agent/conversations/${conversationID}`)
            .then((fullConversation) => {
              setConversations((p) => {
                const existing = p[fullConversation.TaskID]
                if (
                  existing &&
                  existing.ID !== fullConversation.ID &&
                  existing.StartedAt >= fullConversation.StartedAt
                ) {
                  return p
                }
                return { ...p, [fullConversation.TaskID]: fullConversation }
              })
              if (fullConversation.blueprint_run_id) {
                seedChainStepConversations(
                  fullConversation.TaskID,
                  fullConversation.blueprint_run_id,
                )
              }
            })
            .catch(() => {})

          // A few server paths still mutate task state but
          // only emit conversation_update (review/PR approval flips
          // task='done', then broadcasts the conversation completion). Without
          // a refetch here the card stays in its old column until a
          // manual refresh. Cheap to re-pull all three lanes — the
          // queries are indexed and short.
          if (isPermissionTerminalStatus(status)) {
            // The conversation is no longer running a turn, so any prompt parked on it is
            // stale — drop its queue so a finished card doesn't keep an
            // unanswerable Allow/Deny control.
            dropPermissionConversation(conversationID)
            scheduleFetchTasks()
          }

          if (!matched) {
            // Chain step conversation that isn't the active step: a new step
            // started or a prior one changed. Otherwise seed conversations
            // so auto-delegation / cross-tab / delegate responses we
            // haven't tracked yet render immediately.
            let isChainStep = false
            for (const steps of Object.values(chainStepConversationsRef.current)) {
              if (steps.some((r) => r.blueprint_run_id && r.ID === conversationID)) {
                isChainStep = true
                break
              }
            }
            if (isChainStep) {
              if (isTerminalStatus(status)) {
                scheduleFetchTasks()
              }
            } else {
              apiJSON<Conversation>(`/api/agent/conversations/${conversationID}`)
                .then((fullConversation) => {
                  setConversations((p) => ({ ...p, [fullConversation.TaskID]: fullConversation }))
                  if (fullConversation.blueprint_run_id) {
                    seedChainStepConversations(
                      fullConversation.TaskID,
                      fullConversation.blueprint_run_id,
                    )
                  }
                })
                .catch(() => {})
            }
          }
        } else if (event.type === 'artifact_updated') {
          // Reconciler (TFAC-464): an artifact this conversation produced changed state
          // on GitHub. The conversation's own status is unchanged — only its
          // artifact-derived surface (pending kind / approval card) — so refetch
          // the conversation, with NO optimistic Status write (unlike conversation_update).
          apiJSON<Conversation>(`/api/agent/conversations/${event.conversation_id}`)
            .then((fullConversation) => {
              setConversations((p) => {
                const existing = p[fullConversation.TaskID]
                if (
                  existing &&
                  existing.ID !== fullConversation.ID &&
                  existing.StartedAt >= fullConversation.StartedAt
                ) {
                  return p
                }
                return { ...p, [fullConversation.TaskID]: fullConversation }
              })
            })
            .catch(() => {})
        } else if (event.type === 'message') {
          // Live conversation-log tick. AgentCard renders from the bounded per-conversation feed
          // keyed by conversation ID; without this, new agent output only surfaces
          // after a status-change fetchTasks pass. Folding into the feed
          // (rather than appending to a full message array) keeps board state
          // bounded and the per-event work O(1). appendToFeed returns the same
          // reference for a display-no-op message (tool results, mostly) —
          // return prev in that case so React skips the board re-render.
          const conversationID = event.conversation_id
          if (!conversationID) return
          setConversationFeeds((prev) => {
            const cur = prev[conversationID]
            const next = appendToFeed(cur, event.data)
            if (next === (cur ?? EMPTY_FEED)) return prev
            return { ...prev, [conversationID]: next }
          })
        } else if (event.type === 'task_updated' || event.type === 'task_claimed') {
          // Any column-affecting change re-pulls the whole board. The three
          // lanes are cheap to refetch (each is a single indexed query) and
          // this avoids the per-column patch logic getting out of sync with
          // backend rules.
          scheduleFetchTasks()
        } else if (event.type === 'tasks_updated' || event.type === 'scoring_completed') {
          // scoring_completed: the scorer just landed priority_score
          // and ai_summary on a batch. Without this refetch the board
          // keeps stale ordering and missing summaries until another
          // unrelated event triggers fetchTasks. Cards.tsx has the
          // mirror — refetches on both scoring_started and
          // scoring_completed; we only need completed since the
          // priority_score it writes is what drives ordering here.
          scheduleFetchTasks()
        } else if (event.type === 'permission_request' || event.type === 'permission_resolved') {
          // A conversation raised an Allow/Deny prompt, or one it had was answered
          // (here or in another tab) / timed out. Either way the frame is only
          // a hint that this conversation's pending set moved — re-read it, so the card
          // lights up or clears from the server's view rather than from
          // whichever frames this tab happened to be around for.
          refreshPermissions(event.conversation_id)
        }
      },
      [
        scheduleFetchTasks,
        seedChainStepConversations,
        refreshPermissions,
        dropPermissionConversation,
      ],
    ),
  )

  // Cold-load reconstruction: a board opened after a prompt was raised never
  // saw its frame, so ask for each live conversation's pending set the first time that
  // conversation appears. Without this, a conversation parked on a human is indistinguishable
  // from a conversation that is simply working, until it times out.
  //
  // Bounded to non-terminal conversations (a finished conversation can't be waiting on anyone)
  // and asked once per conversation per mount, so a board that refetches its columns on
  // every event doesn't turn into a request per card per event.
  const permissionsAskedRef = useRef<Set<string>>(new Set())
  useEffect(() => {
    for (const conversation of Object.values(conversations)) {
      if (!conversation?.ID || isPermissionTerminalStatus(conversation.Status)) continue
      if (permissionsAskedRef.current.has(conversation.ID)) continue
      permissionsAskedRef.current.add(conversation.ID)
      refreshPermissions(conversation.ID)
    }
  }, [conversations, refreshPermissions])

  // Drop a conversation's queue when the conversation leaves the board (its conversations entry was
  // replaced — e.g. a chain advanced to a new step, or a task got re-delegated).
  // Keyed on conversations only (not the queue map) so a freshly-ingested prompt
  // isn't dropped in the window before its conversation's conversation_update seeds
  // conversations. queuesRef gives the latest queue keys without that dependency.
  useEffect(() => {
    const live = new Set(Object.values(conversations).map((r) => r.ID))
    for (const conversationID of Object.keys(queuesRef.current)) {
      if (!live.has(conversationID)) dropPermissionConversation(conversationID)
    }
  }, [conversations, dropPermissionConversation])

  // Each column's paging, as the column tail reads it. Rebuilt per render (the
  // lists change every load); the loader inside is stable.
  const paging: Record<ColumnId, LanePaging> = {
    queued: lanePaging(queuedList),
    in_progress: lanePaging(inProgressList),
    done: lanePaging(doneList),
  }

  const allTasks = useMemo(() => {
    const map = new Map<string, Task>()
    for (const t of [...queued, ...inProgress, ...done]) {
      map.set(t.id, t)
    }
    return map
  }, [queued, inProgress, done])

  // taskId → column lookup, built once per board state. getColumn is called
  // per dnd onDragOver event (which fires rapidly while dragging), so an O(1)
  // map beats re-scanning all three lists on every move.
  const columnByTask = useMemo<Map<string, ColumnId>>(() => {
    const m = new Map<string, ColumnId>()
    for (const t of queued) m.set(t.id, 'queued')
    for (const t of inProgress) m.set(t.id, 'in_progress')
    for (const t of done) m.set(t.id, 'done')
    return m
  }, [queued, inProgress, done])

  const getColumn = useCallback(
    (taskId: string): ColumnId | null => columnByTask.get(taskId) ?? null,
    [columnByTask],
  )

  // A status write on the task. Complete (drag-to-Done from In Progress)
  // keeps the card in Done; dismiss (drag-to-Done from Queued) closes it
  // unworked; in_progress is the human's one stage marker. Ending the task
  // force-resolves every unresolved artifact server-side and cancels a live
  // conversation; we only confirm first.
  const patchStatus = useCallback(
    async (taskId: string, status: 'done' | 'dismissed' | 'in_progress', failure: string) => {
      try {
        // A failure (e.g. a teardown that partially failed) must surface — a
        // silent fetchTasks would repaint as if the move succeeded.
        await apiFetch(`/api/tasks/${taskId}`, {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ status }),
        })
        fetchTasks()
      } catch (err) {
        toast.error(httpErrorMessage(err, failure))
      }
    },
    [fetchTasks],
  )

  const fireEnd = useCallback(
    (taskId: string, action: EndAction) =>
      action === 'complete'
        ? patchStatus(taskId, 'done', 'Could not mark the task done.')
        : patchStatus(taskId, 'dismissed', 'Could not dismiss the task.'),
    [patchStatus],
  )

  const fireRequeue = useCallback(
    async (taskId: string) => {
      try {
        await apiFetch(`/api/tasks/${taskId}/requeue`, { method: 'POST' })
        fetchTasks()
      } catch (err) {
        toast.error(httpErrorMessage(err, 'Could not return the task to the queue.'))
      }
    },
    [fetchTasks],
  )

  // requestRequeue is the one door every return-to-queue gesture takes — the
  // drop into Queued, and the picker's unassign. It asks only when a run is in
  // flight, because that is the one thing the gesture destroys: the requeue
  // stops it. With nothing running the move is reversible by re-claiming, so
  // it fires without a word. The artifacts are not in question either way —
  // a requeue hands them back to the queue with the task.
  const requestRequeue = useCallback(
    (taskId: string) => {
      const conversation = conversations[taskId]
      if (conversation && isLiveRun(conversation)) {
        setPendingRequeue({ taskId })
        return
      }
      void fireRequeue(taskId)
    },
    [conversations, fireRequeue],
  )

  // requestResolveAll gates a task-ending gesture behind the confirmation
  // modal when the task's conversation still has unresolved artifacts.
  // Returns true when it deferred to the modal (the caller must NOT fire its
  // own request); false when there's nothing to resolve and the caller should
  // proceed directly.
  //
  // Only the gestures that END a task reach it — ending a task is what
  // resolves what it holds. A queued task can be carrying a draft PR back in
  // the queue, so dismissing it is gated the same as completing.
  const requestResolveAll = useCallback(
    (taskId: string, action: EndAction): boolean => {
      const conversation = conversations[taskId]
      if (!conversation || !hasUnresolvedArtifacts(conversation)) return false
      const c = approvalCounts(conversation)
      setConfirmResolve({
        taskId,
        action,
        prCount: c.pr,
        reviewCount: c.review,
        isLive: isActiveConversation(conversation),
      })
      return true
    },
    [conversations],
  )

  // The board opens at the left (Queued first). Three lanes fit without a
  // forced scroll offset, so we start at the natural left edge.
  const scrollRef = useRef<HTMLDivElement>(null)

  // Dynamic edge-fade. The outermost walls — left of Queued, right of Done —
  // stay solid (there's nowhere further to scroll), and the fade ramps in on a
  // side only as content scrolls past it. `left`/`right` are 0→1 fractions of a
  // full FADE_PX fade; recomputed on scroll + resize.
  const FADE_PX = 40
  const [edgeFade, setEdgeFade] = useState({ left: 0, right: 1 })
  // Viewport width of the scrollport — feeds the centered-lane geometry below.
  const [containerW, setContainerW] = useState(0)
  const recomputeFade = useCallback(() => {
    const el = scrollRef.current
    if (!el) return
    setContainerW((w) => (w === el.clientWidth ? w : el.clientWidth))
    const maxScroll = el.scrollWidth - el.clientWidth
    const left = maxScroll <= 0 ? 0 : Math.min(el.scrollLeft / FADE_PX, 1)
    const right = maxScroll <= 0 ? 0 : Math.min((maxScroll - el.scrollLeft) / FADE_PX, 1)
    setEdgeFade((prev) => (prev.left === left && prev.right === right ? prev : { left, right }))
  }, [])
  useEffect(() => {
    if (loading) return
    recomputeFade()
    const el = scrollRef.current
    if (!el) return
    const onResize = () => recomputeFade()
    window.addEventListener('resize', onResize)
    const ro = new ResizeObserver(() => recomputeFade())
    ro.observe(el)
    return () => {
      window.removeEventListener('resize', onResize)
      ro.disconnect()
    }
  }, [loading, recomputeFade])

  const fadeMask = useMemo(() => {
    const l = FADE_PX * edgeFade.left
    const r = FADE_PX * edgeFade.right
    const grad = `linear-gradient(to right, rgba(0,0,0,${1 - edgeFade.left}) 0, #000 ${l}px, #000 calc(100% - ${r}px), rgba(0,0,0,${1 - edgeFade.right}) 100%)`
    return { maskImage: grad, WebkitMaskImage: grad }
  }, [edgeFade])

  // Centered-lane geometry. We want the viewport center to land on the midpoint
  // of the *open* (expanded) columns — one open → that column; two → their
  // midpoint; none → the center of the collapsed rails. Widths are fixed, so we
  // compute positions analytically rather than measuring the DOM. Symmetric
  // padding parks the strip so that midpoint sits dead-center when it fits, and
  // a matching scroll offset (applied below) handles the overflow case.
  const lane = useMemo(() => {
    let x = 0
    const centers: number[] = []
    for (const col of ALL_COLUMNS) {
      const w = collapsed[col] ? RAIL_W : COL_W
      if (!collapsed[col]) centers.push(x + w / 2)
      x += w + GAP
    }
    const stripW = x - GAP
    const centroid = centers.length
      ? centers.reduce((a, b) => a + b, 0) / centers.length
      : stripW / 2
    const half = containerW / 2
    const padLeft = Math.max(0, half - centroid)
    const padRight = Math.max(0, half - (stripW - centroid))
    return { padLeft, padRight, targetScroll: padLeft + centroid - half }
  }, [collapsed, containerW])

  // Park the scroll so the open-columns midpoint is centered (clamped to the
  // scrollable range for the overflow case). Re-runs on collapse + resize.
  useLayoutEffect(() => {
    const el = scrollRef.current
    if (!el || loading) return
    const max = el.scrollWidth - el.clientWidth
    el.scrollLeft = Math.max(0, Math.min(lane.targetScroll, max))
    recomputeFade()
  }, [lane, loading, recomputeFade])

  const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 5 } }))

  const handleDragStart = (event: DragStartEvent) => {
    setActiveId(String(event.active.id))
  }

  const handleDragOver = (event: DragOverEvent) => {
    // Resolve whichever column the cursor is over — a column id directly, or
    // the column owning the card under the cursor — and hand it to that column
    // so its border-trace fires even when you're hovering its cards.
    const overId = event.over?.id
    if (overId == null) {
      setActiveDropCol(null)
      return
    }
    const id = String(overId)
    setActiveDropCol(ALL_COLUMNS.includes(id as ColumnId) ? (id as ColumnId) : getColumn(id))
  }

  // Every card is draggable, whatever its run is doing: the drop is what the
  // gestures are, and each one either confirms first or is reversible. The
  // server is the boundary for a move it will not make — a refused write
  // surfaces as a toast and the next fetch reconciles — rather than a card
  // that silently snaps back.
  const handleDragEnd = async (event: DragEndEvent) => {
    const { active, over } = event
    setActiveId(null)
    setActiveDropCol(null)

    try {
      if (!over) return
      const taskId = String(active.id)
      const sourceCol = getColumn(taskId)
      const task = allTasks.get(taskId)
      if (!sourceCol || !task) return

      const overId = String(over.id)
      let targetCol: ColumnId
      if (ALL_COLUMNS.includes(overId as ColumnId)) {
        targetCol = overId as ColumnId
      } else {
        targetCol = getColumn(overId) || sourceCol
      }

      // Same column — no-op (we don't persist intra-column order).
      if (sourceCol === targetCol) return

      if (sourceCol === 'queued') {
        // Queued → Done: dismiss. Ending the task is what tears its
        // artifacts down, and a task can be carrying a draft PR back in the
        // queue, so the gate that guards completing guards this too.
        if (targetCol === 'done') {
          if (requestResolveAll(taskId, 'dismiss')) return
          await fireEnd(taskId, 'dismiss')
          return
        }
        // Queued → In Progress: the human's one stage marker. Claim first —
        // idempotent when the caller already holds it — then advance.
        await apiFetch(`/api/tasks/${taskId}/claim`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({}),
        })
        await patchStatus(taskId, 'in_progress', 'Could not start the task.')
        return
      }

      // Any → Queued: requeue. Clears the claim and resets status; asks first
      // only when a run is in flight, since that is what the move stops.
      if (targetCol === 'queued') {
        requestRequeue(taskId)
        return
      }

      // Any → Done: complete (preserves the card in Done; distinct from queued →
      // done which dismisses). Ending the task IS what resolves its artifacts,
      // so this gesture keeps the confirmation.
      if (targetCol === 'done') {
        if (requestResolveAll(taskId, 'complete')) return
        await fireEnd(taskId, 'complete')
        return
      }

      // Done → In Progress: the server decides whether a closed task can be
      // reopened; the card follows its answer.
      await patchStatus(taskId, 'in_progress', 'Could not move the task — please try again.')
    } catch (err) {
      // A failed status/claim/requeue mutation would otherwise leave the board
      // silently in the wrong state; surface it and let the next fetchTasks
      // reconcile.
      toast.error(httpErrorMessage(err, 'Could not move the task — please try again.'))
    }
  }

  // Assignee picker callbacks. The picker is the primary surface for
  // claim mutations in the board; drag is for column moves.
  const handlePickerClaim = useCallback(
    async (task: Task) => {
      try {
        await apiFetch(`/api/tasks/${task.id}/claim`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({}),
        })
      } catch (err) {
        toast.error(httpErrorMessage(err, 'Could not claim the task.'))
      }
      fetchTasks()
    },
    [fetchTasks],
  )

  const handlePickerUnclaim = useCallback(
    async (task: Task) => {
      // Unclaim = requeue (clears both claim cols + resets status to
      // queued). Same door the drag-to-Queue gesture takes, confirmed on
      // the same condition.
      requestRequeue(task.id)
    },
    [requestRequeue],
  )

  const handlePickerDelegate = useCallback((task: Task) => {
    pendingDelegateTask.current = task
    setShowPromptPicker(true)
  }, [])

  // Open a conversation's approval overlay (review or PR editor) by artifact id. Hoisted
  // to a stable callback (rather than an inline closure in the column JSX) so
  // the memoized cards' props don't churn on every board render.
  const handleOpenApproval = useCallback(
    (conversationID: string, kind: 'review' | 'pr', artifactId: string) => {
      setApprovalCtx({ conversationID, kind, artifactId })
    },
    [],
  )

  const handlePickerReassign = useCallback(
    async (task: Task, targetUserID: string) => {
      try {
        await apiFetch(`/api/tasks/${task.id}/claim`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ target_user_id: targetUserID }),
        })
      } catch (err) {
        toast.error(httpErrorMessage(err, 'Could not reassign the task.'))
      }
      fetchTasks()
    },
    [fetchTasks],
  )

  const handlePromptSelected = useCallback(
    async (promptId: string) => {
      setShowPromptPicker(false)
      const task = pendingDelegateTask.current
      if (!task) return
      pendingDelegateTask.current = null
      try {
        const body = await apiJSON<{ conversation_id?: string }>(`/api/tasks/${task.id}/delegate`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ blueprint_id: promptId }),
        })
        const conversationID = body.conversation_id
        if (conversationID) {
          setConversations((prev) => ({
            ...prev,
            // Optimistic row for the conversation the delegate call just
            // minted, standing in until the first WS status lands. `queued`
            // is what the backend's display ladder reports for a fresh
            // mint — nobody has claimed it yet — so the card shows its
            // queue wait instead of pretending an agent is already working.
            [task.id]: {
              ID: conversationID,
              TaskID: task.id,
              Status: 'queued',
              Model: '',
              StartedAt: new Date().toISOString(),
              ResultSummary: '',
            },
          }))
          setDelegateFailures((prev) => {
            if (!(task.id in prev)) return prev
            const next = { ...prev }
            delete next[task.id]
            return next
          })
        }
      } catch (err) {
        // Reason SPAWN_FAILED is the backend's "the claim stamped, only the
        // conversation didn't fire" marker — the task is in the bot's lane with no
        // conversation, so record the inline per-card failure the retry affordance
        // renders. Status alone can't be used here: pre-claim faults also
        // answer 500. Anything else means nothing landed; a toast is the
        // right surface. fetchTasks below reconciles either way.
        if (apiErrors(err).some((it) => it.reason === 'SPAWN_FAILED')) {
          setDelegateFailures((prev) => ({
            ...prev,
            [task.id]: httpErrorMessage(err, 'spawn failed'),
          }))
        } else {
          toast.error(httpErrorMessage(err, 'Could not delegate the task.'))
        }
      }
      fetchTasks()
    },
    [fetchTasks],
  )

  const activeTask = activeId ? allTasks.get(activeId) : null

  // Zero-team safe landing (TFAC-445) — a multi-mode user on no team has no
  // team-scoped tasks to show. Surface the friendly empty state instead of an
  // empty board (local mode always has its default team, so this is multi-only
  // in practice). Gated on teamsLoaded so it doesn't flash during cold load.
  if (teamsLoaded && teams.length === 0) {
    return (
      <>
        <GlassBackdrop />
        <ZeroTeamState canCreate={orgIsAdmin} />
      </>
    )
  }

  if (loading) {
    return (
      <>
        <GlassBackdrop />
        <div className="flex items-center justify-center min-h-[70vh]">
          <p className="text-body text-ink-3">Loading board…</p>
        </div>
      </>
    )
  }

  // Per-column header extras. (Queued's snoozed toggle now lives inside the
  // filter panel — see the `snooze` prop below — not as a header button.)
  const doneHeader = (
    <span
      className="text-label text-ink-3"
      title="Done column shows the last 7 days; older entries are hidden"
    >
      last 7 days
    </span>
  )

  return (
    <DndContext
      sensors={sensors}
      collisionDetection={closestCenter}
      onDragStart={handleDragStart}
      onDragOver={handleDragOver}
      onDragEnd={handleDragEnd}
      onDragCancel={() => {
        setActiveId(null)
        setActiveDropCol(null)
      }}
    >
      <GlassBackdrop />

      {/* Full-height stage. Pulled up under the nav (-mt) — this page wants the
          columns close to the chrome, not floating in a big top gap like the
          form pages do. The team filter takes what it needs; the scroll area
          fills the rest to the page bottom (hard floor on short screens). */}
      <div className="-mt-4 flex h-[calc(100%+1rem)] min-h-[26rem] flex-col">
        {/* Per-page team filter. Renders nothing at ≤1 team. */}
        {teams.length >= 2 && (
          <div className="flex items-center justify-end px-1 pb-3">
            <TeamScopeSelect value={teamFilter} onChange={setTeamFilter} />
          </div>
        )}

        {/* Horizontal-scroll container for the three columns. The strip is
            parked so the open-columns midpoint sits at the viewport center (see
            `lane` above) via dynamic left/right padding + a scroll offset. The
            dynamic mask dissolves columns into the page at whichever edge still
            has more to scroll. */}
        <div
          ref={scrollRef}
          onScroll={recomputeFade}
          className="min-h-0 flex-1 overflow-x-auto pb-3 pt-1"
          style={fadeMask}
        >
          <div
            className="flex h-full gap-6"
            style={{ paddingLeft: lane.padLeft, paddingRight: lane.padRight }}
          >
            {ALL_COLUMNS.map((colId, i) =>
              collapsed[colId] ? (
                <CollapsedColumn
                  key={colId}
                  index={i}
                  title={COLUMN_TITLES[colId]}
                  // The rail's number is the lane under its current query —
                  // the same total the open lane's tail counts against — not
                  // how much of it happens to be fetched.
                  count={paging[colId].total ?? paging[colId].shown}
                  onExpand={() => toggleCollapse(colId)}
                />
              ) : (
                <BoardColumn
                  key={colId}
                  id={colId}
                  index={i}
                  dragOver={activeDropCol === colId}
                  title={COLUMN_TITLES[colId]}
                  filter={filters[colId]}
                  onFilterChange={(next) => setFilters((prev) => ({ ...prev, [colId]: next }))}
                  eventTypes={facets[colId]}
                  paging={paging[colId]}
                  onCollapse={() => toggleCollapse(colId)}
                  headerExtra={colId === 'done' ? doneHeader : undefined}
                  snooze={
                    colId === 'queued'
                      ? { shown: showSnoozed, onToggle: () => setShowSnoozed((v) => !v) }
                      : undefined
                  }
                >
                  <ColumnContents
                    colId={colId}
                    tasks={lists[colId].items}
                    narrowed={narrows(filters[colId])}
                    conversations={conversations}
                    conversationFeeds={conversationFeeds}
                    chainStepConversations={chainStepConversations}
                    permQueues={permQueueMap}
                    onResolvePermission={resolvePermission}
                    currentUserID={currentUserID}
                    members={members}
                    bot={bot}
                    delegateFailures={delegateFailures}
                    onPickerClaim={handlePickerClaim}
                    onPickerUnclaim={handlePickerUnclaim}
                    onPickerDelegate={handlePickerDelegate}
                    onPickerReassign={handlePickerReassign}
                    onOpenApproval={handleOpenApproval}
                    onArtifactResolved={fetchTasks}
                    onRetry={handlePickerDelegate}
                  />
                </BoardColumn>
              ),
            )}
          </div>
        </div>
      </div>

      <DragOverlay dropAnimation={null}>
        {activeTask && (
          <div className="w-[400px]">
            <TaskCard task={activeTask} isDragging />
          </div>
        )}
      </DragOverlay>

      <PromptPicker
        open={showPromptPicker}
        source="blueprints"
        title="Choose a blueprint"
        subtitle="Select a blueprint to run for this task"
        selectLabel="Run"
        onSelect={handlePromptSelected}
        onClose={() => {
          setShowPromptPicker(false)
          pendingDelegateTask.current = null
        }}
        onEditPrompts={() => {
          setShowPromptPicker(false)
          pendingDelegateTask.current = null
          window.location.href = '/prompts'
        }}
      />

      <ReviewOverlay
        artifactId={approvalCtx?.kind === 'review' ? approvalCtx.artifactId : ''}
        open={approvalCtx?.kind === 'review'}
        onClose={() => {
          setApprovalCtx(null)
          fetchTasks()
        }}
      />
      <PendingPROverlay
        artifactId={approvalCtx?.kind === 'pr' ? approvalCtx.artifactId : ''}
        open={approvalCtx?.kind === 'pr'}
        onClose={() => {
          setApprovalCtx(null)
          fetchTasks()
        }}
      />

      <ResolveAllConfirm
        open={confirmResolve !== null}
        prCount={confirmResolve?.prCount ?? 0}
        reviewCount={confirmResolve?.reviewCount ?? 0}
        isLive={confirmResolve?.isLive ?? false}
        actionLabel={confirmResolve?.action === 'dismiss' ? 'Dismiss' : 'Mark done'}
        busy={confirmBusy}
        onConfirm={async () => {
          if (!confirmResolve) return
          setConfirmBusy(true)
          try {
            await fireEnd(confirmResolve.taskId, confirmResolve.action)
            setConfirmResolve(null)
          } finally {
            setConfirmBusy(false)
          }
        }}
        onCancel={() => setConfirmResolve(null)}
      />

      <RequeueConfirm
        open={pendingRequeue !== null}
        onConfirm={() => {
          if (!pendingRequeue) return
          const { taskId } = pendingRequeue
          setPendingRequeue(null)
          void fireRequeue(taskId)
        }}
        onCancel={() => setPendingRequeue(null)}
      />
    </DndContext>
  )
}

// lanePaging is the column tail's view of a paged list: what is held, what the
// query matches, and the ask for the next page.
function lanePaging(list: PagedList<Task>): LanePaging {
  return {
    shown: list.items.length,
    total: list.total,
    hasMore: list.hasMore,
    loading: list.loading,
    onNearEnd: list.loadMore,
  }
}

// PickerProps is the assignee-picker wiring shared by both card wrappers —
// the roster data plus the four claim-mutation callbacks (all useCallback-
// stable in Board), threaded down so the wrappers can build the AssigneePicker
// element themselves, below their memo boundary.
interface PickerProps {
  currentUserID: string
  members: TeamMember[]
  bot: TeamBot | null
  pickerReadOnly: boolean
  onPickerClaim: (task: Task) => Promise<void>
  onPickerUnclaim: (task: Task) => Promise<void>
  onPickerDelegate: (task: Task) => void
  onPickerReassign: (task: Task, targetUserID: string) => Promise<void>
}

// ColumnContents is the per-column body — handles empty state, the
// SortableContext, and the per-task card-vs-agentcard branching. The card
// wrappers it renders are memoized with identity-stable props (per-conversation slices
// of the board maps + useCallback'd handlers, with the per-card closures and
// the AssigneePicker element built inside the wrapper, below its memo
// boundary), so a live-feed tick for one conversation re-renders that conversation's card only —
// not every card on the board.
function ColumnContents({
  colId,
  tasks,
  narrowed,
  conversations,
  conversationFeeds,
  chainStepConversations,
  permQueues,
  onResolvePermission,
  currentUserID,
  members,
  bot,
  delegateFailures,
  onPickerClaim,
  onPickerUnclaim,
  onPickerDelegate,
  onPickerReassign,
  onOpenApproval,
  onArtifactResolved,
  onRetry,
}: {
  colId: ColumnId
  tasks: Task[]
  // True when the reader has narrowed the lane — an empty lane then has no
  // matches, which is not the same as being empty.
  narrowed: boolean
  conversations: Record<string, Conversation>
  conversationFeeds: Record<string, ConversationCardFeed>
  chainStepConversations: Record<string, Conversation[]>
  permQueues: Record<string, PendingPermission[]>
  onResolvePermission: (
    conversationID: string,
    toolCallID: string,
    decision: PermissionDecisionInput,
  ) => Promise<void>
  delegateFailures: Record<string, string>
  onOpenApproval: (conversationID: string, kind: 'review' | 'pr', artifactId: string) => void
  onArtifactResolved: () => void
  onRetry: (task: Task) => void
} & Omit<PickerProps, 'pickerReadOnly'>) {
  if (tasks.length === 0) {
    return <EmptyColumn>{narrowed ? 'No matches' : emptyLabelFor(colId)}</EmptyColumn>
  }

  const picker: PickerProps = {
    currentUserID,
    members,
    bot,
    pickerReadOnly: colId === 'done',
    onPickerClaim,
    onPickerUnclaim,
    onPickerDelegate,
    onPickerReassign,
  }

  return (
    <SortableContext items={tasks.map((t) => t.id)} strategy={verticalListSortingStrategy}>
      {tasks.map((task) => {
        // A queued task renders as a task, not as the run it may still be
        // carrying: nobody is working on it, and the agent card's whole
        // vocabulary — elapsed, a live feed, a cancel — would say someone is.
        // Its conversation is still enriched, since the artifacts a requeue
        // handed back with it are the task's now.
        const conversation = colId === 'queued' ? undefined : conversations[task.id]
        if (conversation) {
          return (
            <SortableAgentCard
              key={task.id}
              task={task}
              conversation={conversation}
              chainSteps={chainStepConversations[task.id]}
              feed={conversationFeeds[conversation.ID]}
              pendingPermissions={permQueues[conversation.ID]}
              onResolvePermission={onResolvePermission}
              onOpenApproval={onOpenApproval}
              onArtifactResolved={onArtifactResolved}
              {...picker}
            />
          )
        }
        return (
          <SortableTaskCard
            key={task.id}
            task={task}
            delegateFailure={delegateFailures[task.id]}
            onRetry={onRetry}
            {...picker}
          />
        )
      })}
    </SortableContext>
  )
}

function emptyLabelFor(colId: ColumnId): string {
  switch (colId) {
    case 'queued':
      return 'Queue is empty'
    case 'in_progress':
      return 'Nothing in progress'
    case 'done':
      return 'Nothing done in last 7 days'
  }
}

// CardAssigneePicker builds the per-card AssigneePicker element from the
// shared PickerProps bundle — inside the memoized wrappers, so the element is
// only recreated when the wrapper itself re-renders.
function CardAssigneePicker({ task, picker }: { task: Task; picker: PickerProps }) {
  return (
    <AssigneePicker
      task={task}
      currentUserID={picker.currentUserID}
      members={picker.members}
      bot={picker.bot}
      onClaim={picker.onPickerClaim}
      onUnclaim={picker.onPickerUnclaim}
      onDelegate={picker.onPickerDelegate}
      onReassign={picker.onPickerReassign}
      readOnly={picker.pickerReadOnly}
    />
  )
}

const SortableTaskCard = memo(function SortableTaskCard({
  task,
  delegateFailure,
  onRetry,
  ...picker
}: {
  task: Task
  delegateFailure?: string
  onRetry: (task: Task) => void
} & PickerProps) {
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({
    id: task.id,
  })
  const style: React.CSSProperties = {
    transform: CSS.Transform.toString(transform),
    transition,
    opacity: isDragging ? 0.3 : 1,
  }
  // assigneeSlot is forwarded into TaskCard's header instead
  // of overlaid via absolute positioning — the prior approach
  // collided with the bottom-row affordances on tall cards and with
  // AgentCard's elapsed-time / expand cluster after a conversation completes.
  // Card owns its own layout; we just hand it the slot to render.
  return (
    <TaskCard
      ref={setNodeRef}
      task={task}
      style={style}
      // Deliberately false: the lifted "ghost" is rendered by DragOverlay, and
      // the in-place card fades via style.opacity (set in useSortable above).
      // isDragging only drives a redundant z-50 here, so we never forward it.
      isDragging={false}
      delegateFailed={delegateFailure ? { message: delegateFailure } : undefined}
      onRetry={delegateFailure ? () => onRetry(task) : undefined}
      assigneeSlot={<CardAssigneePicker task={task} picker={picker} />}
      {...attributes}
      {...listeners}
    />
  )
})

const SortableAgentCard = memo(function SortableAgentCard({
  task,
  conversation,
  chainSteps,
  feed,
  pendingPermissions,
  onResolvePermission,
  onOpenApproval,
  onArtifactResolved,
  ...picker
}: {
  task: Task
  conversation: Conversation
  chainSteps?: Conversation[]
  feed?: ConversationCardFeed
  pendingPermissions?: PendingPermission[]
  onResolvePermission: (
    conversationID: string,
    toolCallID: string,
    decision: PermissionDecisionInput,
  ) => Promise<void>
  onOpenApproval: (conversationID: string, kind: 'review' | 'pr', artifactId: string) => void
  onArtifactResolved: () => void
} & PickerProps) {
  // Draggable whatever the run is doing — a working run's drop into Queued
  // asks before it stops the run, and every other drop is reversible or
  // gated, so there is no state where the grab has to be refused.
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({
    id: task.id,
  })
  const style: React.CSSProperties = {
    transform: CSS.Transform.toString(transform),
    transition,
    opacity: isDragging ? 0.3 : 1,
    cursor: 'grab',
  }
  // assigneeSlot forwarded into AgentCard's header cluster
  // so it shares the gap-2 spacing with elapsed/expand/cancel
  // instead of overlapping them via absolute positioning. Same
  // reasoning as the TaskCard wrapper above.
  return (
    <div ref={setNodeRef} style={style} {...attributes} {...listeners}>
      <AgentCard
        task={task}
        conversation={conversation}
        chainSteps={chainSteps}
        feed={feed}
        pendingPermissions={pendingPermissions}
        onResolvePermission={(toolCallID, decision) =>
          onResolvePermission(conversation.ID, toolCallID, decision)
        }
        onOpenArtifact={(kind, artifactId) => onOpenApproval(conversation.ID, kind, artifactId)}
        onArtifactResolved={onArtifactResolved}
        assigneeSlot={<CardAssigneePicker task={task} picker={picker} />}
      />
    </div>
  )
})

function EmptyColumn({ children }: { children: React.ReactNode }) {
  return <p className="text-ui text-ink-3 text-center py-12">{children}</p>
}
