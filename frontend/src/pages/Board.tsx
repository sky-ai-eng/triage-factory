import { useState, useEffect, useCallback, useMemo, useRef } from 'react'
import type { Task, AgentRun, AgentMessage, WSEvent } from '../types'
import { useWebSocket } from '../hooks/useWebSocket'
import AgentCard from '../components/AgentCard'
import TaskCard from '../components/TaskCard'
import PromptPicker from '../components/PromptPicker'
import ReviewOverlay from '../components/ReviewOverlay'
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
  arrayMove,
} from '@dnd-kit/sortable'
import { CSS } from '@dnd-kit/utilities'

type ColumnId = 'queue' | 'in_progress' | 'done'
type InProgressFilter = 'all' | 'user' | 'agent'

export default function Board() {
  const [queued, setQueued] = useState<Task[]>([])
  const [claimed, setClaimed] = useState<Task[]>([])
  const [delegated, setDelegated] = useState<Task[]>([])
  const [done, setDone] = useState<Task[]>([])
  const [filter, setFilter] = useState<InProgressFilter>('all')
  const [loading, setLoading] = useState(true)

  // Agent run state
  const [agentRuns, setAgentRuns] = useState<Record<string, AgentRun>>({})
  const [agentMessages, setAgentMessages] = useState<Record<string, AgentMessage[]>>({})

  // Drag state
  const [activeId, setActiveId] = useState<string | null>(null)
  const [overColumn, setOverColumn] = useState<ColumnId | null>(null)

  // Delegate/claim popup when dragging to in_progress
  const [pendingInProgress, setPendingInProgress] = useState<Task | null>(null)
  const [showPromptPicker, setShowPromptPicker] = useState(false)

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

      // Fetch agent runs for delegated and done tasks (done tasks may have agent history)
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
          // Individual agent run fetch failed — skip it
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

  // Build the in-progress list (claimed + delegated, agents first)
  const inProgress = useMemo(() => {
    const agentItems = delegated.map((t) => ({ task: t, type: 'agent' as const }))
    const userItems = claimed.map((t) => ({ task: t, type: 'user' as const }))
    if (filter === 'user') return userItems
    if (filter === 'agent') return agentItems
    return [...agentItems, ...userItems]
  }, [claimed, delegated, filter])

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
    if (claimed.some((t) => t.id === taskId) || delegated.some((t) => t.id === taskId)) return 'in_progress'
    if (done.some((t) => t.id === taskId)) return 'done'
    return null
  }, [queued, claimed, delegated, done])

  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 5 } })
  )

  const handleDragStart = (event: DragStartEvent) => {
    setActiveId(String(event.active.id))
  }

  const handleDragOver = (event: DragOverEvent) => {
    const { over } = event
    if (!over) { setOverColumn(null); return }

    // over might be a column ID or another card's ID
    const overId = String(over.id)
    if (['queue', 'in_progress', 'done'].includes(overId)) {
      setOverColumn(overId as ColumnId)
    } else {
      // It's a card — figure out which column it's in
      const col = getColumn(overId)
      setOverColumn(col)
    }
  }

  const handleDragEnd = async (event: DragEndEvent) => {
    const { active, over } = event
    setActiveId(null)
    setOverColumn(null)

    if (!over) return

    const taskId = String(active.id)
    const sourceCol = getColumn(taskId)

    // Determine target column
    const overId = String(over.id)
    let targetCol: ColumnId
    if (['queue', 'in_progress', 'done'].includes(overId)) {
      targetCol = overId as ColumnId
    } else {
      targetCol = getColumn(overId) || sourceCol!
    }

    if (!sourceCol) return

    // Same column — reorder (local state only, no backend persistence)
    if (sourceCol === targetCol) {
      if (sourceCol === 'queue') {
        const oldIndex = queued.findIndex((t) => t.id === taskId)
        const overTaskIndex = queued.findIndex((t) => t.id === overId)
        if (oldIndex !== -1 && overTaskIndex !== -1 && oldIndex !== overTaskIndex) {
          setQueued(arrayMove(queued, oldIndex, overTaskIndex))
        }
      } else if (sourceCol === 'in_progress') {
        const oldIndex = claimed.findIndex((t) => t.id === taskId)
        const overTaskIndex = claimed.findIndex((t) => t.id === overId)
        if (oldIndex !== -1 && overTaskIndex !== -1 && oldIndex !== overTaskIndex) {
          setClaimed(arrayMove(claimed, oldIndex, overTaskIndex))
        }
      } else if (sourceCol === 'done') {
        const oldIndex = done.findIndex((t) => t.id === taskId)
        const overTaskIndex = done.findIndex((t) => t.id === overId)
        if (oldIndex !== -1 && overTaskIndex !== -1 && oldIndex !== overTaskIndex) {
          setDone(arrayMove(done, oldIndex, overTaskIndex))
        }
      }
      return
    }

    // Cross-column move
    const task = allTasks.get(taskId)
    if (!task) return

    // Block cross-column moves for externally terminal tasks (merged/closed PRs)
    const terminalEvents = ['github:pr:merged', 'github:pr:closed']
    if (terminalEvents.includes(task.event_type || '')) return

    // Moving to in_progress: show claim/delegate popup
    if (targetCol === 'in_progress' && sourceCol !== 'in_progress') {
      setPendingInProgress(task)
      return
    }

    // Moving back to queue: undo
    if (targetCol === 'queue' && sourceCol !== 'queue') {
      // Optimistic UI
      if (sourceCol === 'in_progress') {
        setClaimed((prev) => prev.filter((t) => t.id !== taskId))
        setDelegated((prev) => prev.filter((t) => t.id !== taskId))
      } else if (sourceCol === 'done') {
        setDone((prev) => prev.filter((t) => t.id !== taskId))
      }
      setQueued((prev) => [task, ...prev])

      await fetch(`/api/tasks/${taskId}/undo`, { method: 'POST' })
      fetchTasks()
      return
    }
  }

  const handleRequeue = useCallback(async (taskId: string) => {
    await fetch(`/api/tasks/${taskId}/undo`, { method: 'POST' })
    fetchTasks()
  }, [fetchTasks])

  const handleClaimFromPopup = useCallback(async (task: Task) => {
    setPendingInProgress(null)
    await fetch(`/api/tasks/${task.id}/swipe`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action: 'claim', hesitation_ms: 0 }),
    })
    fetchTasks()
  }, [fetchTasks])

  const handleDelegateFromPopup = useCallback((task: Task) => {
    setPendingInProgress(null)
    setShowPromptPicker(true)
    // Store the task for when the picker resolves
    pendingDelegateTask.current = task
  }, [])

  const pendingDelegateTask = useRef<Task | null>(null)

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
      <div className="grid grid-cols-3 gap-6 min-h-[70vh]">
        {/* Queue */}
        <DroppableColumn
          id="queue"
          title="Queue"
          count={queued.length}
          isOver={overColumn === 'queue' && getColumn(activeId!) !== 'queue'}
        >
          <SortableContext items={queued.map((t) => t.id)} strategy={verticalListSortingStrategy}>
            {queued.length === 0 ? (
              <EmptyColumn>Queue is empty</EmptyColumn>
            ) : (
              queued.map((task) => (
                <SortableTaskCard key={task.id} task={task} />
              ))
            )}
          </SortableContext>
        </DroppableColumn>

        {/* In Progress */}
        <DroppableColumn
          id="in_progress"
          title="In Progress"
          count={inProgress.length}
          isOver={overColumn === 'in_progress' && getColumn(activeId!) !== 'in_progress'}
          headerRight={
            <div className="flex gap-1">
              {(['all', 'user', 'agent'] as const).map((f) => (
                <button
                  key={f}
                  onClick={() => setFilter(f)}
                  className={`text-[11px] px-2 py-0.5 rounded-full transition-colors ${
                    filter === f
                      ? 'bg-accent-soft text-accent'
                      : 'text-text-tertiary hover:text-text-secondary'
                  }`}
                >
                  {f === 'all' ? 'All' : f === 'user' ? 'You' : 'Agent'}
                </button>
              ))}
            </div>
          }
        >
          <SortableContext items={inProgress.filter((i) => !(i.type === 'agent' && agentRuns[i.task.id])).map((i) => i.task.id)} strategy={verticalListSortingStrategy}>
            {inProgress.length === 0 ? (
              <EmptyColumn>Nothing in progress</EmptyColumn>
            ) : (
              inProgress.map(({ task, type }) =>
                type === 'agent' && agentRuns[task.id] ? (
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

        {/* Done */}
        <DroppableColumn
          id="done"
          title="Done"
          count={done.length}
          isOver={overColumn === 'done' && getColumn(activeId!) !== 'done'}
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

      {/* Drag overlay — floating card that follows the cursor */}
      <DragOverlay dropAnimation={null}>
        {activeTask && (
          <div className="w-[calc((100vw-6rem)/3-1.5rem)]">
            <TaskCard task={activeTask} isDragging />
          </div>
        )}
      </DragOverlay>

      {/* Claim vs Delegate popup */}
      {pendingInProgress && (
        <>
          <div className="fixed inset-0 bg-black/10 backdrop-blur-sm z-40" onClick={() => setPendingInProgress(null)} />
          <div className="fixed left-1/2 top-1/2 -translate-x-1/2 -translate-y-1/2 z-50 bg-surface-raised border border-border-glass rounded-2xl shadow-xl shadow-black/10 p-5 w-[300px]">
            <h3 className="text-[14px] font-semibold text-text-primary mb-1">Move to In Progress</h3>
            <p className="text-[12px] text-text-tertiary mb-4 leading-relaxed">
              How do you want to handle <span className="text-text-secondary font-medium">{pendingInProgress.title.length > 40 ? pendingInProgress.title.slice(0, 37) + '...' : pendingInProgress.title}</span>?
            </p>
            <div className="flex gap-2">
              <button
                onClick={() => handleClaimFromPopup(pendingInProgress)}
                className="flex-1 text-[13px] font-semibold text-claim bg-claim/10 hover:bg-claim/20 px-4 py-2 rounded-xl transition-colors"
              >
                Claim
              </button>
              <button
                onClick={() => handleDelegateFromPopup(pendingInProgress)}
                className="flex-1 text-[13px] font-semibold text-white bg-accent hover:bg-accent/90 px-4 py-2 rounded-xl transition-colors"
              >
                Delegate
              </button>
            </div>
          </div>
        </>
      )}

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

function SortableTaskCard({ task }: { task: Task }) {
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
  headerRight,
  children,
}: {
  id: string
  title: string
  count: number
  isOver: boolean
  headerRight?: React.ReactNode
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
        {headerRight}
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
