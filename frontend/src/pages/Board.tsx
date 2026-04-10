import { useState, useEffect, useCallback, useMemo, useRef } from 'react'
import type { Task, AgentRun, AgentMessage, WSEvent } from '../types'
import { useWebSocket } from '../hooks/useWebSocket'
import AgentCard from '../components/AgentCard'
import TaskCard from '../components/TaskCard'
import PromptPicker from '../components/PromptPicker'
import ReviewOverlay from '../components/ReviewOverlay'
import EventBadge from '../components/EventBadge'
import { motion, AnimatePresence } from 'motion/react'
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
import {
  SortableContext,
  verticalListSortingStrategy,
  useSortable,
} from '@dnd-kit/sortable'
import { CSS } from '@dnd-kit/utilities'

type ColumnId = 'queue' | 'you' | 'agent' | 'done'

export default function Board() {
  const [queued, setQueued] = useState<Task[]>([])
  const [claimed, setClaimed] = useState<Task[]>([])
  const [delegated, setDelegated] = useState<Task[]>([])
  const [done, setDone] = useState<Task[]>([])
  const [loading, setLoading] = useState(true)

  // Agent run state
  const [agentRuns, setAgentRuns] = useState<Record<string, AgentRun>>({})
  const [agentMessages, setAgentMessages] = useState<Record<string, AgentMessage[]>>({})

  // Sidebar state
  const [sidebarOpen, setSidebarOpen] = useState(false)
  const [search, setSearch] = useState('')
  const [sourceFilter, setSourceFilter] = useState<'all' | 'github' | 'jira'>('all')

  // Drag state
  const [activeId, setActiveId] = useState<string | null>(null)
  const [overColumn, setOverColumn] = useState<ColumnId | null>(null)
  const [draggingFromSidebar, setDraggingFromSidebar] = useState(false)

  // Delegate flow
  const [showPromptPicker, setShowPromptPicker] = useState(false)
  const pendingDelegateTask = useRef<Task | null>(null)

  // Review overlay
  const [reviewRunID, setReviewRunID] = useState<string | null>(null)

  const fetchTasks = useCallback(async () => {
    try {
      const [queuedRes, claimedRes, delegatedRes, doneRes] = await Promise.all([
        fetch('/api/queue').then((r) => r.ok ? r.json() : []),
        fetch('/api/tasks?status=claimed').then((r) => r.ok ? r.json() : []),
        fetch('/api/tasks?status=delegated').then((r) => r.ok ? r.json() : []),
        fetch('/api/tasks?status=done').then((r) => r.ok ? r.json() : []),
      ])
      setQueued(queuedRes)
      setClaimed(claimedRes)
      setDelegated(delegatedRes)
      setDone(doneRes)

      // Fetch agent runs for delegated and done tasks
      for (const task of [...delegatedRes, ...doneRes]) {
        try {
          const runsRes = await fetch(`/api/agent/runs?task_id=${task.id}`)
          if (!runsRes.ok) continue
          const runs: AgentRun[] = await runsRes.json()
          if (runs.length > 0) {
            const latestRun = runs[0]
            setAgentRuns((prev) => ({ ...prev, [task.id]: latestRun }))
            const msgsRes = await fetch(`/api/agent/runs/${latestRun.ID}/messages`)
            if (!msgsRes.ok) continue
            const msgs: AgentMessage[] = await msgsRes.json()
            setAgentMessages((prev) => ({ ...prev, [latestRun.ID]: msgs }))
          }
        } catch {
          // Individual agent run fetch failed — skip
        }
      }
    } catch {
      // Network error — keep stale data
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { fetchTasks() }, [fetchTasks])

  useWebSocket(useCallback((event: WSEvent) => {
    if (event.type === 'agent_run_update') {
      setAgentRuns((prev) => {
        const updated = { ...prev }
        for (const [taskId, run] of Object.entries(updated)) {
          if (run.ID === event.run_id) {
            updated[taskId] = { ...run, Status: event.data.status }
            fetch(`/api/agent/runs/${event.run_id}`)
              .then((r) => r.json())
              .then((fullRun: AgentRun) => {
                setAgentRuns((p) => {
                  const u = { ...p }
                  for (const [tid, r] of Object.entries(u)) {
                    if (r.ID === event.run_id) u[tid] = fullRun
                  }
                  return u
                })
              })
            break
          }
        }
        return updated
      })
      if (['completed', 'failed', 'pending_approval'].includes(event.data.status)) {
        fetchTasks()
      }
    }
    if (event.type === 'agent_message') {
      setAgentMessages((prev) => ({
        ...prev,
        [event.run_id]: [...(prev[event.run_id] || []), event.data as AgentMessage],
      }))
    }
    if (event.type === 'tasks_updated' || event.type === 'scoring_completed') {
      fetchTasks()
    }
  }, [fetchTasks]))

  // Agent column: attention-weighted ordering
  // Top: needs review (pending_approval), then failed/cancelled, then running at bottom
  const agentItems = useMemo(() => {
    const weight = (t: Task) => {
      const run = agentRuns[t.id]
      if (!run) return 2
      if (run.Status === 'pending_approval') return 0
      if (run.Status === 'failed' || run.Status === 'cancelled') return 1
      if (run.Status === 'completed') return 3
      return 2 // running/active
    }
    return [...delegated].sort((a, b) => weight(a) - weight(b))
  }, [delegated, agentRuns])

  // Filtered queue for sidebar
  const filteredQueue = useMemo(() => {
    let items = queued
    if (sourceFilter !== 'all') {
      items = items.filter((t) => t.source === sourceFilter)
    }
    if (search.trim()) {
      const q = search.toLowerCase()
      items = items.filter((t) =>
        t.title.toLowerCase().includes(q) ||
        t.repo?.toLowerCase().includes(q) ||
        t.author?.toLowerCase().includes(q) ||
        t.ai_summary?.toLowerCase().includes(q) ||
        t.event_type?.toLowerCase().includes(q)
      )
    }
    return items
  }, [queued, search, sourceFilter])

  // Unique event types in queue for filter display
  const queueEventTypes = useMemo(() => {
    const types = new Set<string>()
    for (const t of queued) {
      if (t.event_type) types.add(t.event_type)
    }
    return Array.from(types)
  }, [queued])

  // All tasks indexed for drag lookup
  const allTasks = useMemo(() => {
    const map = new Map<string, Task>()
    for (const t of [...queued, ...claimed, ...delegated, ...done]) {
      map.set(t.id, t)
    }
    return map
  }, [queued, claimed, delegated, done])

  // Column membership for dragging
  const getColumn = useCallback((taskId: string): ColumnId | null => {
    if (queued.some((t) => t.id === taskId)) return 'queue'
    if (claimed.some((t) => t.id === taskId)) return 'you'
    if (delegated.some((t) => t.id === taskId)) return 'agent'
    if (done.some((t) => t.id === taskId)) return 'done'
    return null
  }, [queued, claimed, delegated, done])

  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 5 } })
  )

  const handleDragStart = (event: DragStartEvent) => {
    const id = String(event.active.id)
    setActiveId(id)
    // Auto-collapse sidebar when dragging from it
    if (sidebarOpen && getColumn(id) === 'queue') {
      setDraggingFromSidebar(true)
      setSidebarOpen(false)
    }
  }

  const handleDragOver = (event: DragOverEvent) => {
    const { over } = event
    if (!over) { setOverColumn(null); return }

    const overId = String(over.id)
    if (['you', 'agent', 'done'].includes(overId)) {
      setOverColumn(overId as ColumnId)
    } else {
      const col = getColumn(overId)
      setOverColumn(col)
    }
  }

  const handleDragEnd = async (event: DragEndEvent) => {
    const { active, over } = event
    const wasDraggingFromSidebar = draggingFromSidebar

    setActiveId(null)
    setOverColumn(null)
    setDraggingFromSidebar(false)

    // Re-expand sidebar if drag came from there (regardless of outcome)
    if (wasDraggingFromSidebar) {
      setSidebarOpen(true)
    }

    if (!over) return

    const taskId = String(active.id)
    const sourceCol = getColumn(taskId)
    const task = allTasks.get(taskId)
    if (!sourceCol || !task) return

    // Determine target column
    const overId = String(over.id)
    let targetCol: ColumnId
    if (['you', 'agent', 'done'].includes(overId)) {
      targetCol = overId as ColumnId
    } else {
      targetCol = getColumn(overId) || sourceCol
    }

    if (sourceCol === targetCol) return

    // Queue → You: claim
    if (sourceCol === 'queue' && targetCol === 'you') {
      await fetch(`/api/tasks/${taskId}/swipe`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ action: 'claim', hesitation_ms: 0 }),
      })
      fetchTasks()
      return
    }

    // Queue → Agent: delegate (prompt picker)
    if (sourceCol === 'queue' && targetCol === 'agent') {
      pendingDelegateTask.current = task
      setShowPromptPicker(true)
      return
    }

    // Queue → Done: dismiss
    if (sourceCol === 'queue' && targetCol === 'done') {
      await fetch(`/api/tasks/${taskId}/swipe`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ action: 'dismiss', hesitation_ms: 0 }),
      })
      fetchTasks()
      return
    }

    // You → Agent: delegate claimed task (SKY-133)
    if (sourceCol === 'you' && targetCol === 'agent') {
      pendingDelegateTask.current = task
      setShowPromptPicker(true)
      return
    }

    // Any → Queue: undo/requeue
    if (targetCol === 'queue' || (wasDraggingFromSidebar && !['you', 'agent', 'done'].includes(overId))) {
      // Don't requeue if source is already queue
      if (sourceCol !== 'queue') {
        await fetch(`/api/tasks/${taskId}/undo`, { method: 'POST' })
        fetchTasks()
      }
      return
    }
  }

  const handleRequeue = useCallback(async (taskId: string) => {
    await fetch(`/api/tasks/${taskId}/undo`, { method: 'POST' })
    fetchTasks()
  }, [fetchTasks])

  const handlePromptSelected = useCallback(async (promptId: string) => {
    setShowPromptPicker(false)
    const task = pendingDelegateTask.current
    if (!task) return
    pendingDelegateTask.current = null
    await fetch(`/api/tasks/${task.id}/swipe`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action: 'delegate', hesitation_ms: 0, prompt_id: promptId }),
    })
    fetchTasks()
  }, [fetchTasks])

  const activeTask = activeId ? allTasks.get(activeId) : null

  if (loading) {
    return (
      <div className="flex items-center justify-center min-h-[70vh]">
        <p className="text-[13px] text-text-tertiary">Loading board...</p>
      </div>
    )
  }

  return (
    <DndContext
      sensors={sensors}
      collisionDetection={closestCenter}
      onDragStart={handleDragStart}
      onDragOver={handleDragOver}
      onDragEnd={handleDragEnd}
    >
      <div className="relative min-h-[70vh]">
        {/* Queue sidebar — collapsed strip */}
        <AnimatePresence mode="wait">
          {!sidebarOpen && (
            <motion.button
              key="collapsed"
              aria-label="Open queue sidebar"
              initial={{ opacity: 0, x: -20 }}
              animate={{ opacity: 1, x: 0 }}
              exit={{ opacity: 0, x: -20 }}
              transition={{ duration: 0.2, ease: 'easeOut' }}
              onClick={() => setSidebarOpen(true)}
              className="fixed left-4 top-20 bottom-4 w-10 z-30 bg-surface-raised/80 backdrop-blur-xl border border-border-glass rounded-2xl shadow-sm shadow-black/[0.03] flex flex-col items-center pt-4 gap-3 hover:border-accent/20 transition-colors group"
            >
              <svg width="16" height="16" viewBox="0 0 16 16" fill="none" className="text-text-tertiary group-hover:text-accent transition-colors shrink-0">
                <path d="M6 3l5 5-5 5" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" />
              </svg>
              <span className="text-[11px] font-medium text-text-tertiary bg-black/[0.04] rounded-full px-2 py-0.5 shrink-0">
                {queued.length}
              </span>
              <span className="text-[10px] text-text-tertiary [writing-mode:vertical-lr] rotate-180 tracking-wider uppercase font-medium mt-1">
                Queue
              </span>
            </motion.button>
          )}
        </AnimatePresence>

        {/* Queue sidebar — expanded overlay */}
        <AnimatePresence>
          {sidebarOpen && (
            <>
              {/* Backdrop */}
              <motion.div
                initial={{ opacity: 0 }}
                animate={{ opacity: 1 }}
                exit={{ opacity: 0 }}
                transition={{ duration: 0.2 }}
                className="fixed inset-0 z-30"
                onClick={() => setSidebarOpen(false)}
              />
              <motion.div
                initial={{ x: -290, opacity: 0 }}
                animate={{ x: 0, opacity: 1 }}
                exit={{ x: -290, opacity: 0 }}
                transition={{ type: 'spring', damping: 28, stiffness: 300 }}
                className="fixed left-4 top-20 bottom-4 w-[280px] z-40 bg-surface-raised/95 backdrop-blur-xl border border-border-glass rounded-2xl shadow-xl shadow-black/[0.08] flex flex-col overflow-hidden"
              >
              {/* Header */}
              <div className="px-4 pt-4 pb-3 border-b border-border-subtle shrink-0">
                <div className="flex items-center justify-between mb-3">
                  <div className="flex items-center gap-2">
                    <h2 className="text-[13px] font-semibold text-text-primary">Queue</h2>
                    <span className="text-[11px] text-text-tertiary bg-black/[0.04] rounded-full px-2 py-0.5">
                      {filteredQueue.length}{filteredQueue.length !== queued.length ? `/${queued.length}` : ''}
                    </span>
                  </div>
                  <button
                    onClick={() => setSidebarOpen(false)}
                    aria-label="Close queue sidebar"
                    className="text-text-tertiary hover:text-text-secondary transition-colors text-lg leading-none px-1"
                  >
                    &times;
                  </button>
                </div>

                {/* Search */}
                <input
                  type="text"
                  placeholder="Search tasks..."
                  value={search}
                  onChange={(e) => setSearch(e.target.value)}
                  className="w-full bg-white/50 border border-border-subtle rounded-xl px-3 py-2 text-[12px] text-text-primary placeholder-text-tertiary focus:outline-none focus:ring-2 focus:ring-accent/30 focus:border-accent/40 transition-colors mb-2"
                  autoFocus
                />

                {/* Source filter */}
                <div className="flex gap-1 mb-1">
                  {(['all', 'github', 'jira'] as const).map((f) => (
                    <button
                      key={f}
                      onClick={() => setSourceFilter(f)}
                      className={`text-[10px] px-2 py-0.5 rounded-full transition-colors ${
                        sourceFilter === f
                          ? 'bg-accent/10 text-accent font-medium'
                          : 'text-text-tertiary hover:text-text-secondary'
                      }`}
                    >
                      {f === 'all' ? 'All' : f === 'github' ? 'GitHub' : 'Jira'}
                    </button>
                  ))}
                </div>

                {/* Event type quick-filter chips */}
                {queueEventTypes.length > 0 && (
                  <div className="flex flex-wrap gap-1 mt-1">
                    {queueEventTypes.map((et) => (
                      <button
                        key={et}
                        onClick={() => setSearch(et)}
                        className="opacity-70 hover:opacity-100 transition-opacity"
                      >
                        <EventBadge eventType={et} compact />
                      </button>
                    ))}
                  </div>
                )}
              </div>

              {/* Scrollable task list */}
              <div className="flex-1 overflow-y-auto p-2 space-y-2">
                <p className="text-[10px] text-text-tertiary px-2 py-1">
                  Drag tasks to a column
                </p>
                {filteredQueue.length === 0 ? (
                  <p className="text-[12px] text-text-tertiary text-center py-8">
                    {queued.length === 0 ? 'Queue is empty' : 'No matching tasks'}
                  </p>
                ) : (
                  <SortableContext items={filteredQueue.map((t) => t.id)} strategy={verticalListSortingStrategy}>
                    {filteredQueue.map((task) => (
                      <SidebarTaskCard key={task.id} task={task} />
                    ))}
                  </SortableContext>
                )}
              </div>
              </motion.div>
            </>
          )}
        </AnimatePresence>

        {/* Main board — 3 columns */}
        <div
          className="grid grid-cols-3 gap-6 min-h-[70vh]"
          style={{ marginLeft: '3rem' }}
        >
          {/* You column */}
          <DroppableColumn
            id="you"
            title="You"
            count={claimed.length}
            isOver={overColumn === 'you'}
          >
            <SortableContext items={claimed.map((t) => t.id)} strategy={verticalListSortingStrategy}>
              {claimed.length === 0 ? (
                <EmptyColumn>Nothing claimed</EmptyColumn>
              ) : (
                claimed.map((task) => (
                  <SortableTaskCard key={task.id} task={task} onRequeue={() => handleRequeue(task.id)} />
                ))
              )}
            </SortableContext>
          </DroppableColumn>

          {/* Agent column — attention-weighted */}
          <DroppableColumn
            id="agent"
            title="Agent"
            count={agentItems.length}
            isOver={overColumn === 'agent'}
          >
            <SortableContext items={agentItems.filter((t) => !agentRuns[t.id]).map((t) => t.id)} strategy={verticalListSortingStrategy}>
              {agentItems.length === 0 ? (
                <EmptyColumn>No delegated tasks</EmptyColumn>
              ) : (
                agentItems.map((task) =>
                  agentRuns[task.id] ? (
                    <AgentCard
                      key={task.id}
                      task={task}
                      run={agentRuns[task.id]}
                      messages={agentMessages[agentRuns[task.id].ID] || []}
                      onRequeue={() => handleRequeue(task.id)}
                      onReview={() => setReviewRunID(agentRuns[task.id].ID)}
                    />
                  ) : (
                    <SortableTaskCard key={task.id} task={task} onRequeue={() => handleRequeue(task.id)} />
                  )
                )
              )}
            </SortableContext>
          </DroppableColumn>

          {/* Done column */}
          <DroppableColumn
            id="done"
            title="Done"
            count={done.length}
            isOver={overColumn === 'done'}
          >
            <SortableContext items={done.filter((t) => !agentRuns[t.id]).map((t) => t.id)} strategy={verticalListSortingStrategy}>
              {done.length === 0 ? (
                <EmptyColumn>No completed items</EmptyColumn>
              ) : (
                done.map((task) =>
                  agentRuns[task.id] ? (
                    <AgentCard
                      key={task.id}
                      task={task}
                      run={agentRuns[task.id]}
                      messages={agentMessages[agentRuns[task.id].ID] || []}
                      onRequeue={() => handleRequeue(task.id)}
                      onReview={() => setReviewRunID(agentRuns[task.id].ID)}
                    />
                  ) : (
                    <SortableTaskCard key={task.id} task={task} />
                  )
                )
              )}
            </SortableContext>
          </DroppableColumn>
        </div>
      </div>

      {/* Drag overlay — floating card that follows cursor */}
      <DragOverlay dropAnimation={null}>
        {activeTask && (
          <div className="w-[250px]">
            <TaskCard task={activeTask} isDragging />
          </div>
        )}
      </DragOverlay>

      {/* Prompt picker for delegation */}
      <PromptPicker
        open={showPromptPicker}
        onSelect={handlePromptSelected}
        onClose={() => { setShowPromptPicker(false); pendingDelegateTask.current = null }}
        onEditPrompts={() => { setShowPromptPicker(false); pendingDelegateTask.current = null; window.location.href = '/prompts' }}
      />

      {/* Review overlay for pending_approval runs */}
      <ReviewOverlay
        runID={reviewRunID ?? ''}
        open={reviewRunID !== null}
        onClose={() => { setReviewRunID(null); fetchTasks() }}
      />
    </DndContext>
  )
}

/** Compact card for the sidebar queue — smaller than a full TaskCard */
function SidebarTaskCard({ task }: { task: Task }) {
  const {
    attributes,
    listeners,
    setNodeRef,
    transform,
    transition,
    isDragging,
  } = useSortable({ id: task.id })

  const style: React.CSSProperties = {
    transform: CSS.Transform.toString(transform),
    transition,
    opacity: isDragging ? 0.3 : 1,
  }

  return (
    <div
      ref={setNodeRef}
      style={style}
      className="bg-white/60 backdrop-blur border border-border-subtle rounded-xl px-3 py-2.5 cursor-grab active:cursor-grabbing hover:border-accent/20 transition-colors"
      {...attributes}
      {...listeners}
    >
      <div className="flex items-center gap-1.5 mb-1">
        <span className={`text-[9px] font-semibold uppercase tracking-wider px-1 py-0.5 rounded ${
          task.source === 'github' ? 'bg-black/[0.04] text-text-secondary' : 'bg-blue-500/10 text-blue-600'
        }`}>
          {task.source === 'github' ? 'GH' : 'Jira'}
        </span>
        <EventBadge eventType={task.event_type} compact />
      </div>
      <h4 className="text-[12px] font-medium text-text-primary leading-snug line-clamp-2">
        {task.title}
      </h4>
      <div className="flex items-center gap-2 mt-1 text-[10px] text-text-tertiary">
        {task.repo && <span className="truncate">{task.repo}</span>}
        {task.author && <span>{task.author}</span>}
      </div>
    </div>
  )
}

function SortableTaskCard({ task, onRequeue }: { task: Task; onRequeue?: () => void }) {
  const {
    attributes,
    listeners,
    setNodeRef,
    transform,
    transition,
    isDragging,
  } = useSortable({ id: task.id })

  const style: React.CSSProperties = {
    transform: CSS.Transform.toString(transform),
    transition,
    opacity: isDragging ? 0.3 : 1,
  }

  return (
    <TaskCard
      ref={setNodeRef}
      task={task}
      style={style}
      isDragging={false}
      onRequeue={onRequeue}
      {...attributes}
      {...listeners}
    />
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
    <div className="flex flex-col">
      <div className="flex items-center justify-between mb-3 px-1">
        <div className="flex items-center gap-2">
          <h2 className="text-[13px] font-medium text-text-secondary">{title}</h2>
          <span className="text-[11px] text-text-tertiary bg-black/[0.04] rounded-full px-2 py-0.5">
            {count}
          </span>
        </div>
      </div>
      <div
        ref={setNodeRef}
        className={`flex-1 rounded-2xl border bg-black/[0.01] p-3 space-y-3 overflow-y-auto max-h-[calc(100vh-180px)] transition-colors ${
          isOver
            ? 'border-accent/30 bg-accent/[0.03]'
            : 'border-border-subtle'
        }`}
      >
        {children}
      </div>
    </div>
  )
}

function EmptyColumn({ children }: { children: React.ReactNode }) {
  return (
    <p className="text-[12px] text-text-tertiary text-center py-12">{children}</p>
  )
}
