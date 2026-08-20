import { useState, useEffect, useCallback } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import * as Tooltip from '@radix-ui/react-tooltip'
import { X } from 'lucide-react'
import PredicateEditor from './PredicateEditor'
import Slider from './Slider'
import TeamPicker from './TeamPicker'
import type { RuleHandler, EventType } from '../types'
import { toast } from './Toast/toastStore'
import { useTeams, pickerDefault, noteWrittenTeam } from '../hooks/useTeams'
import { handlersBase, rulesCreatePath } from '../lib/scope'
import { apiFetch, apiJSON, httpErrorMessage } from '../lib/apiClient'

interface TaskRuleEditorProps {
  open: boolean
  rule: RuleHandler | null // null = create mode
  prefillEventType?: string
  prefillPredicate?: string // JSON string, for forgiving banner pre-fill
  // When set (the single-team prompts page), a new rule is created under
  // this team and the per-modal TeamPicker is replaced by a read-only
  // label. Empty / undefined keeps the modal's own write picker — the
  // standalone path (TaskRulesPanel on Cards) passes nothing and is
  // unchanged.
  lockedTeamId?: string
  onClose: () => void
  onSaved: () => void
  onDeleted?: () => void
}

export default function TaskRuleEditor({
  open,
  rule,
  prefillEventType,
  prefillPredicate,
  lockedTeamId,
  onClose,
  onSaved,
  onDeleted,
}: TaskRuleEditorProps) {
  const isEdit = rule !== null
  const base = handlersBase()

  // Form state
  const [eventType, setEventType] = useState('')
  const [name, setName] = useState('')
  const [predicate, setPredicate] = useState<Record<string, unknown>>({})
  const [priority, setPriority] = useState(0.5)
  const [sortOrder, setSortOrder] = useState(0)
  const [enabled, setEnabled] = useState(true)
  // applies_to_unowned — the explicit "watch" scope opt-in (TFAC-517).
  const [appliesToUnowned, setAppliesToUnowned] = useState(false)

  // UI state
  const [eventTypes, setEventTypes] = useState<EventType[]>([])
  const [saving, setSaving] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const [error, setError] = useState('')

  // Acting team — create mode only; a rule's team is immutable.
  // TeamPicker renders only at ≥2 teams; below that `team` stays '' and
  // the server resolves the sole team. Seeded when the panel opens, once
  // teams have loaded; teamsLoaded also gates the create submit so a
  // multi-team user can't submit team_id:'' in the cold-load window.
  const { teams, lastActingTeamId, loaded: teamsLoaded } = useTeams()
  const [team, setTeam] = useState('')
  useEffect(() => {
    if (!open || isEdit || !teamsLoaded) return
    setTeam(pickerDefault(teams, lastActingTeamId))
  }, [open, isEdit, teamsLoaded, teams, lastActingTeamId])
  // When the page locks the team (single-team prompts page), create uses it
  // directly and the picker is hidden; otherwise the modal's own write
  // picker applies. lockedTeamId is non-empty only for a ≥2-team user, by
  // which point teams are loaded, so no cold-load gate is needed.
  const effectiveTeam = lockedTeamId || team

  // Show the applies_to_unowned ("watch") toggle UNLESS the catalog explicitly
  // marks this event inert (supports_watch === false — pool / requested-party).
  // Fail-open: if the catalog hasn't loaded yet or the fetch failed, the lookup
  // is undefined and we keep the toggle available rather than stranding the user
  // on a valid owner-ladder event.
  const eventWatchInert = eventTypes.find((et) => et.id === eventType)?.supports_watch === false

  // Track original predicate JSON for PATCH diff.
  const [originalPredicateJSON, setOriginalPredicateJSON] = useState<string | null>(null)

  // Fetch event types for the dropdown.
  useEffect(() => {
    if (!open) return
    let cancelled = false
    apiJSON<EventType[]>('/api/event-types')
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
      setAppliesToUnowned(rule.applies_to_unowned)
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
      setAppliesToUnowned(false)
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
      // scope_predicate is an object on the wire (null clears it). The read row
      // carries the same predicate as canonical JSON TEXT under
      // scope_predicate_json, which is what the edit-mode diff compares.
      const hasPredicate = Object.keys(predicate).length > 0
      const predicateJSON = hasPredicate ? JSON.stringify(predicate) : null

      if (isEdit && rule) {
        // PATCH — build body with only changed fields.
        const body: Record<string, unknown> = {}
        if (name !== rule.name) body.name = name
        if (priority !== rule.default_priority) body.default_priority = priority
        if (sortOrder !== rule.sort_order) body.sort_order = sortOrder
        if (enabled !== rule.enabled) body.enabled = enabled
        if (appliesToUnowned !== rule.applies_to_unowned) body.applies_to_unowned = appliesToUnowned

        // Predicate: compare serialised forms, send the object.
        if (predicateJSON !== originalPredicateJSON) {
          body.scope_predicate = hasPredicate ? predicate : null
        }

        if (Object.keys(body).length > 0) {
          await apiFetch(`${base}/${encodeURIComponent(rule.id)}`, {
            method: 'PATCH',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body),
          })
        }
      } else {
        // POST — create. The route names the kind, so the body carries only
        // rule fields; a trigger field here is rejected, not dropped.
        const body: Record<string, unknown> = {
          event_type: eventType,
          name: name.trim(),
          default_priority: priority,
          sort_order: sortOrder,
          enabled,
          applies_to_unowned: appliesToUnowned,
          team_id: effectiveTeam,
        }
        if (hasPredicate) {
          body.scope_predicate = predicate
        }

        await apiFetch(rulesCreatePath(), {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        })
        if (effectiveTeam) noteWrittenTeam(effectiveTeam)
      }

      onSaved()
    } catch (err: unknown) {
      const msg = httpErrorMessage(
        err,
        isEdit ? 'Could not update the rule.' : 'Could not create the rule.',
      )
      setError(msg)
      toast.error(msg)
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
    appliesToUnowned,
    effectiveTeam,
    base,
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
      await apiFetch(`${base}/${encodeURIComponent(rule.id)}`, { method: 'DELETE' })
      onDeleted?.()
      onClose()
    } catch (err: unknown) {
      const msg = httpErrorMessage(err, 'Could not delete the rule.')
      setError(msg)
      toast.error(msg)
    } finally {
      setDeleting(false)
    }
  }, [rule, base, onDeleted, onClose])

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
                  {/* Team (create mode only — a rule's team is immutable).
                      Locked to the page's active team on the single-team
                      prompts page; otherwise the modal's own write picker. */}
                  {!isEdit &&
                    (lockedTeamId ? (
                      <div className="text-[12px] text-text-tertiary">
                        Team:{' '}
                        <span className="font-medium text-text-secondary">
                          {teams.find((t) => t.id === lockedTeamId)?.name ?? 'current team'}
                        </span>
                      </div>
                    ) : (
                      <TeamPicker value={team} onChange={setTeam} label="Team" />
                    ))}
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
                        setAppliesToUnowned(false) // Reset watch flag — the new event may not support it.
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

                  {/* Apply to unowned entities — the explicit "watch" scope
                      opt-in (TFAC-517). Off by default; on it surfaces tasks for
                      entities outside the team (PRs/issues authored by anyone),
                      so it carries a deliberate eyes-open warning. Hidden for
                      event types the catalog marks inert (pool / requested-party
                      — supports_watch=false). */}
                  {!eventWatchInert && (
                    <div>
                      <label className="flex items-start gap-2.5 cursor-pointer">
                        <input
                          type="checkbox"
                          checked={appliesToUnowned}
                          onChange={(e) => setAppliesToUnowned(e.target.checked)}
                          className="mt-0.5 h-4 w-4 shrink-0 rounded border-border-subtle text-accent focus:ring-accent/30"
                        />
                        <span>
                          <span className="block text-[12px] font-medium text-text-secondary">
                            Apply to entities outside this team
                          </span>
                          <span className="block text-[11px] text-text-tertiary mt-0.5">
                            Surface matching tasks even when no one on this team owns the entity.
                          </span>
                        </span>
                      </label>
                      <AnimatePresence>
                        {appliesToUnowned && (
                          <motion.p
                            initial={{ opacity: 0, height: 0 }}
                            animate={{ opacity: 1, height: 'auto' }}
                            exit={{ opacity: 0, height: 0 }}
                            className="overflow-hidden text-[11px] leading-relaxed text-amber-700 bg-amber-500/10 border border-amber-500/20 rounded-lg px-3 py-2 mt-2"
                          >
                            This rule surfaces matching PRs and issues authored by anyone —
                            including people on other teams and outside contributors — so expect
                            significantly higher volume. If you also auto-delegate this event, the
                            bot acts only on the ones no one in your Triage Factory org owns (e.g.
                            dependabot or external contributors), never on a PR a member owns.
                          </motion.p>
                        )}
                      </AnimatePresence>
                    </div>
                  )}

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
                      disabled={
                        saving ||
                        !eventType ||
                        !name.trim() ||
                        (!isEdit && !lockedTeamId && !teamsLoaded)
                      }
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
