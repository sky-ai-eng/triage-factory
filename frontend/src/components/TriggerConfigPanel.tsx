import { useEffect, useState } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import * as Switch from '@radix-ui/react-switch'
import { Tooltip } from '../ui/tooltip/Tooltip'
import { Info } from 'lucide-react'
import type { TriggerHandler, EventType } from '../types'
import EventBadge from './EventBadge'
import PredicateEditor from './PredicateEditor'
import Slider from './Slider'
import { toast } from './Toast/toastStore'
import { apiFetch, apiJSON, httpErrorMessage } from '../lib/apiClient'
import { blueprintsBase, handlersBase } from '../lib/scope'
import { useEventSources } from '../hooks/useEventSources'
import { sourceCannotFireNote, sourceKindOf } from '../lib/eventSources'

interface TriggerConfigPanelProps {
  open: boolean
  trigger: TriggerHandler | null
  // When true the panel is read-only (TFAC-447): inputs are disabled and the
  // Save/Delete affordances are withheld, so a team viewer can inspect a
  // trigger's config without being offered writes that 403 server-side.
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
  const handlerBase = handlersBase()
  const { stateOf } = useEventSources()
  const [predicate, setPredicate] = useState<Record<string, unknown>>({})
  const [minAutonomy, setMinAutonomy] = useState(0)
  const [breakerThreshold, setBreakerThreshold] = useState(4)
  const [enabled, setEnabled] = useState(true)
  // applies_to_unowned — the explicit "watch" reach opt-in (TFAC-517). On a
  // trigger it grants orphan reach AND fires, so the whole orphan auto-delegation
  // can be configured here without a companion task rule.
  const [appliesToUnowned, setAppliesToUnowned] = useState(false)
  const [promptName, setPromptName] = useState('')
  const [saving, setSaving] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState(false)
  // Event-type catalog, fetched once per panel open (not per trigger). Used to
  // decide whether the applies_to_unowned toggle is meaningful for this event.
  const [eventTypes, setEventTypes] = useState<EventType[]>([])

  // Fetch the catalog once when the panel opens — it's session-static, so there's
  // no need to re-fetch on every trigger selection.
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

  // Why this trigger's source can no longer produce events, or null when it
  // can. Reads the row's own derived flag — the state is only consulted for
  // the prose — so the panel and the list agree by construction.
  const triggerSourceKind = sourceKindOf(trigger?.event_type ?? '')
  const sourceUnavailable =
    trigger?.source_available === false
      ? sourceCannotFireNote(triggerSourceKind, stateOf(triggerSourceKind))
      : null

  // Initialize state when trigger changes
  useEffect(() => {
    if (!trigger) return
    let cancelled = false

    setPredicate(parsePredicate(trigger.scope_predicate_json))
    setMinAutonomy(trigger.min_autonomy_suitability)
    setBreakerThreshold(trigger.breaker_threshold)
    setEnabled(trigger.enabled)
    setAppliesToUnowned(trigger.applies_to_unowned)
    setConfirmDelete(false)
    setPromptName('')

    // Resolve the bound blueprint's name for the badge — blueprints expose
    // only a list endpoint, so fetch the list and find by id.
    apiJSON<Array<{ id: string; name: string }>>(blueprintsBase())
      .then((list) => {
        const match = Array.isArray(list)
          ? list.find((b) => b.id === trigger.blueprint_id)
          : undefined
        if (!cancelled && match) setPromptName(match.name)
      })
      .catch(() => {})

    return () => {
      cancelled = true
    }
  }, [trigger])

  const handleToggle = async (checked: boolean) => {
    if (!trigger) return
    setEnabled(checked)
    try {
      // The PATCH answers with the handler resource, so the switch settles on
      // the stored value rather than on what we optimistically assumed.
      const saved = await apiJSON<TriggerHandler>(
        `${handlerBase}/${encodeURIComponent(trigger.id)}`,
        {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ enabled: checked }),
        },
      )
      setEnabled(saved.enabled)
      onRefresh?.()
    } catch (err) {
      setEnabled(!checked)
      toast.error(httpErrorMessage(err, 'Could not toggle the trigger.'))
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
        applies_to_unowned: appliesToUnowned,
      }
      await apiFetch(`${handlerBase}/${encodeURIComponent(trigger.id)}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      })
      onSaved()
    } catch (err) {
      toast.error(httpErrorMessage(err, 'Could not save the trigger.'))
    } finally {
      setSaving(false)
    }
  }

  const handleDelete = async () => {
    if (!trigger) return
    try {
      await apiFetch(`${handlerBase}/${encodeURIComponent(trigger.id)}`, { method: 'DELETE' })
      onDeleted()
    } catch (err) {
      toast.error(httpErrorMessage(err, 'Could not delete the trigger.'))
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
                <h2 className="text-body font-semibold text-ink-1">Trigger Config</h2>
                <div className="flex items-center gap-3">
                  <Switch.Root
                    checked={enabled}
                    onCheckedChange={handleToggle}
                    disabled={readOnly || sourceUnavailable !== null}
                    title={sourceUnavailable ?? undefined}
                    className="w-[34px] h-[18px] rounded-full bg-tint-3 data-[state=checked]:bg-warm transition-colors disabled:opacity-50"
                  >
                    <Switch.Thumb className="block w-[14px] h-[14px] bg-raised rounded-full shadow-float transition-transform translate-x-[2px] data-[state=checked]:translate-x-[18px]" />
                  </Switch.Root>
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
              {/* The trigger's source can no longer produce events. It is kept
                  and shown — deleting a configured trigger because a
                  credential was unbound is not the product's call — but arming
                  it is inert, so the toggle above is too. */}
              {sourceUnavailable && (
                <p className="text-reported text-ink-3 italic mt-2">
                  {sourceUnavailable} This trigger can&rsquo;t fire.
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
                  className="w-full px-3 py-2 rounded-lg border border-line-1 bg-raised text-body text-ink-1 focus:outline-none focus:border-warm/40 focus:ring-1 focus:ring-warm/20 transition-colors"
                />
              </Section>

              {/* Apply to unowned entities — the explicit "watch" reach opt-in
                  (TFAC-517). On a trigger this grants orphan reach AND fires, so
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
