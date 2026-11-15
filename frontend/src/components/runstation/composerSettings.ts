import { useState } from 'react'
import type { Conversation, Task } from '../../types'
import type { ApiErrorItem } from '../../lib/apiClient'
import type { ModelCatalogEntry } from '../../lib/models'
import { useModelCatalog } from '../../hooks/useModelCatalog'
import { useTeamModelConfig } from '../../hooks/useTeamModelConfig'
import { effectiveModelKeys } from '../../pages/team/models'

// The model and effort the composer's next message names, and what the
// station knows about whether the send would take them. The server decides:
// it applies the conversation's next-message settings to a message naming
// none, refuses a model the team does not enable, and fixes a running
// blueprint step's settings. This mirrors those rules so the composer can say
// so before the send rather than after it.

/** A settings fault the send came back with: one the control shows itself. */
export function isSettingsFault(item: ApiErrorItem): boolean {
  return item.field === 'model' || item.field === 'effort'
}

export interface ComposerSettings {
  /** The model the next message names, or '' when none is chosen. */
  model: string
  /** The effort the next message names, or '' for none: the model takes no
   *  effort, or none has been picked and the server applies its own. */
  effort: string
  /** The efforts the selected model takes, lowest first. */
  levels: string[]
  /** The model a message naming none would run on, as the server read it. */
  startingModel: string
  /** The org's models narrowed to the team's enable-set, in catalog order. */
  options: ModelCatalogEntry[]
  /** Both reads behind `options` have landed. */
  optionsLoaded: boolean
  /** The selected model is known to be outside the team's enable-set. */
  unavailable: boolean
  /** A step of a running blueprint: its settings are its plan's, and the
   *  control only shows them. */
  locked: boolean
  /** Whether the composer may send with this selection. */
  canSend: boolean
  /** The settings faults the last send came back with. */
  errors: ApiErrorItem[]
  open: boolean
  setOpen: (open: boolean) => void
  pickModel: (key: string) => void
  pickEffort: (effort: string) => void
  /** Show a refused send's settings faults, opening the control on them. */
  showErrors: (items: ApiErrorItem[]) => void
  clearErrors: () => void
}

export function useComposerSettings(
  conversation: Conversation,
  task: Task | null,
): ComposerSettings {
  const catalog = useModelCatalog()
  const { config } = useTeamModelConfig(task?.team_id ?? '')
  // What the person picked, over the server's starting settings. Unpicked
  // follows the conversation read, so a refetch that lands a starting model
  // shows it; picked stays where the person left it, across sends.
  const [picked, setPicked] = useState<{ model?: string; effort?: string }>({})
  const [errors, setErrors] = useState<ApiErrorItem[]>([])
  const [open, setOpen] = useState(false)

  const startingModel = conversation.next_message_model ?? ''
  const startingEffort = conversation.next_message_effort ?? ''
  // The server answers blueprint_run_status only when it could read the run.
  // Unknown is no lock, and a send naming other settings is refused with the
  // field, which the control then shows.
  const locked = !!conversation.blueprint_run_id && conversation.blueprint_run_status === 'running'

  const enabled =
    catalog.loaded && config ? new Set(effectiveModelKeys(catalog.models, config)) : null
  const options = enabled ? catalog.models.filter((m) => enabled.has(m.key)) : []

  const model = locked ? startingModel : (picked.model ?? startingModel)
  const levels = catalog.all.find((m) => m.key === model)?.effort_levels ?? []
  const chosenEffort = locked ? startingEffort : (picked.effort ?? startingEffort)
  // An effort the model does not take is none: a switch to a model with fewer
  // levels drops it rather than sending a value the send refuses.
  const effort = levels.includes(chosenEffort) ? chosenEffort : ''
  // A step's model is not held to the team's set: its claim runs it on the
  // model its plan froze whatever the set says now.
  const unavailable = !locked && !!model && enabled !== null && !enabled.has(model)

  const dropErrors = (field: string) =>
    setErrors((prev) =>
      prev.some((e) => e.field === field) ? prev.filter((e) => e.field !== field) : prev,
    )

  return {
    model,
    effort,
    levels,
    startingModel,
    options,
    optionsLoaded: enabled !== null,
    unavailable,
    locked,
    canSend: !!model && !unavailable,
    errors,
    open,
    setOpen: (next) => setOpen(next && (!locked || errors.length > 0)),
    pickModel: (key) => {
      setPicked((prev) => ({ ...prev, model: key }))
      dropErrors('model')
    },
    pickEffort: (next) => {
      setPicked((prev) => ({ ...prev, effort: next }))
      dropErrors('effort')
    },
    showErrors: (items) => {
      setErrors(items)
      if (items.length > 0) setOpen(true)
    },
    clearErrors: () => setErrors((prev) => (prev.length === 0 ? prev : [])),
  }
}
