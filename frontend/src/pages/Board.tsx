import { memo, useState, useEffect, useLayoutEffect, useCallback, useMemo, useRef } from 'react'
import { TERMINAL_CONVERSATION_STATUSES } from '../types'
import type {
  Task,
  Conversation,
  Message,
  WSEvent,
  TeamMembersResponse,
  TeamMember,
  TeamBot,
} from '../types'
import { useWebSocket, setPresenceView } from '../hooks/useWebSocket'
import { usePermissionQueues } from '../hooks/usePermissionQueues'
import {
  isActiveConversation,
  isActiveStatus,
  isFailedStatus,
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
import { usePagedList } from '../hooks/usePagedList'
import type { PagedList } from '../hooks/usePagedList'
import { TASK_LIST_PATH, doneListBody, queueListBody, statusListBody } from '../lib/taskList'
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
import { toast } from '../components/Toast/toastStore'
import { apiErrors, apiFetch, apiJSON, httpErrorMessage } from '../lib/apiClient'
import BoardColumn, { CollapsedColumn } from '../components/board/BoardColumn'
import {
  applyColumnFilter,
  emptyFilter,
  type ColumnFilterState,
} from '../components/board/columnFilter'
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

// Five columns on the board. Default view scrolls so Claimed
// is leftmost-visible; user scrolls left for Queued, right for Done.
// Column ids double as drop targets — keep them lowercase + stable
// since they're persisted in localStorage filter keys.
type ColumnId = 'queued' | 'claimed' | 'in_progress' | 'in_review' | 'done'

const ALL_COLUMNS: ColumnId[] = ['queued', 'claimed', 'in_progress', 'in_review', 'done']

const COLUMN_TITLES: Record<ColumnId, string> = {
  queued: 'Queued',
  claimed: 'Claimed',
  in_progress: 'In Progress',
  in_review: 'In Review',
  done: 'Done',
}

// Lane geometry, used to center the strip on the midpoint of the *open*
// columns. COL_W must match the expanded BoardColumn width (w-[430px]); RAIL_W
// the collapsed CollapsedColumn (w-5 = 20px); GAP the row's gap-6 (24px).
const COL_W = 430
const RAIL_W = 20
const GAP = 24

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

type FilterMap = Record<ColumnId, ColumnFilterState>

function loadFilters(userID: string): FilterMap {
  try {
    const raw = localStorage.getItem(filterStorageKey(userID))
    if (!raw) return defaultFilters()
    const parsed = JSON.parse(raw) as Partial<FilterMap>
    const out = defaultFilters()
    for (const col of ALL_COLUMNS) {
      if (parsed[col]) {
        out[col] = { ...emptyFilter, ...parsed[col] }
      }
    }
    return out
  } catch {
    return defaultFilters()
  }
}

function defaultFilters(): FilterMap {
  return {
    queued: { ...emptyFilter },
    claimed: { ...emptyFilter },
    in_progress: { ...emptyFilter },
    in_review: { ...emptyFilter },
    done: { ...emptyFilter },
  }
}

// Collapse persistence: same per-user localStorage scheme as filters. Claimed
// and In Review start collapsed — a fresh board greets you with Queued / In
// Progress / Done (the lanes that hold work) plus two thin rails, so five lanes
// fit without shrinking anything. (A per-user backend UI-prefs store is the
// eventual home; localStorage holds the line until then.)
type CollapseMap = Record<ColumnId, boolean>

const COLLAPSE_STORAGE_PREFIX = 'sky330.board.collapsed.v1'

function collapseStorageKey(userID: string): string {
  return `${COLLAPSE_STORAGE_PREFIX}.${userID || 'anon'}`
}

function defaultCollapsed(): CollapseMap {
  return { queued: false, claimed: true, in_progress: false, in_review: true, done: false }
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

export default function Board() {
  // One paged list per column — five filter sets over the one tasks list
  // route. Bot/user auto-routing keeps them disjoint at the backend, so a task
  // only appears in one column. Each holds its own page token, so a deep
  // column pages independently of the others.
  const queuedList = usePagedList<Task>(TASK_LIST_PATH, 'Could not load the queue.')
  const claimedList = usePagedList<Task>(TASK_LIST_PATH, 'Could not load the claimed tasks.')
  const inProgressList = usePagedList<Task>(TASK_LIST_PATH, 'Could not load the in-progress tasks.')
  const inReviewList = usePagedList<Task>(TASK_LIST_PATH, 'Could not load the in-review tasks.')
  const doneList = usePagedList<Task>(TASK_LIST_PATH, 'Could not load the finished tasks.')
  const queued = queuedList.items
  const claimed = claimedList.items
  const inProgress = inProgressList.items
  const inReview = inReviewList.items
  const done = doneList.items
  // The loaders are stable across renders; the list objects around them are
  // not. fetchTasks closes over these rather than the lists so its own
  // identity stays stable — the WS refresh and the mount effect both key off
  // it, and a per-render identity would refetch the board on every render.
  const loadQueued = queuedList.load
  const loadClaimed = claimedList.load
  const loadInProgress = inProgressList.load
  const loadInReview = inReviewList.load
  const loadDone = doneList.load
  const [loading, setLoading] = useState(true)

  // Presence (TFAC-392): the board is an answer-capable surface for permission
  // prompts (it renders + answers them inline), so report it while mounted and
  // fall back to 'other' on unmount so an unattended run fast-denies once the
  // operator leaves the board.
  useEffect(() => {
    setPresenceView('board')
    return () => setPresenceView('other')
  }, [])

  // Agent run state — runs render on cards regardless of which column
  // the card is in (a user-claimed in_progress task can also have a
  // run; a bot-claimed in_review task definitely has one).
  const [conversations, setConversations] = useState<Record<string, Conversation>>({})
  // Per-run card feed (running stats + last few ticker lines), keyed by run
  // ID. Bounded by construction — the board used to accumulate every run's
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

  // Tool-permission prompts for every run on the board, keyed by run ID. The
  // shared queue core (same one useConversationDetail uses) owns dedup + per-prompt TTL
  // timers; the board ingests `permission_request` and drops a run's queue when
  // it finishes or leaves the board. A queuesRef mirrors the map so the
  // leave-the-board reconciliation effect can read it without re-running on
  // every ingest (which would race a just-ingested prompt against its
  // not-yet-seeded run).
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

  // Team roster for the assignee picker. Includes bot when enabled.
  const [members, setMembers] = useState<TeamMember[]>([])
  const [bot, setBot] = useState<TeamBot | null>(null)
  const [currentUserID, setCurrentUserID] = useState<string>('')

  // multi-team. teamFilter is the per-page read scope (threaded
  // into all five column fetches as team_id); `teams` backs the row
  // color-coding. Both render their UI only at ≥2 teams. teamFilterRef
  // keeps fetchTasks's identity stable while always reading the latest.
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
  // Tracks bot-claimed tasks where the delegate run failed
  // to fire. Cleared when a run for the task lands.
  const [delegateFailures, setDelegateFailures] = useState<Record<string, string>>({})

  // Per-item approval overlay (open one unresolved artifact's editor).
  const [approvalCtx, setApprovalCtx] = useState<{
    conversationID: string
    kind: 'review' | 'pr'
    artifactId: string
  } | null>(null)

  // Resolve-all confirmation (TFAC-384 §4): drag-to-Done (complete) and
  // Return-to-queue (requeue) force-resolve every unresolved artifact and cancel
  // a live run. When the target task has unresolved artifacts we stash the
  // intended action here and gate the request behind the confirmation modal.
  const [confirmResolve, setConfirmResolve] = useState<{
    taskId: string
    prCount: number
    reviewCount: number
    isLive: boolean
    action: 'complete' | 'requeue'
  } | null>(null)
  const [confirmBusy, setConfirmBusy] = useState(false)

  // Fetches a blueprint run's step structure and pads it into a length-N
  // array of step runs — synthetic 'pending' placeholders for steps without
  // a run yet — the shape the chain rail renders. Pure: returns the array
  // (null on error / empty) and writes no state, so fetchTasks can resolve
  // many chains in parallel and apply them in one batched setChainStepConversations.
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
          // A step with no run yet. Its status is empty rather than a
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
  // time); fetchTasks batches via fetchChainStepConversations directly.
  const seedChainStepConversations = useCallback(
    async (taskID: string, blueprintRunID: string) => {
      const steps = await fetchChainStepConversations(blueprintRunID)
      if (steps) setChainStepConversations((prev) => ({ ...prev, [taskID]: steps }))
    },
    [fetchChainStepConversations],
  )

  // Fetch members + bot once at mount. The picker degrades gracefully
  // if this fails (members=[], bot=null → only the avatar renders,
  // picker dropdown is empty).
  useEffect(() => {
    void (async () => {
      try {
        // Either read failing leaves the picker degraded but the board
        // working, so both fall to null rather than rejecting the pair.
        const [meRes, rosterRes] = await Promise.all([
          apiJSON<unknown>('/api/me').catch(() => null),
          apiJSON<unknown>('/api/team/members').catch(() => null),
        ])
        // /api/me returns the caller's identity as `id`, not `user_id`
        // (see auth_handlers.go:514). The picker uses currentUserID to
        // recognize "claimed by me" — without this read, every click on
        // Me would take the claim path instead of toggling unclaim.
        if (meRes && typeof meRes === 'object' && 'id' in meRes) {
          setCurrentUserID((meRes as { id: string }).id ?? '')
        }
        if (rosterRes) {
          const roster = rosterRes as TeamMembersResponse
          setMembers(roster.members ?? [])
          setBot(roster.bot ?? null)
        }
      } catch {
        // Picker degrades; board still works.
      }
    })()
  }, [])

  // Derive the five column lists from five filter sets over the tasks list
  // route. The Queued column is the queue projection (unclaimed, awake); the
  // others are their own lane. Done carries its seven-day window explicitly —
  // the server applies none of its own.
  const fetchTasks = useCallback(async () => {
    try {
      // Thread the per-page team filter into every column
      // fetch. Empty = the union of the viewer's teams (the default).
      const tf = teamFilterRef.current
      // A column that fails keeps whatever it was showing rather than taking
      // the other four down with it — the board is five independent reads,
      // not one. The hook holds each column's items, so a failed refresh
      // paints the previous page instead of blanking the lane.
      //
      // The queued page is loaded but not read here: an unclaimed task has no
      // run, so it skips the enrichment pass below.
      const [, claimedRes, inProgressRes, inReviewRes, doneRes] = await Promise.all([
        loadQueued(queueListBody(tf, showSnoozed)),
        // "claimed" is the claim axis, not a lifecycle status: a queued task
        // someone (or the bot) has taken.
        loadClaimed(statusListBody('claimed', tf)),
        loadInProgress(statusListBody('in_progress', tf)),
        loadInReview(statusListBody('in_review', tf)),
        loadDone(doneListBody(tf)),
      ])

      // Paint the board as soon as the five columns are in state. The agent-run
      // enrichment below fills cards progressively and must not hold the
      // spinner — it used to: setLoading sat in `finally` after the whole serial
      // loop, so first paint waited on every per-task round-trip (TFAC-98).
      setLoading(false)

      // Agent runs for any task that might carry one — claimed, in_progress,
      // in_review, done can all have runs attached. Queued never does (claim
      // cleared = no active run). ONE aggregated call returns every task's runs
      // plus each task's primary-run transcript, replacing the old per-task
      // serial loop of 2–3 round-trips each (TFAC-98).
      const withConversations: Task[] = [
        ...(claimedRes?.items ?? []),
        ...(inProgressRes?.items ?? []),
        ...(inReviewRes?.items ?? []),
        ...(doneRes?.items ?? []),
      ]
      if (withConversations.length === 0) return
      // The window is over CONVERSATIONS, ordered so a task's runs stay
      // contiguous — so a board page's worth of tasks needs a page large
      // enough to hold all their runs. The board reads the first page and
      // paints what it has; a card whose run fell past the window fills in on
      // the next refresh rather than blocking first paint on a second call.
      const agg = await apiJSON<{
        runs?: Record<string, Conversation[]>
        messages?: Record<string, Message[]>
      }>('/api/agent/conversations/list', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          task_ids: withConversations.map((t) => t.id),
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

      for (const task of withConversations) {
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
          // messagesByConversation is keyed by each task's PRIMARY (newest-started) run.
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
    } catch {
      // Network error — keep stale data
    } finally {
      setLoading(false)
    }
  }, [
    fetchChainStepConversations,
    showSnoozed,
    loadQueued,
    loadClaimed,
    loadInProgress,
    loadInReview,
    loadDone,
  ])

  useEffect(() => {
    fetchTasks()
  }, [fetchTasks])

  // Debounced board refresh for websocket-driven refetches. A poll cycle or a
  // scoring pass lands as a burst of task_updated / scoring_completed events,
  // and each used to fire its own five-column refetch. Trailing debounce: each
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

  // Re-fetch all columns when the team filter changes — fetchTasks reads
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
        // conversation_update carries a conversation_id for a delegated run and
        // a project_id (no conversation_id) for a curator turn — the board only
        // tracks delegated runs, so it ignores the curator variant.
        if (event.type === 'conversation_update' && event.conversation_id) {
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
                // full-run refetch below settles.
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
          // task='done', then broadcasts the run completion). Without
          // a refetch here the card stays in its old column until a
          // manual refresh. Cheap to re-pull all five buckets — the
          // queries are indexed and short.
          if (isPermissionTerminalStatus(status)) {
            // The run is no longer running a turn, so any prompt parked on it is
            // stale — drop its queue so a finished card doesn't keep an
            // unanswerable Allow/Deny control.
            dropPermissionConversation(conversationID)
            scheduleFetchTasks()
          }

          if (!matched) {
            // Chain step run that isn't the active step: a new step
            // started or a prior one changed. Otherwise seed conversations
            // so auto-delegation / cross-tab / swipe responses we
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
          // Reconciler (TFAC-464): an artifact this run produced changed state
          // on GitHub. The run's own status is unchanged — only its
          // artifact-derived surface (pending kind / approval card) — so refetch
          // the run, with NO optimistic Status write (unlike conversation_update).
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
        } else if (event.type === 'message' && event.conversation_id) {
          // Live run-log tick. AgentCard renders from the bounded per-run feed
          // keyed by run.ID; without this, new agent output only surfaces
          // after a status-change fetchTasks pass. Folding into the feed
          // (rather than appending to a full message array) keeps board state
          // bounded and the per-event work O(1). appendToFeed returns the same
          // reference for a display-no-op message (tool results, mostly) —
          // return prev in that case so React skips the board re-render. A
          // curator `message` (project_id, no conversation_id) is ignored here.
          const conversationID = event.conversation_id
          setConversationFeeds((prev) => {
            const cur = prev[conversationID]
            const next = appendToFeed(cur, event.data)
            if (next === (cur ?? EMPTY_FEED)) return prev
            return { ...prev, [conversationID]: next }
          })
        } else if (event.type === 'task_updated' || event.type === 'task_claimed') {
          // Any column-affecting change re-pulls the whole
          // board. The 5-column buckets are cheap to refetch (each is
          // a single indexed query) and this avoids the per-column
          // patch logic getting out of sync with backend rules.
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
          // A run raised an Allow/Deny prompt, or one it had was answered
          // (here or in another tab) / timed out. Either way the frame is only
          // a hint that this run's pending set moved — re-read it, so the card
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
  // saw its frame, so ask for each live run's pending set the first time that
  // run appears. Without this, a run parked on a human is indistinguishable
  // from a run that is simply working, until it times out.
  //
  // Bounded to non-terminal runs (a finished run can't be waiting on anyone)
  // and asked once per run per mount, so a board that refetches its columns on
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

  // Drop a run's queue when the run leaves the board (its conversations entry was
  // replaced — e.g. a chain advanced to a new step, or a task got re-delegated).
  // Keyed on conversations only (not the queue map) so a freshly-ingested prompt
  // isn't dropped in the window before its run's conversation_update seeds
  // conversations. queuesRef gives the latest queue keys without that dependency.
  useEffect(() => {
    const live = new Set(Object.values(conversations).map((r) => r.ID))
    for (const conversationID of Object.keys(queuesRef.current)) {
      if (!live.has(conversationID)) dropPermissionConversation(conversationID)
    }
  }, [conversations, dropPermissionConversation])

  // Sort tasks with active runs in a meaningful order. Used for
  // In Progress and In Review where the run state matters for
  // attention. Needs-you (a parked permission prompt or an unresolved
  // artifact) > failed > everything in flight > completed.
  const sortByConversationAttention = useCallback(
    (tasks: Task[]) => {
      const weight = (t: Task) => {
        const conversation = conversations[t.id]
        if (!conversation) return 2
        // A live tool-permission prompt or an unresolved artifact set is the most
        // urgent "needs you" — top weight — so it can't be missed in the column.
        if ((permQueueMap[conversation.ID]?.length ?? 0) > 0) return 0
        if (hasUnresolvedArtifacts(conversation)) return 0
        if (isFailedStatus(conversation.Status)) return 1
        if (conversation.Status === 'completed') return 3
        return 2
      }
      return [...tasks].sort((a, b) => weight(a) - weight(b))
    },
    [conversations, permQueueMap],
  )

  // Resolve a task's claimee to a display name for the claimee sort. Agent
  // claims read the bot's name; user claims read the roster (with "You" for the
  // viewer); unclaimed returns '' (sorts last).
  const resolveClaimee = useCallback(
    (t: Task): string => {
      if (t.claimed_by_agent_id) return bot?.display_name ?? 'Agent'
      if (t.claimed_by_user_id) {
        const m = members.find((mm) => mm.user_id === t.claimed_by_user_id)
        if (m) return m.is_current_user ? 'You' : m.display_name
        return t.claimed_by_user_id === currentUserID ? 'You' : 'User'
      }
      return ''
    },
    [members, bot, currentUserID],
  )

  const filtered = useMemo<Record<ColumnId, Task[]>>(() => {
    const opts = { resolveClaimee }
    return {
      queued: applyColumnFilter(queued, filters.queued, opts),
      claimed: applyColumnFilter(claimed, filters.claimed, opts),
      in_progress: applyColumnFilter(
        sortByConversationAttention(inProgress),
        filters.in_progress,
        opts,
      ),
      in_review: applyColumnFilter(sortByConversationAttention(inReview), filters.in_review, opts),
      done: applyColumnFilter(done, filters.done, opts),
    }
  }, [
    queued,
    claimed,
    inProgress,
    inReview,
    done,
    filters,
    sortByConversationAttention,
    resolveClaimee,
  ])

  // Each column's paging state, so a lane deeper than one page can say so and
  // fetch the rest. Rebuilt per render (the lists change every load) and read
  // only at the column tail.
  const paging: Record<ColumnId, PagedList<Task>> = {
    queued: queuedList,
    claimed: claimedList,
    in_progress: inProgressList,
    in_review: inReviewList,
    done: doneList,
  }

  const rawByColumn = useMemo<Record<ColumnId, Task[]>>(
    () => ({
      queued,
      claimed,
      in_progress: inProgress,
      in_review: inReview,
      done,
    }),
    [queued, claimed, inProgress, inReview, done],
  )

  const allTasks = useMemo(() => {
    const map = new Map<string, Task>()
    for (const t of [...queued, ...claimed, ...inProgress, ...inReview, ...done]) {
      map.set(t.id, t)
    }
    return map
  }, [queued, claimed, inProgress, inReview, done])

  // taskId → column lookup, built once per board state. getColumn is called
  // per dnd onDragOver event (which fires rapidly while dragging), so an O(1)
  // map beats re-scanning all five lists on every move.
  const columnByTask = useMemo<Map<string, ColumnId>>(() => {
    const m = new Map<string, ColumnId>()
    for (const t of queued) m.set(t.id, 'queued')
    for (const t of claimed) m.set(t.id, 'claimed')
    for (const t of inProgress) m.set(t.id, 'in_progress')
    for (const t of inReview) m.set(t.id, 'in_review')
    for (const t of done) m.set(t.id, 'done')
    return m
  }, [queued, claimed, inProgress, inReview, done])

  const getColumn = useCallback(
    (taskId: string): ColumnId | null => columnByTask.get(taskId) ?? null,
    [columnByTask],
  )

  // The raw task-level resolve-all requests. complete = drag-to-Done (keep the
  // card in Done); requeue = back to the queue. Both force-resolve every
  // unresolved artifact server-side and cancel a live run; we only confirm first.
  const fireComplete = useCallback(
    async (taskId: string) => {
      try {
        // A failure (e.g. a teardown that partially failed) must surface — a
        // silent fetchTasks would repaint as if the move succeeded.
        await apiFetch(`/api/tasks/${taskId}/swipe`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ action: 'complete', hesitation_ms: 0 }),
        })
        fetchTasks()
      } catch (err) {
        toast.error(httpErrorMessage(err, 'Could not mark the task done.'))
      }
    },
    [fetchTasks],
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

  // requestResolveAll gates a complete/requeue behind the confirmation modal when
  // the task's run still has unresolved artifacts. Returns true when it deferred
  // to the modal (the caller must NOT fire its own request); false when there's
  // nothing to resolve and the caller should proceed directly.
  const requestResolveAll = useCallback(
    (taskId: string, action: 'complete' | 'requeue'): boolean => {
      const conversation = conversations[taskId]
      if (!conversation || !hasUnresolvedArtifacts(conversation)) return false
      const c = approvalCounts(conversation)
      setConfirmResolve({
        taskId,
        prCount: c.pr,
        reviewCount: c.review,
        isLive: isActiveConversation(conversation),
        action,
      })
      return true
    },
    [conversations],
  )

  // The board opens at the left (Queued first). With Claimed + In
  // Review collapsed by default, the lane strip is compact enough that the
  // work-bearing lanes fit without a forced scroll offset — so we start at the
  // natural left edge rather than snapping past Queued (the old fixed 544px
  // offset assumed every column was a full 520px, which no longer holds).
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

      // Externally terminal tasks (merged/closed PRs) can't be dragged.
      const terminalEvents = ['github:pr:merged', 'github:pr:closed']
      if (terminalEvents.includes(task.event_type)) return

      // Bot-claimed tasks in In Progress / In Review are bot-managed —
      // the user shouldn't drag them around (status is set by the
      // spawner's auto-advance). Reassignment happens via the assignee
      // picker. Silently refuse the drag rather than nag.
      if (task.claimed_by_agent_id && (sourceCol === 'in_progress' || sourceCol === 'in_review')) {
        return
      }

      // Queue → anywhere: requires a claim first. Queue → Claimed is
      // the natural drag; Queue → In Progress / In Review skips a step
      // (rare but allowed for the user's convenience — claims then
      // advances). Queue → Done is dismiss.
      if (sourceCol === 'queued') {
        if (targetCol === 'done') {
          await apiFetch(`/api/tasks/${taskId}/swipe`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ action: 'dismiss', hesitation_ms: 0 }),
          })
          fetchTasks()
          return
        }
        // Claim first, then advance if needed.
        await apiFetch(`/api/tasks/${taskId}/swipe`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ action: 'claim', hesitation_ms: 0 }),
        })
        if (targetCol === 'in_progress' || targetCol === 'in_review') {
          await apiFetch(`/api/tasks/${taskId}/advance`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ to: targetCol }),
          })
        }
        fetchTasks()
        return
      }

      // Any → Queued: requeue. Clears the claim and resets status. When the run
      // still has unresolved artifacts this force-resolves them (+ cancels a live
      // run), so confirm first; otherwise requeue straight away.
      if (targetCol === 'queued') {
        if (requestResolveAll(taskId, 'requeue')) return
        await fireRequeue(taskId)
        return
      }

      // Any → Done: complete (preserves the card in Done; distinct from queue →
      // done which dismisses). Same resolve-all gate as requeue.
      if (targetCol === 'done') {
        if (requestResolveAll(taskId, 'complete')) return
        await fireComplete(taskId)
        return
      }

      // Claimed / In Progress / In Review transitions (user-claimed
      // only — bot tasks short-circuited above). Backward transitions
      // (e.g. In Review → In Progress) are allowed by the backend
      // AdvanceStatusForUser guard.
      if (targetCol === 'claimed') {
        // Claimed isn't a real status — it's "status=queued + claim
        // held". Going "back to Claimed" from In Progress/In Review
        // means flipping status to queued without releasing the claim.
        // The current store doesn't expose that exact transition; the
        // closest is requeue (which clears claim too). For v1, the
        // back-to-Claimed gesture isn't supported — the user can drag
        // forward through stages or all the way back to Queued.
        return
      }
      if (targetCol === 'in_progress' || targetCol === 'in_review') {
        await apiFetch(`/api/tasks/${taskId}/advance`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ to: targetCol }),
        })
        fetchTasks()
        return
      }
    } catch (err) {
      // A failed swipe/advance/requeue mutation would otherwise leave the board
      // silently in the wrong state; surface it and let the next fetchTasks
      // reconcile.
      toast.error(httpErrorMessage(err, 'Could not move the task — please try again.'))
    }
  }

  const handleRequeue = useCallback(
    async (taskId: string) => {
      if (requestResolveAll(taskId, 'requeue')) return
      await fireRequeue(taskId)
    },
    [requestResolveAll, fireRequeue],
  )

  // Assignee picker callbacks. The picker is the primary surface for
  // claim mutations in the board; drag is for column moves.
  const handlePickerClaim = useCallback(
    async (task: Task) => {
      try {
        await apiFetch(`/api/tasks/${task.id}/swipe`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ action: 'claim', hesitation_ms: 0 }),
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
      // queued). Same path the drag-to-Queue gesture uses — so an unclaim that
      // would strand unresolved artifacts is gated by the same confirmation.
      if (requestResolveAll(task.id, 'requeue')) return
      await fireRequeue(task.id)
    },
    [requestResolveAll, fireRequeue],
  )

  const handlePickerDelegate = useCallback((task: Task) => {
    pendingDelegateTask.current = task
    setShowPromptPicker(true)
  }, [])

  // Open a run's approval overlay (review or PR editor) by artifact id. Hoisted
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
        await apiFetch(`/api/tasks/${task.id}/swipe`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            action: 'reassign',
            hesitation_ms: 0,
            target_user_id: targetUserID,
          }),
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
        const body = await apiJSON<{ conversation_id?: string }>(`/api/tasks/${task.id}/swipe`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            action: 'delegate',
            hesitation_ms: 0,
            blueprint_id: promptId,
          }),
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
        // run didn't fire" marker — the task is in the bot's lane with no
        // run, so record the inline per-card failure the retry affordance
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
          <p className="text-[13px] text-text-tertiary">Loading board…</p>
        </div>
      </>
    )
  }

  // Per-column header extras. (Queued's snoozed toggle now lives inside the
  // filter panel — see the `snooze` prop below — not as a header button.)
  const doneHeader = (
    <span
      className="text-[10px] text-text-tertiary"
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
      <div className="-mt-4 flex h-[calc(100dvh-7rem)] min-h-[26rem] flex-col">
        {/* Per-page team filter. Renders nothing at ≤1 team. */}
        {teams.length >= 2 && (
          <div className="flex items-center justify-end px-1 pb-3">
            <TeamScopeSelect value={teamFilter} onChange={setTeamFilter} />
          </div>
        )}

        {/* Horizontal-scroll container for 5 columns. The strip is
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
                  count={rawByColumn[colId].length}
                  onExpand={() => toggleCollapse(colId)}
                />
              ) : (
                <BoardColumn
                  key={colId}
                  id={colId}
                  index={i}
                  dragOver={activeDropCol === colId}
                  title={COLUMN_TITLES[colId]}
                  tasks={rawByColumn[colId]}
                  filter={filters[colId]}
                  onFilterChange={(next) => setFilters((prev) => ({ ...prev, [colId]: next }))}
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
                    tasks={filtered[colId]}
                    conversations={conversations}
                    conversationFeeds={conversationFeeds}
                    chainStepConversations={chainStepConversations}
                    permQueues={permQueueMap}
                    onResolvePermission={resolvePermission}
                    currentUserID={currentUserID}
                    members={members}
                    bot={bot}
                    delegateFailures={delegateFailures}
                    onRequeue={handleRequeue}
                    onPickerClaim={handlePickerClaim}
                    onPickerUnclaim={handlePickerUnclaim}
                    onPickerDelegate={handlePickerDelegate}
                    onPickerReassign={handlePickerReassign}
                    onOpenApproval={handleOpenApproval}
                    onArtifactResolved={fetchTasks}
                    onRetry={handlePickerDelegate}
                  />
                  {paging[colId].hasMore && (
                    <ColumnMore
                      shown={rawByColumn[colId].length}
                      total={paging[colId].total}
                      loading={paging[colId].loading}
                      onLoadMore={paging[colId].loadMore}
                    />
                  )}
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
        actionLabel={confirmResolve?.action === 'complete' ? 'Mark done' : 'Return to queue'}
        busy={confirmBusy}
        onConfirm={async () => {
          if (!confirmResolve) return
          setConfirmBusy(true)
          try {
            if (confirmResolve.action === 'complete') await fireComplete(confirmResolve.taskId)
            else await fireRequeue(confirmResolve.taskId)
            setConfirmResolve(null)
          } finally {
            setConfirmBusy(false)
          }
        }}
        onCancel={() => setConfirmResolve(null)}
      />
    </DndContext>
  )
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
// wrappers it renders are memoized with identity-stable props (per-run slices
// of the board maps + useCallback'd handlers, with the per-card closures and
// the AssigneePicker element built inside the wrapper, below its memo
// boundary), so a live-feed tick for one run re-renders that run's card only —
// not every card on the board.
function ColumnContents({
  colId,
  tasks,
  conversations,
  conversationFeeds,
  chainStepConversations,
  permQueues,
  onResolvePermission,
  currentUserID,
  members,
  bot,
  delegateFailures,
  onRequeue,
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
  onRequeue: (taskID: string) => void
  onOpenApproval: (conversationID: string, kind: 'review' | 'pr', artifactId: string) => void
  onArtifactResolved: () => void
  onRetry: (task: Task) => void
} & Omit<PickerProps, 'pickerReadOnly'>) {
  if (tasks.length === 0) {
    return <EmptyColumn>{emptyLabelFor(colId)}</EmptyColumn>
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
        // Queued tasks never render as AgentCards even if the
        // conversations map has a stale entry from a prior delegate.
        // After requeue, the run row stays in the DB but the task
        // is back in the team queue with claim cols cleared —
        // showing an AgentCard there would lie about who's working
        // on it. The map is cleaned up on the next WS update; this
        // gate covers the window before that lands.
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
              onRequeue={onRequeue}
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
            onRequeue={onRequeue}
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
    case 'claimed':
      return 'Nothing claimed'
    case 'in_progress':
      return 'Nothing in progress'
    case 'in_review':
      return 'Nothing in review'
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
  onRequeue,
  onRetry,
  ...picker
}: {
  task: Task
  delegateFailure?: string
  onRequeue: (taskID: string) => void
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
  // AgentCard's elapsed-time / expand cluster after a run completes.
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
      onRequeue={() => onRequeue(task.id)}
      delegateFailed={delegateFailure ? { message: delegateFailure } : undefined}
      onRetry={delegateFailure ? () => onRetry(task) : undefined}
      assigneeSlot={<CardAssigneePicker task={task} picker={picker} />}
      {...attributes}
      {...listeners}
    />
  )
})

// Run statuses where the AgentCard is safe to drag between columns: the
// terminals, and nothing else. A run still setting up or executing stays
// anchored — the cancel button is the right intent there, and dragging
// mid-run would race with the spawner's status transitions.
const draggableConversationStatuses: ReadonlySet<string> = new Set(TERMINAL_CONVERSATION_STATUSES)

const SortableAgentCard = memo(function SortableAgentCard({
  task,
  conversation,
  chainSteps,
  feed,
  pendingPermissions,
  onResolvePermission,
  onRequeue,
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
  onRequeue: (taskID: string) => void
  onOpenApproval: (conversationID: string, kind: 'review' | 'pr', artifactId: string) => void
  onArtifactResolved: () => void
} & PickerProps) {
  // Bot-managed cards in in_progress / in_review are
  // read-only — column placement is owned by the spawner's run-state
  // mirror. The drop handler also short-circuits these drags, but
  // baking the guard into `disabled` here removes the misleading
  // grab cursor + drag preview so the user doesn't see a draggable
  // affordance that always snaps back.
  const botManaged =
    !!task.claimed_by_agent_id && (task.status === 'in_progress' || task.status === 'in_review')
  // A terminal run is draggable; so is a settled run (idle/open) that still has
  // unresolved artifacts — drag-to-Done / Return-to-queue is how you resolve them
  // (behind the confirmation). An actively-executing turn stays anchored (cancel
  // is the right intent; dragging mid-run races the spawner).
  const draggable =
    (draggableConversationStatuses.has(conversation.Status) ||
      (hasUnresolvedArtifacts(conversation) && !isActiveConversation(conversation))) &&
    !botManaged
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({
    id: task.id,
    disabled: !draggable,
  })
  const style: React.CSSProperties = {
    transform: CSS.Transform.toString(transform),
    transition,
    opacity: isDragging ? 0.3 : 1,
    cursor: draggable ? 'grab' : undefined,
  }
  // assigneeSlot forwarded into AgentCard's header cluster
  // so it shares the gap-2 spacing with elapsed/expand/cancel
  // instead of overlapping them via absolute positioning. Same
  // reasoning as the TaskCard wrapper above.
  return (
    <div
      ref={setNodeRef}
      style={style}
      {...(draggable ? attributes : {})}
      {...(draggable ? listeners : {})}
    >
      <AgentCard
        task={task}
        conversation={conversation}
        chainSteps={chainSteps}
        feed={feed}
        pendingPermissions={pendingPermissions}
        onResolvePermission={(toolCallID, decision) =>
          onResolvePermission(conversation.ID, toolCallID, decision)
        }
        onRequeue={() => onRequeue(task.id)}
        onOpenArtifact={(kind, artifactId) => onOpenApproval(conversation.ID, kind, artifactId)}
        onArtifactResolved={onArtifactResolved}
        assigneeSlot={<CardAssigneePicker task={task} picker={picker} />}
      />
    </div>
  )
})

function EmptyColumn({ children }: { children: React.ReactNode }) {
  return <p className="text-[12px] text-text-tertiary text-center py-12">{children}</p>
}

// ColumnMore is the tail of a column that holds more than one page. A lane
// that silently stopped at its page size would read as "that's all there is",
// which is the failure the list contract exists to prevent — so the count is
// stated and the next page is one click away.
function ColumnMore({
  shown,
  total,
  loading,
  onLoadMore,
}: {
  shown: number
  total: number | null
  loading: boolean
  onLoadMore: () => void
}) {
  // A null total means the server did not count the result set (a proxy
  // list). Say what we know — "showing 40" — rather than inventing an "of 0".
  const label = total === null ? `showing ${shown}` : `showing ${shown} of ${total}`
  return (
    <button
      type="button"
      onClick={onLoadMore}
      disabled={loading}
      className="w-full py-2 text-[12px] text-text-tertiary hover:text-text-secondary disabled:opacity-50"
    >
      {loading ? 'Loading…' : `Load more — ${label}`}
    </button>
  )
}
