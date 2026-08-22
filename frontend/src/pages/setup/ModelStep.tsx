// The two model-tier steps' bodies — flush, leading with a conversational line
// over the shared ModelTierSelector ladder. Selection just records the value;
// the step's Continue advances (a model cap/default is a value worth pausing on,
// not a branch). The org step caps the workspace (No cap + the three tiers, with
// the ceiling/ghost treatment); the team step picks the team's default.

import ModelTierSelector from '../settings/ModelTierSelector'
import { MODEL_CAP_OPTIONS, modelOptionsFrom } from '../settings/modelTiers'
import { useModelCatalog } from '../../hooks/useModelCatalog'
import type { StepContext } from './types'

// OrgModelStep body — the workspace max-tier cap.
export function OrgModelStep({ state, patch }: StepContext) {
  const choose = (tier: string) => patch({ org: { ...state.org, max_llm_model_tier: tier } })
  return (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-ink-1">Cap the model tier</h2>
        <p className="text-body leading-relaxed text-ink-3">
          A hard ceiling for the whole workspace. A team default above the cap is clamped down to it
          — the team is told, but the cap wins.
        </p>
      </div>
      <ModelTierSelector
        value={state.org.max_llm_model_tier}
        onChange={choose}
        options={MODEL_CAP_OPTIONS}
        ariaLabel="Maximum model tier (workspace cap)"
      />
    </div>
  )
}

// TeamModelStep body — the team's default delegation model, drawn from the
// org's model catalog. The stored value is the catalog key; the picker shows
// the display name. Until the read lands the selector renders nothing to pick
// from, which is right: offering a guess would offer a value the save rejects.
export function TeamModelStep({ state, patch }: StepContext) {
  const { models, loaded } = useModelCatalog()
  const choose = (model: string) => patch({ team: { ...state.team, default_model: model } })
  return (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-ink-1">
          This team&rsquo;s default model
        </h2>
        <p className="text-body leading-relaxed text-ink-3">
          The model this team delegates with by default — every step that pins none runs on it.
        </p>
      </div>
      {loaded && models.length === 0 ? (
        <p className="text-[13px] text-text-tertiary">No models are available to this workspace.</p>
      ) : (
        <ModelTierSelector
          value={state.team.default_model}
          onChange={choose}
          options={modelOptionsFrom(models)}
          ariaLabel="Team default model"
        />
      )}
    </div>
  )
}
