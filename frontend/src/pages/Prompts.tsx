import { useCallback } from 'react'
import { useSearchParams } from 'react-router'
import PromptsWorkspace from '../components/PromptsWorkspace'
import TeamSwitch from '../components/TeamSwitch'
import { useActiveTeam } from '../hooks/useTeams'

// Prompts is the standalone prompts/auto-delegation page, in both modes. It
// used to be local-only, because in multi the canonical home for this editor
// was a tab on /team — the /prompts route redirected there. The rail gives
// Prompts a row of its own now, with Library, Marketplace and Bindings under
// it, so the editor is a destination rather than a tab and /team is the team.
//
// It owns its own title + TeamSwitch chrome and delegates the editor itself to
// the shared <PromptsWorkspace>.
//
// Single-team by construction: the editor doubles as the binding graph, so it
// shows exactly one team's prompts + triggers, and new rows belong to that
// team. The switcher renders only for ≥2-team users; solo/local keeps teamId ''
// (the server resolves the sole team).
//
// A notification links to a trigger as ?trigger=…: the workspace opens it, the
// page switches to the trigger's team when the viewer is on that team, and the
// link is dropped from the URL once the trigger has been read.
export default function Prompts() {
  const { teamId, setTeamId, multi, ready, teams } = useActiveTeam('prompts')
  const [params, setParams] = useSearchParams()
  const linkedTrigger = params.get('trigger')

  const onTriggerOpened = useCallback(
    (triggerTeam: string | null) => {
      if (multi && triggerTeam && teams.some((t) => t.id === triggerTeam)) setTeamId(triggerTeam)
      setParams(
        (prev) => {
          const next = new URLSearchParams(prev)
          next.delete('trigger')
          return next
        },
        { replace: true },
      )
    },
    [multi, teams, setTeamId, setParams],
  )

  return (
    <div className="flex h-full flex-col">
      <PromptsWorkspace
        teamId={teamId}
        ready={ready}
        openTriggerId={ready ? linkedTrigger : null}
        onTriggerOpened={onTriggerOpened}
        toolbarLeft={<h1 className="text-section font-semibold text-ink-1">Prompts</h1>}
        toolbarRight={<TeamSwitch value={teamId} onChange={setTeamId} />}
      />
    </div>
  )
}
