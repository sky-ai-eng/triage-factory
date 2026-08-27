// The availability badge — one rendering of a model's stored verdict, shared by
// every surface that shows one (the picker's rows, the org's model list).
//
// It renders NOTHING for a row with no availability field, and that is the
// whole point rather than a guard: availability is a fact about a credential TF
// owns, so an org running on the host's environment has no subject for a
// verdict to be about. A badge there would be a claim about the machine the
// process happens to be on, invalidated by nothing anyone can observe.

import { Check, AlertTriangle, CircleDashed, Plug } from 'lucide-react'
import { availabilityLabel, type ModelAvailability } from '../lib/models'

const STYLE: Record<ModelAvailability, { chip: string; icon: typeof Check }> = {
  verified: { chip: 'border-line-1 text-ink-2', icon: Check },
  unverified: { chip: 'border-line-1 text-ink-3', icon: CircleDashed },
  red: { chip: 'border-alarm/40 text-alarm', icon: AlertTriangle },
  unconfigured: { chip: 'border-line-1 text-ink-3', icon: Plug },
}

export default function ModelAvailabilityBadge({
  state,
  // The actionable half — the provider's own refusal, or the fix. Carried as
  // the title so the badge names what to do about itself on hover, and read out
  // beside it by the surfaces with room for a line of prose.
  help,
}: {
  state?: ModelAvailability
  help?: string
}) {
  if (!state) return null
  const { chip, icon: Icon } = STYLE[state]
  return (
    <span
      title={help || undefined}
      className={`inline-flex shrink-0 items-center gap-1 rounded-full border px-2 py-0.5 text-reported ${chip}`}
    >
      <Icon size={11} aria-hidden />
      {availabilityLabel(state)}
    </span>
  )
}
