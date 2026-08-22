// ModelTierSelector — two controls sharing one shape, chosen by the options.
//
// A CAP set includes the "" (no cap) end and renders as a capability *ladder*:
// the tiers ascend left→right (Haiku → Sonnet → Opus) and the selection reads as
// a ceiling — a warm fill runs along the track up to the chosen tier, everything
// above it ghosts out (the clamped region), and the "No cap" (∞) end lifts the
// ceiling entirely. That maps the data's actual shape, an ordered cap, onto the
// control.
//
// A PICK set — a model choice, drawn from the catalog — is a row of equal cells
// with the chosen one highlighted, and deliberately no fill, no ghosting, and no
// left-to-right meaning: the catalog asserts no ordering over models, so a
// control implying one would be inventing a claim about which is better.
//
// Controlled either way, value-typed as a plain string, radiogroup + roving
// tabIndex + arrow keys — the same a11y contract whichever surface composes it.

import { useRef } from 'react'
import { nextRadioIndex } from '../../lib/rovingRadio'
import type { ModelTierOption } from './modelTiers'

export type { ModelTierOption }

export default function ModelTierSelector({
  value,
  onChange,
  options,
  ariaLabel,
}: {
  value: string
  onChange: (value: string) => void
  options: ModelTierOption[]
  ariaLabel: string
}) {
  const btnRefs = useRef<(HTMLButtonElement | null)[]>([])

  // A CAP set carries the "" (no cap) end — that turns on the ceiling treatment.
  const isCap = options.some((o) => o.value === '')
  // Ladder rungs in ascending order (the no-cap end isn't one). Read only in
  // cap mode; a pick set never consults them.
  const tiers = options.filter((o) => o.value !== '')
  // Where the ceiling sits among the tiers: no cap ("") ⇒ above every tier. An
  // unrecognized tier (a server-side value the UI doesn't list yet) findIndex's
  // to -1; treat that as no cap too, so the ladder doesn't ghost every row and
  // misread as fully disabled.
  const knownTierIndex = tiers.findIndex((o) => o.value === value)
  const capTierIndex = value === '' || knownTierIndex < 0 ? tiers.length : knownTierIndex

  const selectedIndex = options.findIndex((o) => o.value === value)
  const tabbable = selectedIndex < 0 ? 0 : selectedIndex
  const onKeyDown = (e: React.KeyboardEvent) => {
    const next = nextRadioIndex(e.key, selectedIndex, options.length)
    if (next === null) return
    e.preventDefault()
    onChange(options[next].value)
    btnRefs.current[next]?.focus()
  }

  return (
    <div
      role="radiogroup"
      aria-label={ariaLabel}
      onKeyDown={onKeyDown}
      className="flex items-stretch"
    >
      {options.map((opt, i) => {
        const isNoCap = opt.value === ''
        const selected = opt.value === value
        const tierIndex = isNoCap ? -1 : tiers.findIndex((t) => t.value === opt.value)

        let aboveCap = false
        let filled = false
        if (isCap) {
          if (isNoCap) {
            // The end is "filled" only when it's the active choice (full ceiling).
            filled = value === ''
          } else {
            aboveCap = tierIndex > capTierIndex
            filled = tierIndex <= capTierIndex // fill up to (and including) the cap
          }
        } else {
          filled = selected
        }

        const textClass = selected
          ? 'text-accent'
          : aboveCap
            ? 'text-text-tertiary'
            : 'text-text-secondary'
        const railClass = selected
          ? 'bg-accent shadow-[0_0_10px_-1px_var(--color-accent)]'
          : filled
            ? 'bg-accent'
            : 'bg-[var(--color-border-subtle)]'

        return (
          <button
            key={opt.value || 'none'}
            ref={(el) => {
              btnRefs.current[i] = el
            }}
            type="button"
            role="radio"
            aria-checked={selected}
            tabIndex={i === tabbable ? 0 : -1}
            onClick={() => onChange(opt.value)}
            className={`group relative flex-1 px-3 pb-3 pt-2 text-left outline-none transition-opacity ${
              aboveCap ? 'opacity-50' : 'opacity-100'
            }`}
          >
            <span
              className={`flex items-center gap-1 text-[14px] font-medium transition-colors ${textClass}`}
            >
              {isNoCap && <span className="text-[15px] leading-none">∞</span>}
              {opt.label}
            </span>
            {opt.hint && (
              <span className="mt-0.5 block text-[11px] leading-snug text-text-tertiary">
                {opt.hint}
              </span>
            )}
            <span
              aria-hidden
              className={`absolute inset-x-0 bottom-0 h-[2px] transition-colors ${railClass}`}
            />
          </button>
        )
      })}
    </div>
  )
}
