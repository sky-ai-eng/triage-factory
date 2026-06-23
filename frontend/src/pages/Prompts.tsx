import PromptsWorkspace from '../components/PromptsWorkspace'
import TeamSwitch from '../components/TeamSwitch'
import { useActiveTeam } from '../hooks/useTeams'

// Prompts is the standalone prompts/auto-delegation page. In multi mode the
// canonical home for this editor is the /team page's Prompts tab (TFAC-445) —
// the /prompts route there redirects to it — so this page only renders in local
// mode (N=1), where there's no /team route. It owns its own title + TeamSwitch
// chrome and delegates the editor itself to the shared <PromptsWorkspace>.
//
// Single-team by construction: the editor doubles as the binding graph, so it
// shows exactly one team's prompts + triggers, and new rows belong to that
// team. The switcher renders only for ≥2-team users; solo/local keeps teamId ''
// (the server resolves the sole team).
export default function Prompts() {
  const activeTeam = useActiveTeam('prompts')

  return (
    <div className="flex flex-col" style={{ height: 'calc(100vh - 120px)' }}>
      <PromptsWorkspace
        teamId={activeTeam.teamId}
        ready={activeTeam.ready}
        toolbarLeft={<h1 className="text-[17px] font-semibold text-text-primary">Prompts</h1>}
        toolbarRight={<TeamSwitch value={activeTeam.teamId} onChange={activeTeam.setTeamId} />}
      />
    </div>
  )
}
