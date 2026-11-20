import { useState, useEffect, useCallback } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import { X } from 'lucide-react'
import PredicateEditor from './PredicateEditor'
import EventTypeOptions from './EventTypeOptions'
import TeamPicker from './TeamPicker'
import type { NotifyHandler, EventType } from '../types'
import { toast } from './Toast/toastStore'
import { useTeams, pickerDefault, noteWrittenTeam } from '../hooks/useTeams'
import { useEventSources } from '../hooks/useEventSources'
import { apiJSON, httpErrorMessage } from '../lib/apiClient'
import {
  createNotifyRule,
  deleteNotifyRule,
  updateNotifyRule,
  type NotifyRulePatch,
} from '../lib/notifyRules'

// The sources a notify rule may be written on: the ones whose tracked-set gate
// can say for certain that a team tracks an entity. The create route refuses
// the rest.
const NOTIFY_SOURCES = ['github', 'jira', 'linear']

interface NotifyRuleEditorProps {
  open: boolean
  rule: NotifyHandler | null // null = create mode
  onClose: () => void
  onSaved: () => void
  onDeleted?: () => void
}

export default function NotifyRuleEditor({
  open,
  rule,
  onClose,
  onSaved,
  onDeleted,
}: NotifyRuleEditorProps) {
  const isEdit = rule !== null
  const [eventType, setEventType] = useState('')
  const [name, setName] = useState('')
  const [predicate, setPredicate] = useState<Record<string, unknown>>({})
  const [originalPredicateJSON, setOriginalPredicateJSON] = useState<string | null>(null)
  const [eventTypes, setEventTypes] = useState<EventType[]>([])
  const [saving, setSaving] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const [error, setError] = useState('')
  const { canProduce } = useEventSources()

  const { teams, lastActingTeamId, loaded: teamsLoaded } = useTeams()
  const [team, setTeam] = useState('')
  useEffect(() => {
    if (!open || isEdit || !teamsLoaded) return
    setTeam(pickerDefault(teams, lastActingTeamId))
  }, [open, isEdit, teamsLoaded, teams, lastActingTeamId])
  // The team the rule is (or will be) on. '' is the sole team, which the
  // server resolves.
  const ruleTeam = rule ? rule.team_id : team || (teams.length === 1 ? teams[0].id : '')

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

  useEffect(() => {
    if (!open) return
    setEventType(rule?.event_type ?? '')
    setName(rule?.name ?? '')
    setOriginalPredicateJSON(rule?.scope_predicate_json ?? null)
    let parsed: Record<string, unknown> = {}
    if (rule?.scope_predicate_json) {
      try {
        parsed = JSON.parse(rule.scope_predicate_json)
      } catch {
        parsed = {}
      }
    }
    setPredicate(parsed)
    setError('')
    setSaving(false)
    setDeleting(false)
  }, [open, rule])

  const handleSave = useCallback(async () => {
    if (!eventType || !name.trim()) {
      setError(!eventType ? 'Event type is required' : 'Name is required')
      return
    }
    setSaving(true)
    setError('')
    try {
      const hasPredicate = Object.keys(predicate).length > 0
      const predicateJSON = hasPredicate ? JSON.stringify(predicate) : null
      if (isEdit && rule) {
        const patch: NotifyRulePatch = {}
        if (name.trim() !== rule.name) patch.name = name.trim()
        if (predicateJSON !== originalPredicateJSON) {
          patch.scope_predicate = hasPredicate ? predicate : null
        }
        if (Object.keys(patch).length > 0) await updateNotifyRule(rule.id, patch)
      } else {
        await createNotifyRule({
          event_type: eventType,
          name: name.trim(),
          team_id: team,
          ...(hasPredicate ? { scope_predicate: predicate } : {}),
        })
        if (team) noteWrittenTeam(team)
      }
      onSaved()
    } catch (err: unknown) {
      const msg = httpErrorMessage(
        err,
        isEdit ? 'Could not update the notify rule.' : 'Could not create the notify rule.',
      )
      setError(msg)
      toast.error(msg)
    } finally {
      setSaving(false)
    }
  }, [eventType, name, predicate, team, isEdit, rule, originalPredicateJSON, onSaved])

  const handleDelete = useCallback(async () => {
    if (!rule) return
    setDeleting(true)
    setError('')
    try {
      await deleteNotifyRule(rule.id)
      onDeleted?.()
      onClose()
    } catch (err: unknown) {
      const msg = httpErrorMessage(err, 'Could not delete the notify rule.')
      setError(msg)
      toast.error(msg)
    } finally {
      setDeleting(false)
    }
  }, [rule, onDeleted, onClose])

  const fieldClass =
    'w-full px-3 py-2 rounded-lg border border-line-1 bg-raised text-body text-ink-1 placeholder:text-ink-3 focus:outline-none focus:border-warm/40 focus:ring-1 focus:ring-warm/20 transition-colors disabled:opacity-50 disabled:cursor-not-allowed'

  return (
    <AnimatePresence>
      {open && (
        <>
          <motion.div
            className="fixed inset-0 bg-scrim backdrop-blur-sm z-[60]"
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            onClick={onClose}
          />
          <motion.div
            className="fixed inset-0 z-[60] flex items-center justify-center pointer-events-none"
            initial={{ opacity: 0, scale: 0.95 }}
            animate={{ opacity: 1, scale: 1 }}
            exit={{ opacity: 0, scale: 0.95 }}
            transition={{ duration: 0.15 }}
          >
            <div className="pointer-events-auto bg-raised/95 backdrop-blur-2xl border border-line-1 rounded-2xl shadow-float shadow-black/10 w-[520px] max-h-[85vh] flex flex-col overflow-hidden">
              <div className="px-6 pt-5 pb-3 flex items-center justify-between shrink-0">
                <h2 className="text-column font-semibold text-ink-1">
                  {isEdit ? 'Edit Notify Rule' : 'New Notify Rule'}
                </h2>
                <button onClick={onClose} className="text-ink-3 hover:text-ink-2 transition-colors">
                  <X size={18} />
                </button>
              </div>

              <div className="flex-1 overflow-y-auto px-6 py-4 min-h-0">
                <div className="min-w-0 space-y-5">
                  {!isEdit && <TeamPicker value={team} onChange={setTeam} label="Team" />}
                  <div>
                    <label
                      htmlFor="notify-event-type"
                      className="block text-ui font-medium text-ink-2 mb-1.5"
                    >
                      Event type
                    </label>
                    <select
                      id="notify-event-type"
                      value={eventType}
                      onChange={(e) => {
                        setEventType(e.target.value)
                        setPredicate({})
                      }}
                      disabled={isEdit}
                      className={fieldClass}
                    >
                      <option value="">Select event type…</option>
                      <EventTypeOptions
                        eventTypes={eventTypes.filter(
                          (et) =>
                            et.id === eventType ||
                            (NOTIFY_SOURCES.includes(et.source) && canProduce(et.source)),
                        )}
                      />
                    </select>
                    <p className="text-reported text-ink-3 mt-1.5">
                      Applies to events from now on. Nothing that already happened sends a
                      notification.
                    </p>
                  </div>

                  <div>
                    <label
                      htmlFor="notify-name"
                      className="block text-ui font-medium text-ink-2 mb-1.5"
                    >
                      Name
                    </label>
                    <input
                      id="notify-name"
                      type="text"
                      value={name}
                      onChange={(e) => setName(e.target.value)}
                      placeholder="e.g. CI failed on a PR"
                      className={fieldClass}
                    />
                  </div>

                  {eventType && (
                    <div>
                      <span className="block text-ui font-medium text-ink-2 mb-2">
                        When (predicate filter)
                      </span>
                      <div className="bg-tint-2 rounded-lg border border-line-1 p-3">
                        <PredicateEditor
                          eventType={eventType}
                          teamId={ruleTeam}
                          value={predicate}
                          onChange={setPredicate}
                        />
                      </div>
                      <p className="text-reported text-ink-3 mt-1.5">
                        Leave all fields on &ldquo;Any&rdquo; to tell the team about every event of
                        this type on what it tracks.
                      </p>
                    </div>
                  )}
                </div>
              </div>

              <div className="px-6 py-4 border-t border-line-1 flex items-center shrink-0">
                {isEdit && (
                  <button
                    onClick={handleDelete}
                    disabled={deleting}
                    className="text-body font-medium text-alarm hover:text-alarm/80 transition-colors disabled:opacity-50"
                  >
                    {deleting ? 'Deleting…' : 'Delete'}
                  </button>
                )}
                <div className="ml-auto flex items-center gap-3">
                  {error && <span className="text-ui text-alarm mr-2">{error}</span>}
                  <button
                    onClick={onClose}
                    className="text-body font-medium text-ink-3 hover:text-ink-2 transition-colors"
                  >
                    Cancel
                  </button>
                  <button
                    onClick={handleSave}
                    disabled={saving || !eventType || !name.trim() || (!isEdit && !teamsLoaded)}
                    className="text-body font-semibold text-warm-ink bg-warm hover:bg-warm/90 disabled:opacity-50 disabled:cursor-not-allowed px-5 py-2 rounded-full transition-colors"
                  >
                    {saving ? 'Saving…' : isEdit ? 'Save' : 'Create'}
                  </button>
                </div>
              </div>
            </div>
          </motion.div>
        </>
      )}
    </AnimatePresence>
  )
}
