import { useEffect, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { toast } from '../components/Toast/toastStore'
import ReposGroup from './settings/ReposGroup'
import GitHubTeamGroup from './settings/GitHubTeamGroup'
import JiraProjectRulesGroup from './settings/JiraProjectRulesGroup'
import TeamSettingsGroup from './settings/TeamSettingsGroup'
import { fetchOrgSettings } from './settings/orgConfig'
import {
  emptyTeamConfig,
  fetchTeamRepos,
  fetchTeamSettings,
  saveTeamConfig,
  teamConfigFromSettings,
  teamProjectsBlocked,
  type TeamConfigForm,
} from './settings/teamConfig'

/**
 * TeamConfigure is the create-time configure step for the founder's Default
 * team — step 4 of onboarding, the team mirror of OrgConfigure. By the time
 * the founder lands here the org exists and is configured (OrgConfigure ran
 * before), and BootstrapNewOrg already created the Default team, so this
 * *configures* an existing team rather than creating a row: tracked repos,
 * GitHub-team mappings, Jira project rules, and team AI settings, all through
 * the same shared field groups the Settings team tab uses.
 *
 * It sits OUTSIDE RequireGitHubIdentity (like OrgConfigure / ConnectGitHub):
 * the repo + GitHub-team pickers want GitHub configured, but the gate would
 * loop the founder who's mid-setup. Multi-mode only (its sole mounter is
 * MultiRoutes), and intentionally skippable — everything here is reachable
 * later from Settings.
 *
 * The {team_id} segment is usually the literal "default" (the alias the team
 * endpoints resolve to the org's Default team), so OrgConfigure can route
 * here without first resolving the team's UUID.
 */
export default function TeamConfigure() {
  const navigate = useNavigate()
  const { org_id: orgId, team_id: teamId } = useParams<{ org_id: string; team_id: string }>()
  const team = teamId ?? 'default'

  const [form, setForm] = useState<TeamConfigForm>(emptyTeamConfig())
  const [jiraConnected, setJiraConnected] = useState(false)
  const [loaded, setLoaded] = useState(false)
  // False when the team-repos GET failed (form.repos stays undefined). Drives
  // ReposGroup's "couldn't load — left unchanged" state so the empty picker
  // doesn't read as "you picked none".
  const [reposLoaded, setReposLoaded] = useState(false)
  const [saving, setSaving] = useState(false)

  // Seed from the team's current state (the bootstrap set defaults) so the
  // founder edits rather than re-enters. Repos come from their own endpoint;
  // GitHub-team mappings seed via GitHubTeamGroup's onLoaded once it fetches
  // the org's candidate teams. Jira connectivity is an org-level fact, so the
  // project-rules group only shows once org Jira is connected.
  useEffect(() => {
    let cancelled = false
    Promise.all([fetchTeamSettings(team), fetchTeamRepos(team), fetchOrgSettings()])
      .then(([settings, repos, org]) => {
        if (cancelled) return
        if (settings) {
          // fetchTeamRepos returns null when its GET failed — keep repos
          // undefined in that case (not []), so Finish leaves the team's
          // tracked repos untouched instead of wiping them. github_groups
          // likewise stays undefined until GitHubTeamGroup's onLoaded seeds it.
          setForm((f) => ({
            ...teamConfigFromSettings(settings),
            repos: repos ?? undefined,
            github_groups: f.github_groups,
          }))
          setReposLoaded(repos !== null)
          if (repos === null) {
            toast.error('Could not load tracked repositories — they will be left unchanged.')
          }
        } else {
          toast.error('Could not load your team settings. Showing defaults — verify before saving.')
        }
        setJiraConnected(!!org && org.has_jira_pat && !!org.jira_base_url)
        setLoaded(true)
      })
      .catch(() => {
        // A rejected fetch (network error) must not leave the founder stuck on
        // "Loading…". Fall through to the form on defaults and flag it.
        if (cancelled) return
        toast.error('Could not load your team settings. Showing defaults — verify before saving.')
        setLoaded(true)
      })
    return () => {
      cancelled = true
    }
  }, [team])

  const patchForm = (patch: Partial<TeamConfigForm>) => setForm((f) => ({ ...f, ...patch }))

  const goToApp = () => navigate(orgId ? '/orgs/' + orgId : '/', { replace: true })

  const projectsBlocked = teamProjectsBlocked(form.jira_projects, jiraConnected)

  const finish = async () => {
    setSaving(true)
    try {
      const result = await saveTeamConfig(team, form)
      if (!result.ok) {
        toast.error(result.error)
        return
      }
      if (result.warning) toast.info(result.warning)
      goToApp()
    } catch (err) {
      toast.error(`Could not save configuration: ${(err as Error).message}`)
    } finally {
      setSaving(false)
    }
  }

  if (!loaded) {
    return (
      <div className="min-h-screen bg-surface flex items-center justify-center">
        <p className="text-text-tertiary text-sm">Loading…</p>
      </div>
    )
  }

  return (
    <div className="min-h-screen bg-surface py-10 px-4">
      <div className="max-w-2xl mx-auto space-y-5">
        <div className="space-y-1.5">
          <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">
            Configure your team
          </h1>
          <p className="text-[13px] text-text-tertiary leading-relaxed">
            Pick the repositories this team watches, map the GitHub teams whose reviews route here,
            set Jira project rules, and choose the delegation model. You can change any of this
            later in Settings.
          </p>
        </div>

        {/* The groups render a plain array; the undefined ("unloaded") state
            lives only in the form + saver. A user edit here makes the slice
            defined, so an intentional pick is saved even if its initial load
            had failed — only an untouched, never-loaded slice is skipped. */}
        <ReposGroup
          value={form.repos ?? []}
          onChange={(repos) => patchForm({ repos })}
          loaded={reposLoaded}
        />

        <GitHubTeamGroup
          value={form.github_groups ?? []}
          onChange={(github_groups) => patchForm({ github_groups })}
          teamId={team}
          includeMembership
          onLoaded={(github_groups) => patchForm({ github_groups })}
        />

        <JiraProjectRulesGroup
          value={form.jira_projects}
          onChange={(jira_projects) => patchForm({ jira_projects })}
          connected={jiraConnected}
        />

        <TeamSettingsGroup
          value={{
            default_model: form.default_model,
            auto_delegate_enabled: form.auto_delegate_enabled,
          }}
          onChange={patchForm}
        />

        <div className="flex gap-3">
          <button
            type="button"
            onClick={goToApp}
            className="flex-1 bg-white/50 hover:bg-white/80 border border-border-subtle text-text-secondary font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
          >
            I&rsquo;ll configure later
          </button>
          <button
            type="button"
            onClick={finish}
            disabled={saving || projectsBlocked}
            className="flex-1 bg-accent hover:bg-accent/90 disabled:opacity-40 text-white font-medium rounded-xl px-4 py-2.5 text-[13px] transition-colors"
          >
            {saving ? 'Saving…' : 'Finish setup'}
          </button>
        </div>
      </div>
    </div>
  )
}
