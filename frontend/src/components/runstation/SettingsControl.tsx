import { useEffect, useRef, useState } from 'react'
import * as Popover from '@radix-ui/react-popover'
import { SlidersHorizontal } from 'lucide-react'
import type { Conversation } from '../../types'
import ModelPicker from '../ModelPicker'
import { modelDisplayName } from '../../hooks/useModelCatalog'
import { effortLabel } from '../../lib/models'
import { nextRadioIndex } from '../../lib/rovingRadio'
import { clockTime } from '../../lib/conversationStatus'
import type { ComposerSettings } from './composerSettings'

// SettingsControl — the composer's model and effort for the next message: a
// button naming them, left of the send key, and a popover holding the picker.
// The settings travel with each message rather than living on the
// conversation, so this is where a person changes them, mid-chat included.
export default function SettingsControl({
  settings,
  conversation,
  active,
}: {
  settings: ComposerSettings
  conversation: Conversation
  /** A turn is executing now, which is when a change has to say when it lands. */
  active: boolean
}) {
  const s = settings
  const name = s.model ? modelDisplayName(s.model) : ''
  const summary = !name
    ? 'Pick a model'
    : [name, s.effort ? effortLabel(s.effort) : '', s.unavailable ? 'unavailable' : '']
        .filter(Boolean)
        .join(' · ')

  return (
    <Popover.Root open={s.open} onOpenChange={s.setOpen}>
      <Popover.Trigger asChild>
        <button
          type="button"
          disabled={s.locked}
          aria-label={`Message settings: ${summary}`}
          title={
            s.locked
              ? 'This blueprint step runs on the model and effort its plan set.'
              : 'Model and effort for your next message'
          }
          className={`mb-0.5 inline-flex h-7 min-w-0 max-w-[16rem] shrink items-center gap-1.5 rounded-[4px] px-2 font-mono text-label transition-colors enabled:hover:bg-tint-3 enabled:hover:text-ink-1 disabled:cursor-default ${
            s.unavailable || !name ? 'text-warm' : 'text-ink-3'
          }`}
        >
          <SlidersHorizontal size={11} className="shrink-0" />
          <span className="truncate">{summary}</span>
        </button>
      </Popover.Trigger>
      <Popover.Portal>
        <Popover.Content
          side="top"
          align="end"
          sideOffset={8}
          className="z-[100] max-h-[420px] w-[320px] space-y-3 overflow-y-auto rounded-xl border border-line-1 bg-raised/95 p-3 shadow-float shadow-black/[0.08] backdrop-blur-2xl animate-in fade-in-0 zoom-in-95"
        >
          <SettingsBody settings={s} conversation={conversation} active={active} />
          <Popover.Arrow className="fill-raised" />
        </Popover.Content>
      </Popover.Portal>
    </Popover.Root>
  )
}

function SettingsBody({
  settings: s,
  conversation,
  active,
}: {
  settings: ComposerSettings
  conversation: Conversation
  active: boolean
}) {
  const warmUntil = useWarmUntil(conversation.cache_warm_until)
  // A switch off the current model while its cache is warm drops the cache:
  // the next call reads the whole conversation again, uncached. A
  // conversation that has not run has nothing cached to lose.
  const cacheWarning =
    !!warmUntil && conversation.has_run !== false && !!s.model && s.model !== s.startingModel
  const fieldErrors = (field: string) => s.errors.filter((e) => e.field === field)

  return (
    <>
      <section className="space-y-1.5">
        <SectionLabel>Model</SectionLabel>
        {s.unavailable && (
          <p className="text-reported text-warm">
            {modelDisplayName(s.model)} isn’t one of this team’s enabled models. Pick one to send.
          </p>
        )}
        <ModelPicker
          value={s.model}
          onChange={s.pickModel}
          models={s.options}
          loaded={s.optionsLoaded}
          ariaLabel="Model"
        />
        {cacheWarning && warmUntil && (
          <p className="text-reported text-warm">
            Switching models now re-reads the whole conversation at full input price. The current
            model’s cache is warm until {clockTime(warmUntil)}.
          </p>
        )}
        <FieldErrors items={fieldErrors('model')} />
      </section>

      {s.levels.length > 0 && (
        <section className="space-y-1.5">
          <SectionLabel>Effort</SectionLabel>
          <EffortGroup levels={s.levels} value={s.effort} onChange={s.pickEffort} />
          <FieldErrors items={fieldErrors('effort')} />
        </section>
      )}

      {active && (
        <p className="text-reported text-ink-3">
          {conversation.runtime === 'sdk'
            ? 'Applies from the next turn.'
            : 'Applies from the agent’s next model call.'}
        </p>
      )}
    </>
  )
}

function SectionLabel({ children }: { children: React.ReactNode }) {
  return (
    <div className="font-mono text-label-sm font-semibold uppercase tracking-[0.18em] text-ink-3/70">
      {children}
    </div>
  )
}

function FieldErrors({ items }: { items: { message: string }[] }) {
  if (items.length === 0) return null
  return (
    <div role="alert" className="space-y-1">
      {items.map((e, i) => (
        <p key={i} className="text-reported text-alarm">
          {e.message}
        </p>
      ))}
    </div>
  )
}

// EffortGroup — the model's effort levels as a radiogroup, with the same
// roving focus and arrow keys as the model picker above it. Nothing checked
// is a real state: no effort named, and the server applies the provider's.
function EffortGroup({
  levels,
  value,
  onChange,
}: {
  levels: string[]
  value: string
  onChange: (effort: string) => void
}) {
  const refs = useRef<(HTMLButtonElement | null)[]>([])
  const selected = levels.indexOf(value)
  const onKeyDown = (e: React.KeyboardEvent) => {
    const next = nextRadioIndex(e.key, selected, levels.length)
    if (next === null) return
    e.preventDefault()
    onChange(levels[next])
    refs.current[next]?.focus()
  }
  return (
    <div
      role="radiogroup"
      aria-label="Effort"
      onKeyDown={onKeyDown}
      className="flex flex-wrap gap-1"
    >
      {levels.map((level, i) => {
        const checked = level === value
        return (
          <button
            key={level}
            ref={(el) => {
              refs.current[i] = el
            }}
            type="button"
            role="radio"
            aria-checked={checked}
            tabIndex={i === (selected < 0 ? 0 : selected) ? 0 : -1}
            onClick={() => onChange(level)}
            className={`rounded-[4px] px-2 py-1 text-reported outline-none transition-colors focus-visible:outline focus-visible:outline-2 focus-visible:outline-warm/50 ${
              checked ? 'bg-warm/[0.10] text-warm' : 'text-ink-2 hover:bg-tint-3'
            }`}
          >
            {effortLabel(level)}
          </button>
        )
      })}
    </div>
  )
}

// useWarmUntil is the cache's cold time while it is still ahead, and null
// once it has passed. The clock starts when the popover opens, which mounts
// this, and moves once more at the cold time, so a warning about a warm cache
// never outlives it.
function useWarmUntil(iso: string | undefined): Date | null {
  const parsed = iso ? Date.parse(iso) : NaN
  const ms = Number.isNaN(parsed) ? null : parsed
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (ms === null) return
    const wait = ms - Date.now()
    if (wait <= 0) return
    const t = setTimeout(() => setNow(Date.now()), wait)
    return () => clearTimeout(t)
  }, [ms, now])
  return ms !== null && ms > now ? new Date(ms) : null
}
