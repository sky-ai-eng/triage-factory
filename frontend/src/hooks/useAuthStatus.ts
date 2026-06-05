import { useState, useEffect } from 'react'

interface AuthStatus {
  // configured = "a provisioned tenant exists" (the user has run the
  // explicit "Start your Triage Factory" provision action). It no longer
  // means "GitHub creds present" — those moved to a later config step,
  // surfaced via the github/jira/github_repos fields below. No tenant ⇒
  // first-run; the AuthGate routes to the "Start your Triage Factory" screen.
  configured: boolean
  github: boolean
  // github_ready folds the PAT signal together with a registered GitHub App
  // (the multi-mode access path), so the setup gate treats an App-configured
  // org as having GitHub set up even when no PAT is stored. `github` stays
  // PAT-only for the connection-status display.
  github_ready?: boolean
  jira: boolean
  github_url?: string
  jira_url?: string
  github_repos?: number
  env_provided?: string[]
  // setup_complete = github_ready AND the org tracks ≥1 repo. The gate
  // blocks the product until this is true; setup_step names which configure
  // screen an incomplete founder resumes on ('org' → GitHub access, 'team' →
  // tracked repos, 'done' → complete). Both absent only while loading.
  setup_complete?: boolean
  setup_step?: 'org' | 'team' | 'done'
  loading: boolean
}

// orgKey lets a multi-mode caller refetch when the active org changes (the
// endpoint resolves the session's active org, so a switch must re-poll).
// Local mode (N=1, no switching) omits it.
export function useAuthStatus(orgKey?: string): AuthStatus {
  const [status, setStatus] = useState<AuthStatus>({
    configured: false,
    github: false,
    jira: false,
    loading: true,
  })

  useEffect(() => {
    let cancelled = false
    fetch('/api/integrations/status')
      .then((res) => res.json())
      .then((data) => {
        if (!cancelled) setStatus({ ...data, loading: false })
      })
      .catch(() => {
        if (!cancelled) setStatus((s) => ({ ...s, loading: false }))
      })
    return () => {
      cancelled = true
    }
  }, [orgKey])

  return status
}
