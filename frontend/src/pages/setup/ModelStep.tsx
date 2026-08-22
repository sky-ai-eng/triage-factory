// The model steps' bodies — flush, leading with a conversational line over the
// shared ModelTierSelector ladder. Selection just records the value; the step's
// Continue advances (a model cap/default is a value worth pausing on, not a
// branch). The org step caps the workspace (No cap + the three tiers, with the
// ceiling/ghost treatment); the team step picks the team's default; the
// background-jobs step picks what the headless jobs run on.

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
        <h2 className="text-[19px] font-medium tracking-tight text-text-primary">
          This team&rsquo;s default model
        </h2>
        <p className="text-[13px] leading-relaxed text-text-tertiary">
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

// OrgBackgroundJobsModelStep body — the model the three headless background
// jobs run on. Drawn from the same catalog as every other model choice, with no
// further narrowing: the delegation picker's gates (tool support, a big enough
// context window) are about running an agent, and these jobs are toolless
// single-turn calls that fit in any window the catalog offers.
//
// `allowOff` is passed by the render site, not carried on StepContext, because
// the two surfaces want opposite things from the same body. The wizard step is
// mandatory — it blocks until a model is chosen, which is the whole reason a
// fresh workspace cannot end up with three jobs that silently never run — so it
// offers no way out. Settings passes it, because turning the jobs off is a
// decision an admin is allowed to make later, and the alternative is unbinding
// the Claude credential every other feature shares.
export function OrgBackgroundJobsModelStep({
  state,
  patch,
  allowOff,
}: StepContext & { allowOff?: boolean }) {
  const { models, loaded } = useModelCatalog()
  const choose = (model: string) => patch({ org: { ...state.org, background_jobs_model: model } })
  const off = state.org.background_jobs_model === ''
  return (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-text-primary">
          Background jobs model
        </h2>
        <p className="text-[13px] leading-relaxed text-text-tertiary">
          Scoring, project classification and repo profiling all run on this one model.
          They&rsquo;re short, toolless calls, so background jobs can use any model — a cheap one is
          usually the right answer. Without a model picked, these jobs don&rsquo;t run.
        </p>
      </div>
      {loaded && models.length === 0 ? (
        <p className="text-[13px] text-text-tertiary">No models are available to this workspace.</p>
      ) : (
        <ModelTierSelector
          value={state.org.background_jobs_model}
          onChange={choose}
          options={modelOptionsFrom(models)}
          ariaLabel="Background jobs model"
        />
      )}
      {/* The off switch sits BESIDE the selector rather than in it. An empty
          option would turn the control into its cap form — the ladder with the
          ghosted region and the "no cap" end — which reads as an ordering over
          models, the one claim the catalog declines to make. */}
      {allowOff &&
        (off ? (
          <p className="text-[12px] text-text-tertiary">
            Background jobs are off. Pick a model to turn them back on.
          </p>
        ) : (
          <button
            type="button"
            onClick={() => choose('')}
            className="text-[11px] text-dismiss transition-colors hover:text-dismiss/80"
          >
            Turn background jobs off
          </button>
        ))}
    </div>
  )
}
