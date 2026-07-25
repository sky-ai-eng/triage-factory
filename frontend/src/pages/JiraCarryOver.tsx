import { useNavigate } from 'react-router'
import CarryOverList from '../components/CarryOverList'

/**
 * JiraCarryOver is the final local first-run step — the migrated Jira
 * carry-over from the retired Setup wizard. It's reached only when local Jira
 * is connected: the setup wizard routes here on Finish when Jira is the
 * connected tracker, and straight to the app otherwise. The shared
 * CarryOverList owns the stock deck, polling, and per-row actions; this page
 * only wires its terminal navigations.
 *
 * Local-mode only for now (multi can adopt later). Save and Skip both land in
 * the app — carry-over is optional — and Back returns to the /setup wizard.
 */
export default function JiraCarryOver() {
  const navigate = useNavigate()

  const goToApp = () => navigate('/', { replace: true })

  return (
    <div className="min-h-screen bg-surface flex items-center justify-center p-4">
      <CarryOverList
        onSave={goToApp}
        onSkip={goToApp}
        onBack={() => navigate('/setup', { replace: true })}
      />
    </div>
  )
}
