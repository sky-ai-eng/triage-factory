// The two model-tier steps' bodies — flush, leading with a conversational line
// over the shared ModelTierSelector ladder. Both are action-on-click
// (selfAdvancing): choosing a tier records it and advances, so there's no
// Continue. The org step caps the workspace (No cap + the three tiers, with the
// ceiling/ghost treatment); the team step picks the team's default (the three
// tiers).

import ModelTierSelector from '../settings/ModelTierSelector'
import { MODEL_CAP_OPTIONS, MODEL_TIER_OPTIONS } from '../settings/modelTiers'
import type { StepContext } from './types'

// OrgModelStep body — the workspace max-tier cap.
export function OrgModelStep({ state, patch, advance }: StepContext) {
  const choose = (tier: string) => {
    patch({ org: { ...state.org, max_llm_model_tier: tier } })
    advance?.()
  }
  return (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-text-primary">
          Cap the model tier
        </h2>
        <p className="text-[13px] leading-relaxed text-text-tertiary">
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

// TeamModelStep body — the team's default delegation model.
export function TeamModelStep({ state, patch, advance }: StepContext) {
  const choose = (tier: string) => {
    patch({ team: { ...state.team, default_model: tier } })
    advance?.()
  }
  return (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-text-primary">
          This team&rsquo;s default model
        </h2>
        <p className="text-[13px] leading-relaxed text-text-tertiary">
          The model this team delegates with by default — clamped down to the workspace cap if it
          exceeds it.
        </p>
      </div>
      <ModelTierSelector
        value={state.team.default_model}
        onChange={choose}
        options={MODEL_TIER_OPTIONS}
        ariaLabel="Team default model"
      />
    </div>
  )
}
