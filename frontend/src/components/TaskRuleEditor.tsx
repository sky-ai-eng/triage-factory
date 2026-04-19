import { useState, useEffect, useCallback } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import * as Tooltip from '@radix-ui/react-tooltip'
import { X } from 'lucide-react'
import PredicateEditor from './PredicateEditor'
import Slider from './Slider'
import type { TaskRule, EventType } from '../types'

interface TaskRuleEditorProps {
  open: boolean
  rule: TaskRule | null // null = create mode
  prefillEventType?: string
  prefillPredicate?: string // JSON string, for forgiving banner pre-fill
  onClose: () => void
  onSaved: () => void
  onDeleted?: () => void
}

export default function TaskRuleEditor({
  open,
  rule,
  prefillEventType,
  prefillPredicate,
  onClose,
  onSaved,
  onDeleted,
}: TaskRuleEditorProps) {
  const isEdit = rule !== null

  // Form state
  const [eventType, setEventType] = useState('')
  const [name, setName] = useState('')
  const [predicate, setPredicate] = useState<Record<string, unknown>>({})
  const [priority, setPriority] = useState(0.5)
  const [sortOrder, setSortOrder] = useState(0)
  const [enabled, setEnabled] = useState(true)

  // UI state
  const [eventTypes, setEventTypes] = useState<EventType[]>([])
  const [saving, setSaving] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const [error, setError] = useState('')

  // Track original predicate JSON for PATCH diff.
  const [originalPredicateJSON, setOriginalPredicateJSON] = useState<string | null>(null)

  // Fetch event types for the dropdown.
  useEffect(() => {
    if (!open) return
    let cancelled = false
    fetch('/api/event-types')
      .then((r) => r.json())
      .then((data) => {
        if (!cancelled) setEventTypes(data)
      })
      .catch(() => {})
    return () => {
      cancelled = true
    }
  }, [open])

  // Populate form when opening.
  useEffect(() => {
    if (!open) return
    if (rule) {
      // Edit mode — populate from existing rule.
      setEventType(rule.event_type)
      setName(rule.name)
      setPriority(rule.default_priority)
      setSortOrder(rule.sort_order)
      setEnabled(rule.enabled)
      setOriginalPredicateJSON(rule.scope_predicate_json)
      if (rule.scope_predicate_json) {
        try {
          setPredicate(JSON.parse(rule.scope_predicate_json))
        } catch {
          setPredicate({})
        }
      } else {
        setPredicate({})
      }
    } else {
      // Create mode — reset or prefill.
      setEventType(prefillEventType ?? '')
      setName('')
      setPriority(0.5)
      setSortOrder(0)
      setEnabled(true)
      setOriginalPredicateJSON(null)
      if (prefillPredicate) {
        try {
          setPredicate(JSON.parse(prefillPredicate))
        } catch {
          setPredicate({})
        }
      } else {
        setPredicate({})
      }
    }
    setError('')
    setSaving(false)
    setDeleting(false)
  }, [open, rule, prefillEventType, prefillPredicate])

  const handleSave = useCallback(async () => {
    if (!eventType) {
      setError('Event type is required')
      return
    }
    if (!name.trim()) {
      setError('Name is required')
      return
    }

    setSaving(true)
    setError('')

    try {
      const predicateJSON = Object.keys(predicate).length > 0 ? JSON.stringify(predicate) : null

      if (isEdit && rule) {
        // PATCH — build body with only changed fields.
        const body: Record<string, unknown> = {}
        if (name !== rule.name) body.name = name
        if (priority !== rule.default_priority) body.default_priority = priority
        if (sortOrder !== rule.sort_order) body.sort_order = sortOrder
        if (enabled !== rule.enabled) body.enabled = enabled

        // Predicate: compare serialised forms.
        const currentJSON = predicateJSON
        if (currentJSON !== originalPredicateJSON) {
          // Explicit key: null clears, string sets, omitting leaves unchanged.
          body.scope_predicate_json = currentJSON
        }

        if (Object.keys(body).length > 0) {
          const res = await fetch(`/api/task-rules/${encodeURIComponent(rule.id)}`, {
            method: 'PATCH',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body),
          })
          if (!res.ok) {
            const err = await res.json()
            throw new Error(err.error || 'Failed to update rule')
          }
        }
      } else {
        // POST — create.
        const body: Record<string, unknown> = {
          event_type: eventType,
          name: name.trim(),
          default_priority: priority,
          sort_order: sortOrder,
          enabled,
        }
        if (predicateJSON) {
          body.scope_predicate_json = predicateJSON
        }

        const res = await fetch('/api/task-rules', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        })
        if (!res.ok) {
          const err = await res.json()
          throw new Error(err.error || 'Failed to create rule')
        }
      }

      onSaved()
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : 'Something went wrong')
    } finally {
      setSaving(false)
    }
  }, [
    eventType,
    name,
    predicate,
    priority,
    sortOrder,
    enabled,
    isEdit,
    rule,
    originalPredicateJSON,
    onSaved,
  ])

  const handleDelete = useCallback(async () => {
    if (!rule) return
    setDeleting(true)
    setError('')
    try {
      const res = await fetch(`/api/task-rules/${encodeURIComponent(rule.id)}`, {
        method: 'DELETE',
      })
      if (!res.ok) {
        const err = await res.json()
        throw new Error(err.error || 'Failed to delete rule')
      }
      onDeleted?.()
      onClose()
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : 'Something went wrong')
    } finally {
      setDeleting(false)
    }
  }, [rule, onDeleted, onClose])

  return (
    <AnimatePresence>
      {open && (
        <>
          {/* Backdrop */}
          <motion.div
            className="fixed inset-0 bg-black/20 backdrop-blur-sm z-[60]"
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            onClick={onClose}
          />

          {/* Panel */}
          <motion.div
            className="fixed inset-0 z-[60] flex items-center justify-center pointer-events-none"
            initial={{ opacity: 0, scale: 0.95 }}
            animate={{ opacity: 1, scale: 1 }}
            exit={{ opacity: 0, scale: 0.95 }}
            transition={{ duration: 0.15 }}
          >
            <Tooltip.Provider delayDuration={300}>
              <div className="pointer-events-auto bg-surface-raised/95 backdrop-blur-2xl border border-border-glass rounded-2xl shadow-2xl shadow-black/10 w-[520px] max-h-[85vh] flex flex-col overflow-hidden">
                {/* Header */}
                <div className="px-6 pt-5 pb-3 flex items-center justify-between shrink-0">
                  <h2 className="text-[15px] font-semibold text-text-primary">
                    {isEdit ? 'Edit Task Rule' : 'New Task Rule'}
                  </h2>
                  <button
                    onClick={onClose}
                    className="text-text-tertiary hover:text-text-secondary transition-colors"
                  >
                    <X size={18} />
                  </button>
                </div>

                {/* Body — scrollable */}
                <div className="flex-1 overflow-y-auto px-6 py-4 space-y-5 min-h-0">
                  {/* Event type */}
                  <div>
                    <label className="block text-[12px] font-medium text-text-secondary mb-1.5">
                      Event type
                    </label>
                    <select
                      value={eventType}
                      onChange={(e) => {
                        setEventType(e.target.value)
                        setPredicate({}) // Reset predicate when switching event types in create mode.
                      }}
                      disabled={isEdit}
                      className="w-full px-3 py-2 rounded-lg border border-border-subtle bg-white/50 text-[13px] text-text-primary focus:outline-none focus:border-accent/40 focus:ring-1 focus:ring-accent/20 transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
                    >
                      <option value="">Select event type…</option>
                      {eventTypes
                        .filter((et) => et.source !== 'system')
                        .map((et) => (
                          <option key={et.id} value={et.id}>
                            {et.label} ({et.id})
                          </option>
                        ))}
                    </select>
                  </div>

                  {/* Name */}
                  <div>
                    <label className="block text-[12px] font-medium text-text-secondary mb-1.5">
                      Name
                    </label>
                    <input
                      type="text"
                      value={name}
                      onChange={(e) => setName(e.target.value)}
                      placeholder="e.g. CI failures on my PRs"
                      className="w-full px-3 py-2 rounded-lg border border-border-subtle bg-white/50 text-[13px] text-text-primary placeholder:text-text-tertiary focus:outline-none focus:border-accent/40 focus:ring-1 focus:ring-accent/20 transition-colors"
                    />
                  </div>

                  {/* Predicate */}
                  {eventType && (
                    <div>
                      <label className="block text-[12px] font-medium text-text-secondary mb-2">
                        When (predicate filter)
                      </label>
                      <div className="bg-black/[0.02] rounded-lg border border-border-subtle p-3">
                        <PredicateEditor
                          eventType={eventType}
                          value={predicate}
                          onChange={setPredicate}
                        />
                      </div>
                      <p className="text-[11px] text-text-tertiary mt-1.5">
                        Leave all fields on &ldquo;Any&rdquo; to match every event of this type.
                      </p>
                    </div>
                  )}

                  {/* Priority */}
                  <div>
                    <label className="block text-[12px] font-medium text-text-secondary mb-1.5">
                      Default priority{' '}
                      <span className="text-text-tertiary font-normal">
                        ({priority.toFixed(2)})
                      </span>
                    </label>
                    <Slider
                      value={priority}
                      onChange={setPriority}
                      min={0}
                      max={1}
                      step={0.05}
                      label="Default priority"
                    />
                    <div className="flex justify-between text-[10px] text-text-tertiary mt-0.5">
                      <span>Low</span>
                      <span>High</span>
                    </div>
                  </div>

                  {/* Enabled toggle removed — use the inline toggle in the rules list instead */}
                </div>

                {/* Footer */}
                <div className="px-6 py-4 border-t border-border-subtle flex items-center shrink-0">
                  {/* Left: delete — only for user rules. System rules use the enabled toggle instead. */}
                  {isEdit && rule?.source !== 'system' && (
                    <button
                      onClick={handleDelete}
                      disabled={deleting}
                      className="text-[13px] font-medium text-dismiss hover:text-dismiss/80 transition-colors disabled:opacity-50"
                    >
                      {deleting ? 'Deleting…' : 'Delete'}
                    </button>
                  )}

                  {/* Right: cancel + save */}
                  <div className="ml-auto flex items-center gap-3">
                    {error && <span className="text-[12px] text-dismiss mr-2">{error}</span>}
                    <button
                      onClick={onClose}
                      className="text-[13px] font-medium text-text-tertiary hover:text-text-secondary transition-colors"
                    >
                      Cancel
                    </button>
                    <button
                      onClick={handleSave}
                      disabled={saving || !eventType || !name.trim()}
                      className="text-[13px] font-semibold text-white bg-accent hover:bg-accent/90 disabled:opacity-50 disabled:cursor-not-allowed px-5 py-2 rounded-full transition-colors"
                    >
                      {saving ? 'Saving…' : isEdit ? 'Save' : 'Create'}
                    </button>
                  </div>
                </div>
              </div>
            </Tooltip.Provider>
          </motion.div>
        </>
      )}
    </AnimatePresence>
  )
}
