import { useState } from 'react'
import { Link, useLocation } from 'react-router'
import { useAuth } from '../contexts/AuthContext'
import { useActiveOrgId } from '../contexts/OrgContext'
import { isOrgAdminRole } from '../hooks/useOrgRole'
import { startManagedGitHubConnect } from '../lib/githubApp'

/**
 * GitHubInstalled is where the deployment App's install callback lands when
 * there is no bind ceremony behind it: someone installed the App from its
 * public page on GitHub, or an owner approved an install request GitHub had
 * parked, or a reinstall came back without the cookie Connect set. The
 * installation is real and belongs to no workspace — an ordinary outcome of a
 * supported install path, not a failure, and the copy here never calls it one.
 *
 * The page does exactly two things: says what happened, and offers the same
 * Connect button Workspace Settings has. Pressing it runs the ordinary bind
 * ceremony; because the installation already exists, GitHub hands it straight
 * back with the same id and the bind completes. There is no "adopt" path and
 * no list of unbound installations to pick from — on a shared App such a list
 * is every other prospective tenant's GitHub account, and the page is handed
 * nothing about the installation that sent the visitor here, not even its id.
 *
 * Mounted at /github/installed in multi mode only, behind AuthGate: a visitor
 * with no session is sent to sign in with this page as the return target, and
 * a signed-in one has their memberships to connect from. The callback is a
 * top-level navigation from github.com, so the ?outcome= it carries is the
 * only thing the page learns: `requested` means GitHub sent the install to an
 * owner for approval and there is nothing to connect yet.
 */
export default function GitHubInstalled() {
  const auth = useAuth()
  const activeOrgId = useActiveOrgId()
  const location = useLocation()
  const requested = new URLSearchParams(location.search).get('outcome') === 'requested'

  // The workspace to connect FROM. The active one by default; a member of
  // several picks, because the one the callback interrupted is not
  // necessarily the one the installation is for.
  const orgs = auth.orgs
  const [chosenOrgId, setChosenOrgId] = useState<string | null>(null)
  const orgId = chosenOrgId ?? activeOrgId ?? orgs[0]?.id ?? null
  const org = orgs.find((o) => o.id === orgId) ?? null
  const canConnect = isOrgAdminRole(org?.role)

  return (
    <div className="min-h-screen bg-ground flex items-center justify-center p-4">
      <div className="w-full max-w-md backdrop-blur-xl bg-raised border border-line-1 rounded-2xl p-8 space-y-6 shadow-float shadow-black/[0.04]">
        <div className="space-y-1.5">
          <h1 className="text-[22px] font-semibold text-ink-1 tracking-tight">
            {requested ? 'Install request sent' : 'GitHub App installed'}
          </h1>
          <p className="text-body text-ink-3 leading-relaxed">
            {requested ? (
              <>
                GitHub sent your install request to an owner of that account rather than installing
                the App. Once they approve it, come back and press Connect from the workspace it
                belongs to.
              </>
            ) : (
              <>
                The installation went through on GitHub, but it isn&rsquo;t connected to a workspace
                yet &mdash; Triage Factory can&rsquo;t tell which one it belongs to when the App is
                installed from GitHub rather than from here. Press Connect to finish: GitHub will
                hand the installation back and it will be attached to this workspace.
              </>
            )}
          </p>
        </div>

        {orgs.length > 1 && (
          <label className="block space-y-1.5">
            <span className="text-label uppercase tracking-wide text-ink-3">Workspace</span>
            <select
              value={orgId ?? ''}
              onChange={(e) => setChosenOrgId(e.target.value)}
              className="w-full rounded-xl border border-line-1 bg-tint-2 px-3 py-2 text-body text-ink-1"
            >
              {orgs.map((o) => (
                <option key={o.id} value={o.id}>
                  {o.name}
                </option>
              ))}
            </select>
          </label>
        )}

        {org && !requested && canConnect && (
          <button
            type="button"
            onClick={() => startManagedGitHubConnect(org.id)}
            className="w-full bg-warm hover:bg-warm/90 text-warm-ink font-medium rounded-xl px-4 py-2.5 text-body transition-colors"
          >
            Connect GitHub to {org.name}
          </button>
        )}

        {org && !requested && !canConnect && (
          <p className="text-reported text-ink-3 leading-relaxed">
            Connecting GitHub takes a workspace admin. Ask an admin of {org.name} to press Connect
            GitHub in Workspace Settings.
          </p>
        )}

        {org && (
          <p className="text-reported text-ink-3">
            <Link
              to={`/orgs/${encodeURIComponent(org.id)}/settings#github-app`}
              className="text-warm hover:underline"
            >
              {requested ? 'Go to Workspace Settings' : 'Or finish later from Workspace Settings'}
            </Link>
          </p>
        )}
      </div>
    </div>
  )
}
