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
export default function Prompts() {
  const activeTeam = useActiveTeam('prompts')

  return (
    <div className="flex h-full flex-col">
      <PromptsWorkspace
        teamId={activeTeam.teamId}
        ready={activeTeam.ready}
        toolbarLeft={<h1 className="text-section font-semibold text-ink-1">Prompts</h1>}
        toolbarRight={<TeamSwitch value={activeTeam.teamId} onChange={activeTeam.setTeamId} />}
      />
    </div>
  )
}
