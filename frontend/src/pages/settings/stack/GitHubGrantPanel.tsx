// The grant half of Settings' "GitHub access" section, shared by the two
// App-class shapes — a workspace with its own App and one riding the
// deployment's. What a workspace's App installations can reach is a fact about
// the ORG'S credential, identical for every admin who looks: nothing here is
// narrowed to the viewer's own GitHub access, because delegation mints from the
// grant and never consults anyone's personal permissions, so a narrowed view
// would describe nothing about capability and only mislead.
//
// Two pieces. The installation list renders each bound account with the three
// things the mirror knows about it that change what an admin should do: a
// suspension (GitHub refuses every token — its own state, not a generic
// error), the width of its grant (every repository, a selected set, or not yet
// known, which is neither), and the GitHub page where that grant is edited —
// the grant is never a form in TF. The findings render the two things the
// mirror exists to make computable: repositories the App reaches that nobody
// tracks, and repositories a team tracks that the App cannot reach. Both are
// read from the server's mirror — opening the panel asks GitHub nothing — and
// an empty finding says so, because a blank region reads like a load that
// failed.

import { ExternalLink } from 'lucide-react'
import type { PagedList } from '../../../hooks/usePagedList'
import type { GrantFindings } from '../../../hooks/useGrantFindings'
import { isHttpUrl } from '../../../lib/reachability'
import type { GitHubAppInstallation, ScopeDriftItem } from '../../../lib/githubApp'

// driftCountByInstallation folds the loaded drift page into a per-installation
// count, for the card's "N tracked repositories outside this grant" line. It
// counts the loaded rows, which is exact while the finding fits one page and a
// floor after that; the heading beside it carries the server's total.
function driftCountByInstallation(items: ScopeDriftItem[]): Map<string, number> {
  const counts = new Map<string, number>()
  for (const item of items) {
    if (!item.installation_id) continue
    counts.set(item.installation_id, (counts.get(item.installation_id) ?? 0) + 1)
  }
  return counts
}

function formatDate(rfc3339: string): string {
  const d = new Date(rfc3339)
  return Number.isNaN(d.getTime()) ? rfc3339 : d.toLocaleDateString()
}

// GitHubLink is an outbound link the backend derived from a GitHub base URL —
// gated to http(s) before it becomes an href, as every backend-derived link on
// this surface is. Renders nothing for a blank or non-http URL rather than a
// dead anchor.
function GitHubLink({ href, children }: { href: string; children: React.ReactNode }) {
  if (!isHttpUrl(href)) return null
  return (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      className="inline-flex items-center gap-1 text-ui text-warm hover:underline"
    >
      {children}
      <ExternalLink size={12} />
    </a>
  )
}

// GrantWidth is the three-way sentence on an installation's
// repository_selection. The three are different claims and get different
// words: "cannot drift" (all), "N outside" or "none outside" (selected), and
// "not known yet" (null) — never one of the other two dressed up.
function GrantWidth({
  selection,
  driftCount,
}: {
  selection: GitHubAppInstallation['repository_selection']
  driftCount: number
}) {
  if (selection === 'all') {
    return (
      <p className="text-reported text-ink-3">
        Grants every repository on this account — a tracked repository here can never fall outside
        the grant.
      </p>
    )
  }
  if (selection === 'selected') {
    return driftCount > 0 ? (
      <p className="text-reported text-warm">
        Grants selected repositories — {driftCount} tracked{' '}
        {driftCount === 1 ? 'repository is' : 'repositories are'} outside the grant. Add{' '}
        {driftCount === 1 ? 'it' : 'them'} on GitHub, or untrack {driftCount === 1 ? 'it' : 'them'}.
      </p>
    ) : (
      <p className="text-reported text-ink-3">
        Grants selected repositories — nothing tracked is outside the grant. A repository created
        later stays outside it until it is added on GitHub.
      </p>
    )
  }
  return (
    <p className="text-reported text-ink-3">
      Which repositories this installation grants isn&rsquo;t known yet — the next refresh records
      it.
    </p>
  )
}

// GitHubInstallationList renders the accounts the App is installed on, each
// with its suspension, its grant width, and its settings page on GitHub. When
// `onDisconnect` is given (the deployment-App workspace), every account carries
// its own Disconnect — the narrowed verb, which is the full disconnect when the
// account is the last one. A workspace with its own App tears the App down
// through the switch flow instead, so it passes none.
export function GitHubInstallationList({
  installations,
  drift,
  busy,
  onDisconnect,
}: {
  installations: GitHubAppInstallation[]
  drift: ScopeDriftItem[]
  busy?: boolean
  onDisconnect?: (inst: GitHubAppInstallation) => void
}) {
  if (installations.length === 0) return null
  const driftCounts = driftCountByInstallation(drift)
  return (
    <ul className="space-y-2" aria-label="Installed on">
      {installations.map((inst) => {
        const suspended = inst.suspended_at !== ''
        return (
          <li
            key={inst.installation_id}
            data-suspended={suspended || undefined}
            className={`space-y-1.5 rounded-xl border px-3 py-2.5 ${
              suspended ? 'border-alarm/40 bg-alarm/[0.06]' : 'border-line-1 bg-raised'
            }`}
          >
            <div className="flex flex-wrap items-center justify-between gap-2">
              <div className="flex items-center gap-2">
                <span className="text-ui font-medium text-ink-1">@{inst.account_login}</span>
                <span className="text-label uppercase tracking-wide text-ink-3">
                  {inst.account_type}
                </span>
                {suspended && (
                  <span className="rounded-full border border-alarm/40 px-2 py-0.5 text-label uppercase tracking-wide text-alarm">
                    Suspended
                  </span>
                )}
              </div>
              <div className="flex items-center gap-3">
                <GitHubLink href={inst.settings_url}>Manage on GitHub</GitHubLink>
                {onDisconnect && (
                  <button
                    type="button"
                    disabled={busy}
                    onClick={() => onDisconnect(inst)}
                    className="text-ui text-ink-3 underline transition-colors hover:text-alarm disabled:opacity-40"
                  >
                    Disconnect @{inst.account_login}
                  </button>
                )}
              </div>
            </div>
            {suspended ? (
              <p className="text-reported text-alarm">
                Suspended on GitHub{inst.suspended_by ? ` by @${inst.suspended_by}` : ''} since{' '}
                {formatDate(inst.suspended_at)}. The grant is intact, but GitHub refuses every token
                minted from it until the account owner unsuspends the App there.
              </p>
            ) : (
              <GrantWidth
                selection={inst.repository_selection}
                driftCount={driftCounts.get(inst.installation_id) ?? 0}
              />
            )}
          </li>
        )
      })}
    </ul>
  )
}

// FindingList is one finding's block: a heading with the server's total, the
// rows with their verb, and — when the finding is empty — a sentence saying
// so. `total` is the count across every page; the rows are the loaded ones.
function FindingList<T>({
  title,
  lead,
  list,
  nothing,
  keyOf,
  row,
}: {
  title: string
  lead: string
  list: PagedList<T>
  nothing: string
  keyOf: (item: T) => string
  row: (item: T) => React.ReactNode
}) {
  const total = list.total ?? list.items.length
  return (
    <section className="space-y-2" aria-label={title}>
      <div className="flex items-baseline justify-between gap-3">
        <h4 className="text-body font-medium text-ink-1">
          {title}
          {total > 0 && <span className="ml-2 text-ui font-normal text-ink-3">{total}</span>}
        </h4>
      </div>
      <p className="text-reported leading-relaxed text-ink-3">{lead}</p>
      {list.error ? (
        <p className="text-ui text-alarm">{list.error}</p>
      ) : list.items.length === 0 ? (
        <p className="text-ui text-ink-3">{list.loading ? 'Checking…' : nothing}</p>
      ) : (
        <ul className="space-y-1">
          {list.items.map((item) => (
            <li
              key={keyOf(item)}
              className="flex flex-wrap items-center justify-between gap-2 rounded-xl border border-line-1 bg-raised px-3 py-2"
            >
              {row(item)}
            </li>
          ))}
        </ul>
      )}
      {list.hasMore && (
        <button
          type="button"
          disabled={list.loading}
          onClick={() => void list.loadMore()}
          className="text-ui text-ink-3 underline hover:text-ink-2 disabled:opacity-40"
        >
          Show more
        </button>
      )}
    </section>
  )
}

// GitHubGrantFindings renders both findings for an App-class workspace. It is
// never rendered for a PAT workspace: a token's reach is not a grant TF holds,
// and no copy here may imply one.
export function GitHubGrantFindings({ findings }: { findings: GrantFindings }) {
  return (
    <div className="space-y-4">
      <FindingList
        title="Reachable but untracked"
        lead="Repositories the App can reach that no team tracks. Triage Factory holds access to code nobody asked it to touch — narrow the grant on GitHub, or track them."
        list={findings.reach}
        nothing="Nothing to address — every repository the App reaches is tracked by a team."
        keyOf={(item) => item.slug}
        row={(item) => (
          <>
            <span className="text-ui text-ink-1">
              {item.slug}
              {item.private && (
                <span className="ml-2 text-label uppercase tracking-wide text-ink-3">private</span>
              )}
            </span>
            <span className="flex items-center gap-3 text-reported text-ink-3">
              via @{item.account_login}
              <GitHubLink href={item.settings_url}>Narrow the grant</GitHubLink>
            </span>
          </>
        )}
      />
      <FindingList
        title="Tracked but outside the grant"
        lead="Repositories a team tracks that the App cannot reach. They are silently not polled — add them to the grant on GitHub, or untrack them."
        list={findings.drift}
        nothing="Nothing to address — every tracked repository is inside the grant."
        keyOf={(item) => item.slug}
        row={(item) => (
          <>
            <span className="text-ui text-ink-1">{item.slug}</span>
            {item.installation_id ? (
              <span className="flex items-center gap-3 text-reported text-ink-3">
                outside @{item.account_login}&rsquo;s grant
                <GitHubLink href={item.settings_url}>Add it on GitHub</GitHubLink>
              </span>
            ) : (
              <span className="text-reported text-ink-3">
                no connected account owns @{item.owner} — connect it, or untrack the repository
              </span>
            )}
          </>
        )}
      />
    </div>
  )
}
