import { useEffect, useState } from 'react'
import { getGitHubAppStatus, getGitHubAppInstallURL, type GitHubAppStatus } from '../lib/githubApp'

/**
 * useGitHubAppInstall fetches the org's App status (for the installed-accounts
 * list) and its install deep-link, refetching on window focus so returning from
 * the GitHub install tab refreshes the view. Read failures are swallowed —
 * status keeps its last value, installUrl stays blank — because the
 * authoritative install check is the host's refresh-then-verify action, not this
 * display fetch (and in local mode this read sees a mirror a fresh install
 * hasn't touched yet anyway).
 *
 * setStatus is exposed so a host that runs the refresh endpoint (which returns
 * fresh status) can fold the reconciled installations straight back into the
 * view without a second round trip.
 *
 * Shared by the setup wizard's "Install the App" step (GitHubStep) and the
 * Settings "App installation" section (OrgSettings).
 */
export function useGitHubAppInstall(orgId: string | null): {
  status: GitHubAppStatus | null
  setStatus: (s: GitHubAppStatus) => void
  installUrl: string
} {
  const [status, setStatus] = useState<GitHubAppStatus | null>(null)
  const [installUrl, setInstallUrl] = useState('')

  useEffect(() => {
    if (!orgId) return
    let cancelled = false
    const load = () => {
      getGitHubAppStatus(orgId)
        .then((s) => {
          if (!cancelled) setStatus(s)
        })
        .catch(() => {})
      getGitHubAppInstallURL(orgId)
        .then((u) => {
          if (!cancelled) setInstallUrl(u)
        })
        .catch(() => {})
    }
    load()
    const onFocus = () => load()
    window.addEventListener('focus', onFocus)
    return () => {
      cancelled = true
      window.removeEventListener('focus', onFocus)
    }
  }, [orgId])

  return { status, setStatus, installUrl }
}
