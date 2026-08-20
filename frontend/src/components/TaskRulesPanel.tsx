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
import type { RuleHandler } from '../types'
import { toast } from './Toast/toastStore'
import { apiFetch, apiListAll, httpErrorMessage } from '../lib/apiClient'

interface TaskRulesPanelProps {
  open: boolean
  onClose: () => void
}

export default function TaskRulesPanel({ open, onClose }: TaskRulesPanelProps) {
  const [rules, setRules] = useState<RuleHandler[]>([])
  const [loading, setLoading] = useState(false)
  const [editingRule, setEditingRule] = useState<RuleHandler | null>(null)
  const [creating, setCreating] = useState(false)
  const [refreshKey, setRefreshKey] = useState(0)

  // distance:5 activation constraint means a drag only starts after 5px of
  // pointer movement. Normal toggle/button clicks are press-and-release with
  // negligible movement, so nested interactive controls (the enabled switch)
  // work fine without special handling despite listeners being on the whole row.
  const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 5 } }))

  // Fetch rules on open or after mutations. The synchronous setLoading
  // before fetch costs one extra render per panel-open — acceptable for
  // a settings panel that opens once per session — and the alternatives
  // (Suspense, data-fetching library) would require a broader refactor.
  useEffect(() => {
    if (!open) return
    let cancelled = false
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setLoading(true)

    // Walked to completion: the panel reorders rules by drag, and a reorder
    // write must name the caller's ENTIRE visible rule set — a partial list
    // would submit a partial order.
    apiListAll<RuleHandler>('/api/event-handlers/list', { kind: 'rule' })
      .then((loaded) => {
        if (!cancelled) {
          setRules(loaded)
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
  const toggleEnabled = useCallback(async (rule: RuleHandler) => {
    const prev = rule.enabled
    setRules((rs) => rs.map((r) => (r.id === rule.id ? { ...r, enabled: !prev } : r)))

    try {
      await apiFetch(`/api/event-handlers/${encodeURIComponent(rule.id)}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ enabled: !prev }),
      })
    } catch (err) {
      setRules((rs) => rs.map((r) => (r.id === rule.id ? { ...r, enabled: prev } : r)))
      toast.error(httpErrorMessage(err, 'Could not toggle the rule.'))
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
        // ids must name the caller's ENTIRE visible rule set, in the new
        // order — the server refuses a subset rather than applying half of it.
        await apiFetch('/api/event-handlers/reorder', {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ ids: reordered.map((r) => r.id) }),
        })
      } catch (err) {
        toast.error(httpErrorMessage(err, 'Could not reorder the rules.'))
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
            className="fixed top-20 right-4 bottom-4 z-50 w-[340px] bg-raised border border-line-1 rounded-2xl shadow-float shadow-black/[0.08] flex flex-col overflow-hidden"
            initial={{ x: '100%', opacity: 0 }}
            animate={{ x: 0, opacity: 1 }}
            exit={{ x: '100%', opacity: 0 }}
            transition={{ type: 'spring', damping: 28, stiffness: 300 }}
          >
            {/* Header */}
            <div className="px-5 pt-5 pb-3 flex items-center justify-between shrink-0 border-b border-line-1">
              <h2 className="text-column font-semibold text-ink-1">Task Rules</h2>
              <div className="flex items-center gap-2">
                <button
                  onClick={() => setCreating(true)}
                  className="flex items-center gap-1 text-ui font-semibold text-warm-ink bg-warm hover:bg-warm/90 px-3 py-1.5 rounded-full transition-colors"
                >
                  <Plus size={14} />
                  New Rule
                </button>
                <button onClick={onClose} className="text-ink-3 hover:text-ink-2 transition-colors">
                  <X size={18} />
                </button>
              </div>
            </div>

            {/* Body */}
            <div className="flex-1 overflow-y-auto p-3 space-y-1">
              {loading && rules.length === 0 && (
                <div className="space-y-2 p-2">
                  {[1, 2, 3].map((i) => (
                    <div key={i} className="h-16 rounded-xl bg-tint-2 animate-pulse" />
                  ))}
                </div>
              )}

              {!loading && rules.length === 0 && (
                <div className="text-center py-12 px-4">
                  <p className="text-body text-ink-3">
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
  rule: RuleHandler
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
      <div className="mt-3.5 shrink-0 text-ink-3/30 group-hover:text-ink-3/60 transition-colors">
        <GripVertical size={14} />
      </div>

      {/* Row content — click to edit. min-w-0 on every flex-growing
          ancestor so the truncate on the rule name actually fires; without
          it, a long name (e.g. "Jira issue decomposition resolved (now
          actionable)") pushes the row wider than the 340px panel and
          forces horizontal scroll. */}
      <button
        onClick={onEdit}
        className="flex-1 min-w-0 text-left px-3 py-3 rounded-xl hover:bg-tint-2 transition-colors"
      >
        <div className="flex items-start justify-between gap-3 min-w-0">
          <div className="min-w-0 flex-1">
            <div className="flex items-center gap-2 min-w-0">
              <span
                className={`text-body font-medium truncate min-w-0 ${
                  rule.enabled ? 'text-ink-1' : 'text-ink-3 line-through'
                }`}
              >
                {rule.name}
              </span>
              {rule.source === 'system' && (
                <span className="text-label font-medium text-ink-3 bg-tint-3 px-1.5 py-0.5 rounded shrink-0">
                  SYS
                </span>
              )}
            </div>
            <div className="mt-1">
              <EventBadge eventType={rule.event_type} compact />
            </div>
            {rule.scope_predicate_json && (
              <p className="text-reported text-ink-3 mt-1 truncate font-mono">
                {formatPredicate(rule.scope_predicate_json)}
              </p>
            )}
          </div>

          {/* Enabled toggle */}
          <div onClick={(e) => e.stopPropagation()} className="shrink-0 mt-0.5">
            <Switch.Root
              checked={rule.enabled}
              onCheckedChange={onToggle}
              className="relative w-8 h-[18px] rounded-full transition-colors data-[state=checked]:bg-warm data-[state=unchecked]:bg-line-2 cursor-pointer"
            >
              <Switch.Thumb className="block w-[14px] h-[14px] rounded-full bg-raised shadow transition-transform data-[state=checked]:translate-x-[14px] data-[state=unchecked]:translate-x-[2px]" />
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
