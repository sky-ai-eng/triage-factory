import { useCallback, useEffect } from 'react'
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
// A notification links to one team's trigger as ?team=…&trigger=…: the page
// switches to that team when the viewer is on it, then opens the trigger, and
// drops both from the URL once the trigger has been read.
export default function Prompts() {
  const activeTeam = useActiveTeam('prompts')
  const { teamId, setTeamId, multi, ready, teams } = activeTeam
  const [params, setParams] = useSearchParams()
  const linkedTeam = params.get('team') ?? ''
  const linkedTrigger = params.get('trigger')
  const onLinkedTeam = multi && teams.some((t) => t.id === linkedTeam)

  useEffect(() => {
    if (ready && onLinkedTeam && teamId !== linkedTeam) setTeamId(linkedTeam)
  }, [ready, onLinkedTeam, teamId, linkedTeam, setTeamId])

  // Wait for the switch, so the panel opens read-only or not by the trigger's
  // own team.
  const openTriggerId =
    linkedTrigger && ready && (!onLinkedTeam || teamId === linkedTeam) ? linkedTrigger : null

  const clearLink = useCallback(() => {
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev)
        next.delete('team')
        next.delete('trigger')
        return next
      },
      { replace: true },
    )
  }, [setParams])

  return (
    <div className="flex h-full flex-col">
      <PromptsWorkspace
        teamId={teamId}
        ready={ready}
        openTriggerId={openTriggerId}
        onTriggerOpened={clearLink}
        toolbarLeft={<h1 className="text-section font-semibold text-ink-1">Prompts</h1>}
        toolbarRight={<TeamSwitch value={teamId} onChange={setTeamId} />}
      />
    </div>
  )
}
