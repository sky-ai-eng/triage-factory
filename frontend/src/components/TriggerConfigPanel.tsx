import { useEffect, useState } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import { Tooltip } from '../ui/tooltip/Tooltip'
import { Info } from 'lucide-react'
import type { Blueprint, EventHandler, EventHandlerMode, EventType } from '../types'
import EventBadge from './EventBadge'
import EventHandlerModeControl from './EventHandlerModeControl'
import PredicateEditor from './PredicateEditor'
import Slider from './Slider'
import { toast } from './Toast/toastStore'
import { apiJSON } from '../lib/apiClient'
import { blueprintsBase } from '../lib/scope'
import { deleteEventHandler, hasBlueprint, updateEventHandler } from '../lib/eventHandlers'
import { useEventSources } from '../hooks/useEventSources'
import { sourceCannotFireNote, sourceKindOf } from '../lib/eventSources'

interface TriggerConfigPanelProps {
  open: boolean
  trigger: EventHandler | null
  // When true the panel is read-only (TFAC-447): inputs are disabled and the
  // Save/Delete affordances are withheld, so a team viewer can inspect a
  // handler's config without being offered writes that 403 server-side.
  readOnly?: boolean
  onClose: () => void
  onSaved: () => void
  onDeleted: () => void
  onRefresh?: () => void
}

export default function TriggerConfigPanel({
  open,
  trigger,
  readOnly = false,
  onClose,
  onSaved,
  onDeleted,
  onRefresh,
}: TriggerConfigPanelProps) {
  const { stateOf } = useEventSources()
  const [predicate, setPredicate] = useState<Record<string, unknown>>({})
  const [minAutonomy, setMinAutonomy] = useState(0)
  const [breakerThreshold, setBreakerThreshold] = useState(4)
  const [mode, setMode] = useState<EventHandlerMode>('watch')
  // applies_to_unowned — the explicit reach opt-in. On a handler in automate it
  // grants orphan reach AND fires, so the whole orphan auto-delegation can be
  // configured here.
  const [appliesToUnowned, setAppliesToUnowned] = useState(false)
  const [promptName, setPromptName] = useState('')
  const [saving, setSaving] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState(false)
  // Event-type catalog, fetched once per panel open (not per handler). Used to
  // decide whether the applies_to_unowned toggle is meaningful for this event.
  const [eventTypes, setEventTypes] = useState<EventType[]>([])

  // Fetch the catalog once when the panel opens — it's session-static, so there's
  // no need to re-fetch on every handler selection.
  useEffect(() => {
    if (!open) return
    let cancelled = false
    apiJSON<EventType[]>('/api/event-types')
      .then((data) => {
        if (!cancelled && Array.isArray(data)) setEventTypes(data)
      })
      .catch(() => {})
    return () => {
      cancelled = true
    }
  }, [open])

  // Hide the watch toggle only when the catalog EXPLICITLY marks this event inert
  // (pool / requested-party). Fail-open: an unloaded/failed catalog leaves the
  // lookup undefined, so the toggle stays available on a valid owner-ladder event.
  const eventWatchInert =
    eventTypes.find((et) => et.id === trigger?.event_type)?.supports_watch === false

  // Why this handler's source can no longer produce events, or null when it
  // can. Reads the row's own derived flag — the state is only consulted for
  // the prose — so the panel and the list agree by construction.
  const triggerSourceKind = sourceKindOf(trigger?.event_type ?? '')
  const sourceUnavailable =
    trigger?.source_available === false
      ? sourceCannotFireNote(triggerSourceKind, stateOf(triggerSourceKind))
      : null

  // Initialize state when the handler changes
  useEffect(() => {
    if (!trigger) return
    let cancelled = false

    setPredicate(parsePredicate(trigger.scope_predicate_json))
    setMinAutonomy(trigger.min_autonomy_suitability)
    setBreakerThreshold(trigger.breaker_threshold)
    setMode(trigger.mode)
    setAppliesToUnowned(trigger.applies_to_unowned)
    setConfirmDelete(false)
    setPromptName('')

    // The bound blueprint's name is the badge. A failed read leaves it empty.
    if (trigger.blueprint_id) {
      apiJSON<Pick<Blueprint, 'name'>>(
        `${blueprintsBase()}/${encodeURIComponent(trigger.blueprint_id)}`,
      )
        .then((bp) => {
          if (!cancelled && bp?.name) setPromptName(bp.name)
        })
        .catch(() => {})
    }

    return () => {
      cancelled = true
    }
  }, [trigger])

  const handleModeChange = async (next: EventHandlerMode) => {
    if (!trigger) return
    const prev = mode
    setMode(next)
    try {
      // The PATCH answers with the handler resource, so the control settles on
      // the stored value rather than on what we optimistically assumed.
      const saved = await updateEventHandler(trigger.id, { mode: next })
      setMode(saved.mode)
      onRefresh?.()
    } catch (err) {
      setMode(prev)
      toast.failed(err, 'Could not change the mode.')
    }
  }

  const handleSave = async () => {
    if (!trigger) return
    setSaving(true)
    try {
      const body = {
        // An object, or null to clear it back to match-all.
        scope_predicate: Object.keys(predicate).length > 0 ? predicate : null,
        breaker_threshold: breakerThreshold,
        min_autonomy_suitability: minAutonomy,
        // Only where the control is shown: the route refuses true on an
        // event type the flag cannot widen.
        ...(eventWatchInert ? {} : { applies_to_unowned: appliesToUnowned }),
      }
      await updateEventHandler(trigger.id, body)
      onSaved()
    } catch (err) {
      toast.failed(err, 'Could not save the event handler.')
    } finally {
      setSaving(false)
    }
  }

  const handleDelete = async () => {
    if (!trigger) return
    try {
      await deleteEventHandler(trigger.id)
      onDeleted()
    } catch (err) {
      toast.failed(err, 'Could not delete the event handler.')
    }
  }

  return (
    <AnimatePresence>
      {open && trigger && (
        <>
          {/* Backdrop */}
          <motion.div
            initial={{ opacity: 0 }}
            animate={{ opacity: 1, pointerEvents: 'auto' as const }}
            exit={{ opacity: 0, pointerEvents: 'none' as const }}
            transition={{ duration: 0.2 }}
            className="fixed inset-0 bg-scrim backdrop-blur-sm z-40"
            onClick={onClose}
          />

          {/* Panel */}
          <motion.div
            initial={{ x: '100%', opacity: 0 }}
            animate={{ x: 0, opacity: 1 }}
            exit={{ x: '100%', opacity: 0 }}
            transition={{ type: 'spring', damping: 28, stiffness: 300 }}
            className="fixed top-20 right-4 bottom-4 w-[380px] z-50 bg-raised/95 backdrop-blur-xl border border-line-1 rounded-2xl shadow-float shadow-black/[0.08] flex flex-col overflow-hidden"
          >
            {/* Header */}
            <div className="px-5 pt-5 pb-3 border-b border-line-1 shrink-0">
              <div className="flex items-center justify-between mb-3">
                <h2 className="text-body font-semibold text-ink-1">Event handler</h2>
                <div className="flex items-center gap-3">
                  <button
                    onClick={onClose}
                    className="text-ink-3 hover:text-ink-2 transition-colors text-lg leading-none px-1"
                  >
                    &times;
                  </button>
                </div>
              </div>

              {/* Event type + prompt badges */}
              <div className="flex items-center gap-2 flex-wrap">
                <EventBadge eventType={trigger.event_type} compact />
                {promptName && (
                  <span className="text-reported text-ink-3 bg-tint-3 px-2 py-0.5 rounded-full truncate max-w-[180px]">
                    {promptName}
                  </span>
                )}
              </div>
              <div className="mt-3" title={sourceUnavailable ?? undefined}>
                <EventHandlerModeControl
                  mode={mode}
                  eventType={trigger.event_type}
                  hasBlueprint={hasBlueprint(trigger)}
                  onChange={handleModeChange}
                  disabled={readOnly || sourceUnavailable !== null}
                />
              </div>
              {/* The handler's source can no longer produce events. It is kept
                  and shown — deleting a configured handler because a
                  credential was unbound is not the product's call — but
                  turning it on is inert, so the mode control above is too. */}
              {sourceUnavailable && (
                <p className="text-reported text-ink-3 italic mt-2">
                  {sourceUnavailable} This handler can&rsquo;t fire.
                </p>
              )}
            </div>

            {/* Body — a disabled fieldset makes every native control read-only
                in one place when the viewer can't write (TFAC-447). */}
            <fieldset
              disabled={readOnly}
              className="flex-1 min-w-0 overflow-y-auto border-0 px-5 py-4 space-y-5 disabled:opacity-70"
            >
              {/* Predicate editor */}
              <Section
                label="Scope filter"
                description="Only auto-delegate when event metadata matches these conditions. Leave empty to match all events."
              >
                <PredicateEditor
                  eventType={trigger.event_type}
                  teamId={trigger.team_id}
                  value={predicate}
                  onChange={setPredicate}
                />
              </Section>

              <div className="border-t border-line-1" />

              {/* Autonomy threshold */}
              <Section
                label="Min autonomy suitability"
                description="0 = fire immediately on event. Higher values defer until AI scores the task above this threshold."
              >
                <div className="flex items-center gap-3">
                  <Slider
                    value={minAutonomy}
                    onChange={setMinAutonomy}
                    min={0}
                    max={1}
                    step={0.05}
                    label="Min autonomy suitability"
                  />
                  <span className="text-body font-medium text-ink-1 w-[36px] text-right tabular-nums">
                    {minAutonomy.toFixed(2)}
                  </span>
                </div>
              </Section>

              {/* Breaker threshold */}
              <Section
                label="Breaker threshold"
                description="Consecutive auto-delegation failures before pausing. A successful manual run resets the counter."
              >
                <input
                  type="number"
                  min={1}
                  value={breakerThreshold}
                  onChange={(e) => setBreakerThreshold(Math.max(1, Number(e.target.value)))}
                  className="w-full px-3 py-2 rounded-lg border border-line-1 bg-raised text-body text-ink-1 focus:border-warm/40 focus:ring-1 focus:ring-warm/20 transition-colors"
                />
              </Section>

              {/* Apply to unowned entities — the explicit reach opt-in. On a
                  handler in automate this grants orphan reach AND fires, so
                  the whole orphan auto-delegation is configured here. Off by
                  default; carries an eyes-open warning. Hidden for events the
                  catalog marks inert (pool / requested-party —
                  supports_watch=false). */}
              {!eventWatchInert && (
                <>
                  <div className="border-t border-line-1" />
                  <div>
                    <label className="flex items-start gap-2.5 cursor-pointer">
                      <input
                        type="checkbox"
                        checked={appliesToUnowned}
                        onChange={(e) => setAppliesToUnowned(e.target.checked)}
                        className="mt-0.5 h-4 w-4 shrink-0 rounded border-line-1 text-warm focus:ring-warm/30"
                      />
                      <span>
                        <span className="block text-ui font-medium text-ink-2">
                          Apply to entities outside this team
                        </span>
                        <span className="block text-reported text-ink-3 mt-0.5">
                          Also auto-delegate on entities no one on this team owns.
                        </span>
                      </span>
                    </label>
                    <AnimatePresence>
                      {appliesToUnowned && (
                        <motion.p
                          initial={{ opacity: 0, height: 0 }}
                          animate={{ opacity: 1, height: 'auto' }}
                          exit={{ opacity: 0, height: 0 }}
                          className="overflow-hidden text-reported leading-relaxed text-warm bg-warm/10 border border-warm/20 rounded-lg px-3 py-2 mt-2"
                        >
                          The bot will auto-delegate on matching PRs and issues that no one in your
                          Triage Factory org owns (e.g. dependabot or outside contributors), not
                          just your team&rsquo;s — expect more bot activity. It still won&rsquo;t
                          act on a PR or issue another member owns.
                        </motion.p>
                      )}
                    </AnimatePresence>
                  </div>
                </>
              )}
            </fieldset>

            {/* Footer */}
            <div className="px-5 py-4 border-t border-line-1 shrink-0 flex items-center justify-between">
              <div>
                {readOnly ? null : confirmDelete ? (
                  <div className="flex items-center gap-2">
                    <span className="text-ui text-ink-3">Delete?</span>
                    <button
                      onClick={handleDelete}
                      className="text-ui font-medium text-alarm hover:text-alarm/80 transition-colors"
                    >
                      Yes
                    </button>
                    <button
                      onClick={() => setConfirmDelete(false)}
                      className="text-ui font-medium text-ink-3 hover:text-ink-2 transition-colors"
                    >
                      No
                    </button>
                  </div>
                ) : (
                  <button
                    onClick={() => setConfirmDelete(true)}
                    className="text-ui font-medium text-alarm/60 hover:text-alarm transition-colors"
                  >
                    Delete
                  </button>
                )}
              </div>
              <div className="flex items-center gap-2">
                {readOnly ? (
                  <>
                    <span className="text-reported font-medium text-ink-3">View only</span>
                    <button
                      onClick={onClose}
                      className="text-ui font-medium text-warm-ink bg-warm hover:bg-warm/90 px-4 py-1.5 rounded-lg transition-colors"
                    >
                      Close
                    </button>
                  </>
                ) : (
                  <>
                    <button
                      onClick={onClose}
                      className="text-ui font-medium text-ink-3 hover:text-ink-2 px-3 py-1.5 transition-colors"
                    >
                      Cancel
                    </button>
                    <button
                      onClick={handleSave}
                      disabled={saving}
                      className="text-ui font-medium text-warm-ink bg-warm hover:bg-warm/90 disabled:opacity-50 px-4 py-1.5 rounded-lg transition-colors"
                    >
                      {saving ? 'Saving...' : 'Save'}
                    </button>
                  </>
                )}
              </div>
            </div>
          </motion.div>
        </>
      )}
    </AnimatePresence>
  )
}

function Section({
  label,
  description,
  children,
}: {
  label: string
  description: string
  children: React.ReactNode
}) {
  return (
    <div>
      <div className="flex items-center gap-1.5 mb-2">
        <span className="text-ui font-medium text-ink-2">{label}</span>
        <Tooltip content={description} wrap>
          <Info size={12} className="text-ink-3 cursor-help" />
        </Tooltip>
      </div>
      {children}
    </div>
  )
}

function parsePredicate(json: string | null): Record<string, unknown> {
  if (!json) return {}
  try {
    const parsed = JSON.parse(json)
    if (typeof parsed === 'object' && parsed !== null) return parsed
  } catch {
    // invalid JSON
  }
  return {}
}
