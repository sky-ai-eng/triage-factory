import { useState, useEffect } from 'react'

interface AuthStatus {
  // configured = "a provisioned tenant exists" (the user has run the
  // explicit "Start your Triage Factory" provision action). It no longer
  // means "GitHub creds present" — those moved to a later config step,
  // surfaced via the github/jira/github_repos fields below. No tenant ⇒
  // first-run; the AuthGate routes to /setup.
  configured: boolean
  github: boolean
  jira: boolean
  github_url?: string
  jira_url?: string
  github_repos?: number
  env_provided?: string[]
  loading: boolean
}

export function useAuthStatus(): AuthStatus {
  const [status, setStatus] = useState<AuthStatus>({
    configured: false,
    github: false,
    jira: false,
    loading: true,
  })

  useEffect(() => {
    fetch('/api/integrations/status')
      .then((res) => res.json())
      .then((data) => setStatus({ ...data, loading: false }))
      .catch(() => setStatus((s) => ({ ...s, loading: false })))
  }, [])

  return status
}
