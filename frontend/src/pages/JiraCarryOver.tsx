import { useNavigate, useParams } from 'react-router-dom'
import CarryOverList from '../components/CarryOverList'

/**
 * JiraCarryOver is the final local first-run step — the migrated Jira
 * carry-over from the retired Setup wizard. It's reached only when local Jira
 * is connected: TeamConfigure routes here on Finish when jiraConnected, and
 * straight to the app otherwise. The shared CarryOverList owns the stock
 * deck, polling, and per-row actions; this page only wires its terminal
 * navigations.
 *
 * Local-mode only for now (multi can adopt later). Save and Skip both land in
 * the app — carry-over is optional — and Back returns to team-configure.
 */
export default function JiraCarryOver() {
  const navigate = useNavigate()
  const { org_id: orgId } = useParams<{ org_id: string }>()

  const goToApp = () => navigate('/', { replace: true })

  return (
    <div className="min-h-screen bg-surface flex items-center justify-center p-4">
      <CarryOverList
        onSave={goToApp}
        onSkip={goToApp}
        onBack={() =>
          navigate(orgId ? `/orgs/${orgId}/teams/default/configure` : '/', { replace: true })
        }
      />
    </div>
  )
}
