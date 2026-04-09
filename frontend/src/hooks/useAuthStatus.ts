import { useState, useEffect } from 'react'

interface AuthStatus {
  configured: boolean
  github: boolean
  jira: boolean
  github_url?: string
  jira_url?: string
  github_repos?: number
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
    fetch('/api/auth/status')
      .then((res) => res.json())
      .then((data) => setStatus({ ...data, loading: false }))
      .catch(() => setStatus((s) => ({ ...s, loading: false })))
  }, [])

  return status
}
