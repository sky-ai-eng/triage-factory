// Whether the org's GitHub connection can read the Dependabot alerts of the
// repositories its teams track, from the poll's daily check of each one.
//
// A connection without access is a warning and never a failure: pull requests,
// reviews and CI work the same either way, and the copy says so. It also says
// who sees alerts once they are readable, because Triage Factory shows a
// repository's alerts to every member of a team that tracks it, which can be
// more people than GitHub shows them to.
//
// Renders nothing until at least one tracked repository has been checked, and
// nothing when the counts can't be read: an absent answer is "not known".
import { useEffect, useState } from 'react'
import { AlertTriangle, Info } from 'lucide-react'
import { getDependabotAccess, type DependabotAccess } from '../../lib/githubApp'

const AUDIENCE =
  'Every member of a team that tracks a repository sees its Dependabot alerts in Triage Factory, which can include people GitHub doesn’t show them to.'

const FIX = {
  app: 'The GitHub App needs read access to Dependabot alerts, and each installation has to accept the new permission on GitHub.',
  token:
    'A classic token needs the security_events scope, which repo includes; a fine-grained token needs read access to Dependabot alerts. GitHub also shows a repository’s alerts only to some accounts, and the token’s account has to be one of them.',
}

function repositories(n: number): string {
  return `${n} tracked ${n === 1 ? 'repository' : 'repositories'}`
}

function dependabotNotice(
  a: DependabotAccess,
  credential: 'app' | 'token',
): { warn: boolean; headline: string; detail: string[] } | null {
  if (a.readable + a.no_permission + a.disabled === 0) return null
  const off =
    a.disabled > 0
      ? [`Dependabot alerts are turned off on GitHub for ${repositories(a.disabled)}.`]
      : []
  if (a.no_permission > 0) {
    return {
      warn: true,
      headline: `Triage Factory can’t read Dependabot alerts on ${repositories(a.no_permission)}.`,
      detail: [FIX[credential], 'Everything else on GitHub works as before.', ...off],
    }
  }
  return {
    warn: false,
    headline:
      a.readable > 0
        ? `Triage Factory can read Dependabot alerts on ${repositories(a.readable)}.`
        : `Dependabot alerts are turned off on GitHub for ${repositories(a.disabled)}.`,
    detail: a.readable > 0 ? [AUDIENCE, ...off] : [],
  }
}

export default function GitHubDependabotNotice({
  orgId,
  credential,
}: {
  orgId: string
  credential: 'app' | 'token'
}) {
  const [access, setAccess] = useState<DependabotAccess | null>(null)

  useEffect(() => {
    let cancelled = false
    getDependabotAccess(orgId)
      .then((a) => {
        if (!cancelled) setAccess(a)
      })
      .catch(() => {
        // Not known is not a verdict: say nothing.
      })
    return () => {
      cancelled = true
    }
  }, [orgId])

  if (!access) return null
  const n = dependabotNotice(access, credential)
  if (!n) return null
  const Icon = n.warn ? AlertTriangle : Info

  return (
    <div
      className={`flex items-start gap-2.5 rounded-xl border px-3.5 py-3 ${
        n.warn ? 'border-alarm/25 bg-alarm/[0.06]' : 'border-line-1 bg-white/[0.02]'
      }`}
    >
      <Icon size={14} className={`mt-0.5 shrink-0 ${n.warn ? 'text-alarm' : 'text-ink-3'}`} />
      <div className="space-y-1">
        <p className="text-[13px] font-medium text-ink-2">{n.headline}</p>
        {n.detail.map((line) => (
          <p key={line} className="text-[12px] leading-relaxed text-ink-3">
            {line}
          </p>
        ))}
      </div>
    </div>
  )
}
