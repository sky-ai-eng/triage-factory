import { useCallback, useEffect, useRef, useState } from 'react'
import { apiJSON } from '../lib/apiClient'

// The team's model choices, read and written on GET/PATCH
// /api/teams/{id}/settings. Only the two model fields of that resource are
// surfaced here: the rest of the settings row belongs to the settings stack,
// and a second full-form reader would be a second copy of its mapping.

/** The settings resource's model slice, under the wire's own field names —
 * the resource embeds the Go domain struct, so the casing is its. */
type TeamSettingsResource = {
  team_settings: {
    DefaultModel: string
    EnabledModels: string[] | null
  }
}

export type TeamModelConfig = {
  /** The team default — what an unpinned blueprint step falls back to. */
  defaultModel: string
  /** The STORED enable-set. Null is "no preference": the team inherits the
   * org's whole set, which is a different state from naming every model. */
  enabledModels: string[] | null
}

/** One save's fields. `enabled_models` replaces the whole set; the two travel
 * in one PATCH when a star lands on an unenabled row, because the server
 * validates the final state (default ∈ enabled) and does no implicit enable. */
export type TeamModelPatch = {
  ai_model?: string
  enabled_models?: string[]
}

function slice(resource: TeamSettingsResource): TeamModelConfig {
  return {
    defaultModel: resource.team_settings.DefaultModel,
    enabledModels: resource.team_settings.EnabledModels,
  }
}

/**
 * useTeamModelConfig reads the team's model slice and saves changes to it.
 *
 * `config` is null until the read lands — the panel renders nothing rather
 * than a guessed state. `save` applies the patch optimistically (a toggle that
 * waits a round trip reads as broken), then reconciles from the PATCH's own
 * response, which answers with the resource as a follow-up GET would; on
 * failure the optimistic state reverts to the last server truth and the error
 * is rethrown for the caller to phrase. Only the newest in-flight save may
 * reconcile or revert, so rapid toggles cannot resurrect an older answer.
 */
export function useTeamModelConfig(teamId: string): {
  config: TeamModelConfig | null
  save: (patch: TeamModelPatch) => Promise<void>
} {
  const [config, setConfig] = useState<TeamModelConfig | null>(null)
  // The last state the server confirmed — what a failed save falls back to.
  const settled = useRef<TeamModelConfig | null>(null)
  const seq = useRef(0)

  useEffect(() => {
    if (!teamId) return
    let live = true
    void apiJSON<TeamSettingsResource>(`/api/teams/${encodeURIComponent(teamId)}/settings`)
      .then((resource) => {
        if (!live) return
        settled.current = slice(resource)
        setConfig(settled.current)
      })
      .catch(() => {
        // The panel stays empty; the page around it still works.
      })
    return () => {
      live = false
    }
  }, [teamId])

  const save = useCallback(
    async (patch: TeamModelPatch) => {
      const mine = ++seq.current
      setConfig((prev) =>
        prev
          ? {
              defaultModel: patch.ai_model ?? prev.defaultModel,
              enabledModels: patch.enabled_models ?? prev.enabledModels,
            }
          : prev,
      )
      try {
        const resource = await apiJSON<TeamSettingsResource>(
          `/api/teams/${encodeURIComponent(teamId)}/settings`,
          {
            method: 'PATCH',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(patch),
          },
        )
        if (seq.current !== mine) return
        settled.current = slice(resource)
        setConfig(settled.current)
      } catch (e) {
        if (seq.current === mine) setConfig(settled.current)
        throw e
      }
    },
    [teamId],
  )

  return { config, save }
}
