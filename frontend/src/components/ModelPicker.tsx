// ModelPicker — the one model picker in the product. The team default, the
// org's background-jobs model, a prompt's per-step pin and both setup steps are
// all this component fed a different option set, so a person learns to read one
// control and every model choice looks the same.
//
// A vertical list of equal rows with the chosen one highlighted, and
// deliberately no ordering treatment — no fill running up to the selection, no
// ghosting above it, no rank. The catalog asserts no ordering over models, and a
// control implying one would be inventing a claim about which is better. The
// order is the order the read returned and means nothing else.
//
// Two semantics, unset and pinned, and no third: there is no "auto", no
// "recommended", and no sorting by anything but display order. `unsetOption` is
// how a surface that HAS an unset meaning (a prompt inherits its team's
// default) offers it; a surface where unset means nothing simply omits it.
//
// Everything a row shows comes from the row. A model that names no access path
// shows no provider, one whose cost the harness settles shows no price, and one
// with no availability field shows no badge — the read's absent fields are the
// only signal, and this component reads no mode.

// TODO(TFAC-907): raw Tailwind rather than a src/ui instrument. It mounts on
// four surfaces with different cutover dates, so its treatment is tracked as
// the component's rather than any one surface's. The SHAPE is settled: a
// single-select radiogroup, deliberately not the table.

import { useRef } from 'react'
import { nextRadioIndex } from '../lib/rovingRadio'
import {
  availabilityHelp,
  lacksPromptCaching,
  secondaryLine,
  selectable,
  PROMPT_CACHING_WARNING,
  type ModelCatalogEntry,
} from '../lib/models'
import ModelAvailabilityBadge from './ModelAvailabilityBadge'

export interface ModelPickerProps {
  /** The selected catalog key, or '' for no selection. */
  value: string
  onChange: (key: string) => void
  /** The options, in the order the read returned them. */
  models: ModelCatalogEntry[]
  /** False while the catalog read is still outstanding — the list renders
   *  nothing rather than an empty state, because "no models" and "not yet" are
   *  different answers and only one of them is worth telling somebody. */
  loaded: boolean
  ariaLabel: string
  /** The row for "no model of its own", where that means something. Always
   *  selectable and never badged: it names no model, so there is nothing about
   *  it to verify. */
  unsetOption?: { label: string; detail?: string }
  /** A read-only surface — the selection is shown and cannot be moved. Separate
   *  from a row's own unselectability, which is about that model rather than
   *  about this viewer's permission to choose. */
  readOnly?: boolean
}

export default function ModelPicker({
  value,
  onChange,
  models,
  loaded,
  ariaLabel,
  unsetOption,
  readOnly = false,
}: ModelPickerProps) {
  const btnRefs = useRef<(HTMLButtonElement | null)[]>([])

  // One row list, so selection, roving tabIndex and arrow keys are computed
  // once over the same indices the render walks. A row with no entry is the
  // unset one.
  const rows: { key: string; model?: ModelCatalogEntry }[] = [
    ...(unsetOption ? [{ key: '' }] : []),
    ...models.map((m) => ({ key: m.key, model: m })),
  ]
  const pickable = (i: number) => {
    if (readOnly) return false
    const m = rows[i].model
    return m === undefined || selectable(m)
  }

  const selectedIndex = rows.findIndex((r) => r.key === value)
  const tabbable = selectedIndex < 0 ? rows.findIndex((_, i) => pickable(i)) : selectedIndex
  const onKeyDown = (e: React.KeyboardEvent) => {
    const next = nextRadioIndex(e.key, selectedIndex, rows.length, pickable)
    if (next === null) return
    e.preventDefault()
    onChange(rows[next].key)
    btnRefs.current[next]?.focus()
  }

  if (!loaded) return null

  // Why the list offers nothing, when it offers nothing — and it NAMES THE FIX
  // rather than the state, since a person told only that something is wrong has
  // nowhere to go. The second case is a list whose every row refuses for one
  // reason: this organization has connected no credential that could run any of
  // them.
  const note =
    models.length === 0
      ? 'No models are enabled for this workspace. An org admin chooses which models teams may use.'
      : !models.some(selectable) && models.every((m) => m.availability === 'unconfigured')
        ? 'Connect a provider in Settings → Claude credentials to see the models you can run.'
        : ''

  // The note REPLACES the list only where there is genuinely nothing to pick. A
  // surface carrying an unset row always has something — inheriting the team
  // default is a real choice, and one that stays available precisely when no
  // model can be picked — so there the note sits beside the list instead.
  // Swallowing it would leave a prompt author unable to un-pin a model on the
  // day their org narrows its enable-set.
  if (note && !unsetOption) return <p className="text-[13px] text-ink-3">{note}</p>

  return (
    <div className="space-y-2">
      <div
        role="radiogroup"
        aria-label={ariaLabel}
        onKeyDown={onKeyDown}
        className="divide-y divide-[var(--color-line-1)] border-y border-line-1"
      >
        {rows.map((row, i) => {
          const m = row.model
          const isSelected = row.key === value
          const canPick = pickable(i)
          const help = m ? availabilityHelp(m) : ''
          const secondary = m ? secondaryLine(m) : (unsetOption?.detail ?? '')
          return (
            <button
              key={row.key || 'unset'}
              ref={(el) => {
                btnRefs.current[i] = el
              }}
              type="button"
              role="radio"
              aria-checked={isSelected}
              // Disabled rather than aria-disabled: a red or unconnected model is
              // not a choice with a caveat, it is a choice the save refuses, and
              // an activatable control that always fails is worse than one that
              // says so up front. It stays listed, and its help line says what to
              // do about it.
              disabled={!canPick}
              tabIndex={i === tabbable ? 0 : -1}
              onClick={() => onChange(row.key)}
              className={`flex w-full flex-col gap-1 px-3 py-2.5 text-left outline-none transition-colors focus-visible:outline focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-warm/50 ${
                isSelected ? 'bg-warm/[0.06]' : canPick ? 'hover:bg-tint-3' : 'opacity-60'
              }`}
            >
              <span className="flex items-center gap-2">
                <span
                  className={`text-body font-medium ${isSelected ? 'text-warm' : 'text-ink-1'}`}
                >
                  {m ? m.display_name : unsetOption?.label}
                </span>
                <ModelAvailabilityBadge state={m?.availability} help={help} />
              </span>
              {secondary && <span className="text-reported text-ink-3">{secondary}</span>}
              {m && lacksPromptCaching(m) && (
                <span className="text-reported text-warm">{PROMPT_CACHING_WARNING}</span>
              )}
              {/* The fix, spelled out, for the two states a person has to act on.
                A verified or untested row says nothing extra — its badge is the
                whole story, and the gate on save tells the untested one what
                happens next. */}
              {(m?.availability === 'red' || m?.availability === 'unconfigured') && help && (
                <span className="text-reported text-ink-3">{help}</span>
              )}
            </button>
          )
        })}
      </div>
      {note && <p className="text-[13px] text-ink-3">{note}</p>}
    </div>
  )
}
