import { useState, useEffect, useCallback } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import { X, Plus, GripVertical } from 'lucide-react'
import * as Switch from '@radix-ui/react-switch'
import {
  DndContext,
  closestCenter,
  PointerSensor,
  useSensor,
  useSensors,
  type DragEndEvent,
} from '@dnd-kit/core'
import {
  SortableContext,
  verticalListSortingStrategy,
  useSortable,
  arrayMove,
} from '@dnd-kit/sortable'
import { CSS } from '@dnd-kit/utilities'
import EventBadge from './EventBadge'
import TaskRuleEditor from './TaskRuleEditor'
import type { TaskRule } from '../types'

interface TaskRulesPanelProps {
  open: boolean
  onClose: () => void
}

export default function TaskRulesPanel({ open, onClose }: TaskRulesPanelProps) {
  const [rules, setRules] = useState<TaskRule[]>([])
  const [loading, setLoading] = useState(false)
  const [editingRule, setEditingRule] = useState<TaskRule | null>(null)
  const [creating, setCreating] = useState(false)
  const [refreshKey, setRefreshKey] = useState(0)

  const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 5 } }))

  // Fetch rules on open or after mutations.
  useEffect(() => {
    if (!open) return
    let cancelled = false
    setLoading(true)

    fetch('/api/task-rules')
      .then((r) => r.json())
      .then((data) => {
        if (!cancelled) {
          setRules(data || [])
          setLoading(false)
        }
      })
      .catch(() => {
        if (!cancelled) setLoading(false)
      })

    return () => {
      cancelled = true
    }
  }, [open, refreshKey])

  const refresh = useCallback(() => setRefreshKey((k) => k + 1), [])

  // Inline enabled toggle — optimistic update + PATCH.
  const toggleEnabled = useCallback(async (rule: TaskRule) => {
    const prev = rule.enabled
    setRules((rs) => rs.map((r) => (r.id === rule.id ? { ...r, enabled: !prev } : r)))

    try {
      const res = await fetch(`/api/task-rules/${encodeURIComponent(rule.id)}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ enabled: !prev }),
      })
      if (!res.ok) throw new Error()
    } catch {
      setRules((rs) => rs.map((r) => (r.id === rule.id ? { ...r, enabled: prev } : r)))
    }
  }, [])

  // Drag-to-reorder.
  const handleDragEnd = useCallback(
    async (event: DragEndEvent) => {
      const { active, over } = event
      if (!over || active.id === over.id) return

      const oldIndex = rules.findIndex((r) => r.id === active.id)
      const newIndex = rules.findIndex((r) => r.id === over.id)
      const reordered = arrayMove(rules, oldIndex, newIndex)
      setRules(reordered) // Optimistic

      try {
        await fetch('/api/task-rules/reorder', {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(reordered.map((r) => r.id)),
        })
      } catch {
        refresh() // Revert on failure
      }
    },
    [rules, refresh],
  )

  const editorOpen = creating || editingRule !== null

  return (
    <AnimatePresence>
      {open && (
        <>
          {/* Invisible backdrop — click to close */}
          <motion.div
            className="fixed inset-0 z-40"
            initial={{ opacity: 0, pointerEvents: 'none' as const }}
            animate={{ opacity: 1, pointerEvents: 'auto' as const }}
            exit={{ opacity: 0, pointerEvents: 'none' as const }}
            onClick={onClose}
          />

          {/* Panel */}
          <motion.div
            className="fixed top-20 right-4 bottom-4 z-50 w-[340px] bg-surface-raised border border-border-glass rounded-2xl shadow-xl shadow-black/[0.08] flex flex-col overflow-hidden"
            initial={{ x: '100%', opacity: 0 }}
            animate={{ x: 0, opacity: 1 }}
            exit={{ x: '100%', opacity: 0 }}
            transition={{ type: 'spring', damping: 28, stiffness: 300 }}
          >
            {/* Header */}
            <div className="px-5 pt-5 pb-3 flex items-center justify-between shrink-0 border-b border-border-subtle">
              <h2 className="text-[15px] font-semibold text-text-primary">Task Rules</h2>
              <div className="flex items-center gap-2">
                <button
                  onClick={() => setCreating(true)}
                  className="flex items-center gap-1 text-[12px] font-semibold text-white bg-accent hover:bg-accent/90 px-3 py-1.5 rounded-full transition-colors"
                >
                  <Plus size={14} />
                  New Rule
                </button>
                <button
                  onClick={onClose}
                  className="text-text-tertiary hover:text-text-secondary transition-colors"
                >
                  <X size={18} />
                </button>
              </div>
            </div>

            {/* Body */}
            <div className="flex-1 overflow-y-auto p-3 space-y-1">
              {loading && rules.length === 0 && (
                <div className="space-y-2 p-2">
                  {[1, 2, 3].map((i) => (
                    <div key={i} className="h-16 rounded-xl bg-black/[0.03] animate-pulse" />
                  ))}
                </div>
              )}

              {!loading && rules.length === 0 && (
                <div className="text-center py-12 px-4">
                  <p className="text-[13px] text-text-tertiary">
                    No rules yet. Create one to control what shows up in your queue.
                  </p>
                </div>
              )}

              {rules.length > 0 && (
                <DndContext
                  sensors={sensors}
                  collisionDetection={closestCenter}
                  onDragEnd={handleDragEnd}
                >
                  <SortableContext
                    items={rules.map((r) => r.id)}
                    strategy={verticalListSortingStrategy}
                  >
                    {rules.map((rule) => (
                      <SortableRuleRow
                        key={rule.id}
                        rule={rule}
                        onEdit={() => setEditingRule(rule)}
                        onToggle={() => toggleEnabled(rule)}
                      />
                    ))}
                  </SortableContext>
                </DndContext>
              )}
            </div>
          </motion.div>

          {/* Editor modal (renders above the panel) */}
          <TaskRuleEditor
            open={editorOpen}
            rule={editingRule}
            onClose={() => {
              setEditingRule(null)
              setCreating(false)
            }}
            onSaved={() => {
              setEditingRule(null)
              setCreating(false)
              refresh()
            }}
            onDeleted={refresh}
          />
        </>
      )}
    </AnimatePresence>
  )
}

// --- Sortable rule row -----------------------------------------------------

function SortableRuleRow({
  rule,
  onEdit,
  onToggle,
}: {
  rule: TaskRule
  onEdit: () => void
  onToggle: () => void
}) {
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({
    id: rule.id,
  })

  const style = {
    transform: CSS.Transform.toString(transform),
    transition,
    opacity: isDragging ? 0.5 : 1,
    zIndex: isDragging ? 10 : undefined,
  }

  return (
    <div
      ref={setNodeRef}
      style={style}
      {...attributes}
      {...listeners}
      className="flex items-start gap-1 group cursor-grab active:cursor-grabbing"
    >
      {/* Drag affordance icon */}
      <div className="mt-3.5 shrink-0 text-text-tertiary/30 group-hover:text-text-tertiary/60 transition-colors">
        <GripVertical size={14} />
      </div>

      {/* Row content — click to edit */}
      <button
        onClick={onEdit}
        className="flex-1 text-left px-3 py-3 rounded-xl hover:bg-black/[0.03] transition-colors"
      >
        <div className="flex items-start justify-between gap-3">
          <div className="min-w-0 flex-1">
            <div className="flex items-center gap-2">
              <span
                className={`text-[13px] font-medium truncate ${
                  rule.enabled ? 'text-text-primary' : 'text-text-tertiary line-through'
                }`}
              >
                {rule.name}
              </span>
              {rule.source === 'system' && (
                <span className="text-[10px] font-medium text-text-tertiary bg-black/[0.04] px-1.5 py-0.5 rounded shrink-0">
                  SYS
                </span>
              )}
            </div>
            <div className="mt-1">
              <EventBadge eventType={rule.event_type} compact />
            </div>
            {rule.scope_predicate_json && (
              <p className="text-[11px] text-text-tertiary mt-1 truncate font-mono">
                {formatPredicate(rule.scope_predicate_json)}
              </p>
            )}
          </div>

          {/* Enabled toggle */}
          <div onClick={(e) => e.stopPropagation()} className="shrink-0 mt-0.5">
            <Switch.Root
              checked={rule.enabled}
              onCheckedChange={onToggle}
              className="relative w-8 h-[18px] rounded-full transition-colors data-[state=checked]:bg-accent data-[state=unchecked]:bg-black/10 cursor-pointer"
            >
              <Switch.Thumb className="block w-[14px] h-[14px] rounded-full bg-white shadow transition-transform data-[state=checked]:translate-x-[14px] data-[state=unchecked]:translate-x-[2px]" />
            </Switch.Root>
          </div>
        </div>
      </button>
    </div>
  )
}

/** Compact predicate display — shows just the field:value pairs. */
function formatPredicate(json: string): string {
  try {
    const obj = JSON.parse(json)
    return Object.entries(obj)
      .map(([k, v]) => `${k}: ${v}`)
      .join(', ')
  } catch {
    return json
  }
}
