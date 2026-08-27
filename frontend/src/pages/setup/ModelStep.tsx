// The model steps' bodies — flush, leading with a conversational line over the
// shared ModelPicker. Selection just records the value; the step's Continue
// advances, and the save gate the wizard and Settings both run is what decides
// whether it lands. The team step picks the team's default; the background-jobs
// step picks what the headless jobs run on.

import ModelPicker from '../../components/ModelPicker'
import { useModelCatalog } from '../../hooks/useModelCatalog'
import type { StepContext } from './types'

// TeamModelStep body — the team's default delegation model, drawn from the
// org's model catalog. The stored value is the catalog key; the picker shows
// the display name, what it costs, and whether this org's credentials have been
// shown to run it. Until the read lands the picker renders nothing to pick
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
      <ModelPicker
        value={state.team.default_model}
        onChange={choose}
        models={models}
        loaded={loaded}
        ariaLabel="Team default model"
      />
    </div>
  )
}

// OrgBackgroundJobsModelStep body — the model the headless background jobs run
// on. Drawn from the same catalog as every other model choice, with no further
// narrowing: the delegation picker's gates (tool support, a big enough context
// window) are about running an agent, and these jobs are toolless single-turn
// calls that fit in any window the catalog offers.
//
// `allowOff` is passed by the render site, not carried on StepContext, because
// the two surfaces want opposite things from the same body. The wizard step is
// mandatory — it blocks until a model is chosen, which is the whole reason a
// fresh workspace cannot end up with jobs that silently never run — so it
// offers no way out. Settings passes it where turning the jobs off is a
// decision an admin is allowed to make later.
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
        <h2 className="text-[19px] font-medium tracking-tight text-ink-1">Background jobs model</h2>
        <p className="text-[13px] leading-relaxed text-ink-3">
          Scoring, project classification and repo profiling all run on this one model.
          They&rsquo;re short, toolless calls, so background jobs can use any model — a cheap one is
          usually the right answer. Without a model picked, these jobs don&rsquo;t run.
        </p>
      </div>
      <ModelPicker
        value={state.org.background_jobs_model}
        onChange={choose}
        models={models}
        loaded={loaded}
        ariaLabel="Background jobs model"
      />
      {/* The off switch sits BESIDE the picker rather than in it. An empty
          option in the list would read as a model, and the list's job is to
          offer models. */}
      {allowOff &&
        (off ? (
          <p className="text-[12px] text-ink-3">
            Background jobs are off. Pick a model to turn them back on.
          </p>
        ) : (
          <button
            type="button"
            onClick={() => choose('')}
            className="text-[11px] text-alarm transition-colors hover:text-alarm/80"
          >
            Turn background jobs off
          </button>
        ))}
    </div>
  )
}
