import { useState, useEffect, useCallback } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import { X, Plus } from 'lucide-react'
import * as Switch from '@radix-ui/react-switch'
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
    // Optimistic update.
    setRules((rs) => rs.map((r) => (r.id === rule.id ? { ...r, enabled: !prev } : r)))

    try {
      const res = await fetch(`/api/task-rules/${encodeURIComponent(rule.id)}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ enabled: !prev }),
      })
      if (!res.ok) throw new Error()
    } catch {
      // Revert on failure.
      setRules((rs) => rs.map((r) => (r.id === rule.id ? { ...r, enabled: prev } : r)))
    }
  }, [])

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

          {/* Drawer */}
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

              {rules.map((rule) => (
                <button
                  key={rule.id}
                  onClick={() => setEditingRule(rule)}
                  className="w-full text-left px-4 py-3 rounded-xl hover:bg-black/[0.03] transition-colors group"
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
                    <Switch.Root
                      checked={rule.enabled}
                      onCheckedChange={() => toggleEnabled(rule)}
                      onClick={(e) => e.stopPropagation()}
                      className="shrink-0 relative w-8 h-[18px] rounded-full transition-colors data-[state=checked]:bg-accent data-[state=unchecked]:bg-black/10 cursor-pointer"
                    >
                      <Switch.Thumb className="block w-[14px] h-[14px] rounded-full bg-white shadow transition-transform data-[state=checked]:translate-x-[14px] data-[state=unchecked]:translate-x-[2px]" />
                    </Switch.Root>
                  </div>
                </button>
              ))}
            </div>
          </motion.div>

          {/* Editor modal (renders above the drawer) */}
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
