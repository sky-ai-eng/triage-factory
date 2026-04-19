import { useEffect, useState } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import * as Switch from '@radix-ui/react-switch'
import * as Tooltip from '@radix-ui/react-tooltip'
import { Info } from 'lucide-react'
import type { PromptTrigger } from '../types'
import EventBadge from './EventBadge'
import PredicateEditor from './PredicateEditor'
import Slider from './Slider'

interface TriggerConfigPanelProps {
  open: boolean
  trigger: PromptTrigger | null
  onClose: () => void
  onSaved: () => void
  onDeleted: () => void
  onRefresh?: () => void
}

export default function TriggerConfigPanel({
  open,
  trigger,
  onClose,
  onSaved,
  onDeleted,
  onRefresh,
}: TriggerConfigPanelProps) {
  const [predicate, setPredicate] = useState<Record<string, unknown>>({})
  const [minAutonomy, setMinAutonomy] = useState(0)
  const [breakerThreshold, setBreakerThreshold] = useState(4)
  const [cooldownSeconds, setCooldownSeconds] = useState(60)
  const [enabled, setEnabled] = useState(true)
  const [promptName, setPromptName] = useState('')
  const [saving, setSaving] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState(false)

  // Initialize state when trigger changes
  useEffect(() => {
    if (!trigger) return
    let cancelled = false

    setPredicate(parsePredicate(trigger.scope_predicate_json))
    setMinAutonomy(trigger.min_autonomy_suitability)
    setBreakerThreshold(trigger.breaker_threshold)
    setCooldownSeconds(trigger.cooldown_seconds)
    setEnabled(trigger.enabled)
    setConfirmDelete(false)
    setPromptName('')

    fetch(`/api/prompts/${encodeURIComponent(trigger.prompt_id)}`)
      .then((r) => (r.ok ? r.json() : null))
      .then((p) => {
        if (!cancelled && p) setPromptName(p.name)
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
      const res = await fetch(`/api/triggers/${encodeURIComponent(trigger.id)}/toggle`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ enabled: checked }),
      })
      if (!res.ok) {
        setEnabled(!checked)
        return
      }
      onRefresh?.()
    } catch {
      setEnabled(!checked)
    }
  }

  const handleSave = async () => {
    if (!trigger) return
    setSaving(true)
    try {
      const body = {
        scope_predicate_json: Object.keys(predicate).length > 0 ? JSON.stringify(predicate) : '',
        breaker_threshold: breakerThreshold,
        cooldown_seconds: cooldownSeconds,
        min_autonomy_suitability: minAutonomy,
      }
      const res = await fetch(`/api/triggers/${encodeURIComponent(trigger.id)}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      })
      if (res.ok) onSaved()
    } finally {
      setSaving(false)
    }
  }

  const handleDelete = async () => {
    if (!trigger) return
    try {
      const res = await fetch(`/api/triggers/${encodeURIComponent(trigger.id)}`, {
        method: 'DELETE',
      })
      if (res.ok) onDeleted()
    } catch {
      // ignore
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
            className="fixed inset-0 bg-black/10 backdrop-blur-sm z-40"
            onClick={onClose}
          />

          {/* Panel */}
          <motion.div
            initial={{ x: '100%', opacity: 0 }}
            animate={{ x: 0, opacity: 1 }}
            exit={{ x: '100%', opacity: 0 }}
            transition={{ type: 'spring', damping: 28, stiffness: 300 }}
            className="fixed top-20 right-4 bottom-4 w-[380px] z-50 bg-surface-raised/95 backdrop-blur-xl border border-border-glass rounded-2xl shadow-xl shadow-black/[0.08] flex flex-col overflow-hidden"
          >
            {/* Header */}
            <div className="px-5 pt-5 pb-3 border-b border-border-subtle shrink-0">
              <div className="flex items-center justify-between mb-3">
                <h2 className="text-[13px] font-semibold text-text-primary">Trigger Config</h2>
                <div className="flex items-center gap-3">
                  <Switch.Root
                    checked={enabled}
                    onCheckedChange={handleToggle}
                    className="w-[34px] h-[18px] rounded-full bg-black/[0.08] data-[state=checked]:bg-accent transition-colors"
                  >
                    <Switch.Thumb className="block w-[14px] h-[14px] bg-white rounded-full shadow-sm transition-transform translate-x-[2px] data-[state=checked]:translate-x-[18px]" />
                  </Switch.Root>
                  <button
                    onClick={onClose}
                    className="text-text-tertiary hover:text-text-secondary transition-colors text-lg leading-none px-1"
                  >
                    &times;
                  </button>
                </div>
              </div>

              {/* Event type + prompt badges */}
              <div className="flex items-center gap-2 flex-wrap">
                <EventBadge eventType={trigger.event_type} compact />
                {promptName && (
                  <span className="text-[11px] text-text-tertiary bg-black/[0.04] px-2 py-0.5 rounded-full truncate max-w-[180px]">
                    {promptName}
                  </span>
                )}
              </div>
            </div>

            {/* Body */}
            <Tooltip.Provider delayDuration={200}>
              <div className="flex-1 overflow-y-auto px-5 py-4 space-y-5">
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

                <div className="border-t border-border-subtle" />

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
                    />
                    <span className="text-[13px] font-medium text-text-primary w-[36px] text-right tabular-nums">
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
                    className="w-full px-3 py-2 rounded-lg border border-border-subtle bg-white/50 text-[13px] text-text-primary focus:outline-none focus:border-accent/40 focus:ring-1 focus:ring-accent/20 transition-colors"
                  />
                </Section>

                {/* Cooldown */}
                <Section
                  label="Cooldown (seconds)"
                  description="Minimum time between auto-fires on the same task. Does not gate the first fire on task creation."
                >
                  <input
                    type="number"
                    min={0}
                    value={cooldownSeconds}
                    onChange={(e) => setCooldownSeconds(Math.max(0, Number(e.target.value)))}
                    className="w-full px-3 py-2 rounded-lg border border-border-subtle bg-white/50 text-[13px] text-text-primary focus:outline-none focus:border-accent/40 focus:ring-1 focus:ring-accent/20 transition-colors"
                  />
                </Section>
              </div>
            </Tooltip.Provider>

            {/* Footer */}
            <div className="px-5 py-4 border-t border-border-subtle shrink-0 flex items-center justify-between">
              <div>
                {confirmDelete ? (
                  <div className="flex items-center gap-2">
                    <span className="text-[12px] text-text-tertiary">Delete?</span>
                    <button
                      onClick={handleDelete}
                      className="text-[12px] font-medium text-dismiss hover:text-dismiss/80 transition-colors"
                    >
                      Yes
                    </button>
                    <button
                      onClick={() => setConfirmDelete(false)}
                      className="text-[12px] font-medium text-text-tertiary hover:text-text-secondary transition-colors"
                    >
                      No
                    </button>
                  </div>
                ) : (
                  <button
                    onClick={() => setConfirmDelete(true)}
                    className="text-[12px] font-medium text-dismiss/60 hover:text-dismiss transition-colors"
                  >
                    Delete
                  </button>
                )}
              </div>
              <div className="flex items-center gap-2">
                <button
                  onClick={onClose}
                  className="text-[12px] font-medium text-text-tertiary hover:text-text-secondary px-3 py-1.5 transition-colors"
                >
                  Cancel
                </button>
                <button
                  onClick={handleSave}
                  disabled={saving}
                  className="text-[12px] font-medium text-white bg-accent hover:bg-accent/90 disabled:opacity-50 px-4 py-1.5 rounded-lg transition-colors"
                >
                  {saving ? 'Saving...' : 'Save'}
                </button>
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
        <span className="text-[12px] font-medium text-text-secondary">{label}</span>
        <Tooltip.Root>
          <Tooltip.Trigger asChild>
            <Info size={12} className="text-text-tertiary cursor-help" />
          </Tooltip.Trigger>
          <Tooltip.Portal>
            <Tooltip.Content
              sideOffset={5}
              className="max-w-[260px] px-3 py-2 rounded-lg bg-text-primary text-white text-[11px] leading-relaxed shadow-lg z-[100]"
            >
              {description}
              <Tooltip.Arrow className="fill-text-primary" />
            </Tooltip.Content>
          </Tooltip.Portal>
        </Tooltip.Root>
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
