import { useState, useEffect } from 'react'
import { apiJSON } from '../lib/apiClient'

interface AuthStatus {
  // configured = "a provisioned tenant exists" (the user has run the
  // explicit "Start your factory" provision action). It no longer
  // means "GitHub creds present" — those moved to a later config step,
  // surfaced via the github/jira/github_repos fields below. No tenant ⇒
  // first-run; the AuthGate routes to the "Start your factory" screen.
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
  // setup_complete = github_ready AND the org tracks ≥1 repo AND every model
  // choice setup requires has been made. The gate blocks the product until this
  // is true; setup_step names which configure screen an incomplete founder
  // resumes on ('org' → GitHub access or the background-jobs model, 'team' →
  // tracked repos or the team's default model, 'done' → complete). Both absent
  // only while loading.
  setup_complete?: boolean
  setup_step?: 'org' | 'team' | 'done'
  loading: boolean
}

type StatusBody = Omit<AuthStatus, 'loading'>

const NOT_READ: StatusBody = { configured: false, github: false, jira: false }

// orgId is the org whose status to read: local mode's sentinel org, or the
// active org in multi mode, where a switch re-reads. The hook answers only
// from a read of the org it was given: while orgId is null (multi mode still
// resolving the org), and from a switch until the new org's first read
// settles, it reports loading, so a gate never decides on another org's
// status.
//
// pollMs, when set, re-polls on that interval so a gate blocking on an
// incomplete org self-heals: a non-admin parked on the "setup isn't
// finished" screen is admitted automatically once an admin completes setup,
// rather than having to refresh by hand. Polling stops the moment a response
// reports setup_complete (nothing left to watch) and pauses while the tab is
// hidden — but a tab becoming visible again triggers an immediate refetch so
// returning to a backgrounded tab is snappy rather than waiting out the
// interval.
export function useAuthStatus(orgId: string | null, pollMs?: number): AuthStatus {
  // The last settled read, with the org it was for. body is null when that
  // org's reads have all failed.
  const [read, setRead] = useState<{ orgId: string; body: StatusBody | null } | null>(null)

  useEffect(() => {
    if (!orgId) return
    let cancelled = false
    let timer: ReturnType<typeof setInterval> | undefined

    const stopPolling = () => {
      if (timer) {
        clearInterval(timer)
        timer = undefined
      }
    }

    // Every failure — a non-2xx, a network drop, a body that isn't JSON —
    // lands in the same arm: stop the spinner, keep this org's last known
    // status, let the next tick try again. This is a gate poll, not a user
    // action; there is no message to show and nothing to retry by hand.
    const load = () => {
      apiJSON<StatusBody>(`/api/orgs/${encodeURIComponent(orgId)}/integrations/status`)
        .then((data) => {
          if (cancelled) return
          setRead({ orgId, body: data })
          // Once the org is fully configured the gate admits the user, so
          // there's nothing left to poll for — drop the interval.
          if (data.setup_complete) stopPolling()
        })
        .catch(() => {
          if (cancelled) return
          setRead((prev) => (prev?.orgId === orgId ? prev : { orgId, body: null }))
        })
    }

    load()
    if (pollMs && pollMs > 0) {
      // Skip the tick while the tab is backgrounded; the visibility handler
      // below covers the catch-up fetch when it returns to the foreground.
      timer = setInterval(() => {
        if (!document.hidden) load()
      }, pollMs)
    }

    const onVisible = () => {
      if (!document.hidden && !cancelled) load()
    }
    document.addEventListener('visibilitychange', onVisible)

    return () => {
      cancelled = true
      stopPolling()
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [orgId, pollMs])

  if (!orgId || read?.orgId !== orgId) return { ...NOT_READ, loading: true }
  return { ...(read.body ?? NOT_READ), loading: false }
}
