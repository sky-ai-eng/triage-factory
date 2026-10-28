// Calls for the Settings "Commit signing" section: the org's commit signer and
// the identity its runs commit as, under /api/orgs/{org_id}/github/
// (specs/commit-signing). Reads are member-gated, writes are
// org-admin.
//
// Unlike lib/githubApp.ts these let the HttpError escape: the setup form reads
// each fault's field as well as its message, through apiErrors, and every other
// caller turns it into a string with httpErrorMessage.

import { apiFetch, apiJSON, HttpError } from './apiClient'
import { hostOf } from './reachability'
import { timeAgo } from './relativeTime'

/** Whether a GitHub web base is github.com, the only host with a noreply
 *  address a signer's commits can carry without an address of its own. */
export function isGitHubCom(baseUrl: string): boolean {
  return hostOf(baseUrl).toLowerCase() === 'github.com'
}

/** Whether `login` on `baseUrl` is a managed user, whose email can start
 *  unverified: every GHE.com account, and on github.com an Enterprise Managed
 *  User, whose login ends in its enterprise's shortcode after an underscore,
 *  a character no other github.com login can contain. */
export function isManagedUser(baseUrl: string, login: string): boolean {
  const host = hostOf(baseUrl).toLowerCase()
  if (host === 'ghe.com' || host.endsWith('.ghe.com')) return true
  return host === 'github.com' && login.includes('_')
}

/** How long a status has held, from its RFC3339 start: "3h", "2d", or "" when
 *  it began under a minute ago. */
export function heldFor(since: string, now: number = Date.now()): string {
  const ago = timeAgo(since, now)
  return ago === 'just now' ? '' : ago.replace(/ ago$/, '')
}

export type CommitSignerStatus = 'pending' | 'active' | 'broken'

// CommitSigner mirrors the server's commitSignerJSON.
export interface CommitSigner {
  // The GitHub the account is on, as a web base: https://github.com, or a
  // GitHub Enterprise URL.
  host: string
  github_user_id: string
  login: string
  // The account's profile name, '' when it has none.
  name: string
  // The address the signer was given, null when its commits carry the derived
  // noreply address.
  email: string | null
  // What the signer's commits carry.
  commit_name: string
  commit_email: string
  public_key: string
  // A rotation's new key, null outside one.
  pending_public_key: string | null
  // The title to give whichever key is being added to the account.
  key_title: string
  status: CommitSignerStatus
  // Why a broken signer is broken: GitHub's verification reason for a pushed
  // commit (unknown_key, no_user, unverified_email, …) or key_missing.
  status_reason: string | null
  status_since: string
  checked_at: string | null
}

export type CommitSigningMode = 'none' | 'org' | 'deployment'

// CommitIdentity mirrors GET commit-identity: who the org's runs commit as,
// and whether and how they sign.
export interface CommitIdentity {
  identity: {
    name: string
    email: string
    github_user_id: string | null
    // The signer's login when the identity is a signer's.
    login: string | null
  } | null
  signing: CommitSigningMode
  // Why an org whose signer is active still commits unsigned (local mode):
  // git_too_old or git_unavailable.
  unsigned_reason: string | null
  git_version: string | null
}

const base = (orgId: string) => `/api/orgs/${encodeURIComponent(orgId)}/github`

const jsonInit = (method: string, body?: unknown): RequestInit => ({
  method,
  headers: { 'Content-Type': 'application/json' },
  ...(body === undefined ? {} : { body: JSON.stringify(body) }),
})

/** The org's signer, null when it has none (the route's 404). */
export async function getCommitSigner(orgId: string): Promise<CommitSigner | null> {
  try {
    return await apiJSON<CommitSigner>(`${base(orgId)}/commit-signer`)
  } catch (e) {
    if (e instanceof HttpError && e.status === 404) return null
    throw e
  }
}

export function getCommitIdentity(orgId: string): Promise<CommitIdentity> {
  return apiJSON<CommitIdentity>(`${base(orgId)}/commit-identity`)
}

/** Registers the signer. `email` is omitted for the derived noreply address,
 *  which only github.com has. */
export function createCommitSigner(
  orgId: string,
  login: string,
  email?: string,
): Promise<CommitSigner> {
  return apiJSON<CommitSigner>(
    `${base(orgId)}/commit-signer`,
    jsonInit('POST', email ? { login, email } : { login }),
  )
}

export function verifyCommitSigner(orgId: string): Promise<CommitSigner> {
  return apiJSON<CommitSigner>(`${base(orgId)}/commit-signer/verify`, jsonInit('POST'))
}

export function rotateCommitSigner(orgId: string): Promise<CommitSigner> {
  return apiJSON<CommitSigner>(`${base(orgId)}/commit-signer/rotate`, jsonInit('POST'))
}

/** Sets the committer address; null restores the derived noreply address. */
export function setCommitSignerEmail(orgId: string, email: string | null): Promise<CommitSigner> {
  return apiJSON<CommitSigner>(`${base(orgId)}/commit-signer`, jsonInit('PATCH', { email }))
}

export async function deleteCommitSigner(orgId: string): Promise<void> {
  await apiFetch(`${base(orgId)}/commit-signer`, { method: 'DELETE' })
}
