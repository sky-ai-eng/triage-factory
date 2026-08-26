import { useState } from 'react'
import {
  Section,
  Field,
  inputClass,
  glassInputClass,
  segmentedWrap,
  segmentedBtn,
} from './primitives'
import GitHubAppPanel from './GitHubAppPanel'
import type { CloneProtocol } from './orgConfig'
import { apiJSON, httpErrorMessage } from '../../lib/apiClient'

interface GitHubAccessValue {
  github_url: string
  github_pat: string
  github_clone_protocol: CloneProtocol
}

/**
 * GitHubAccessGroup is the org-level GitHub credential field group: base
 * URL, PAT (with "leave blank to keep current" semantics), and — local mode
 * only — the clone-protocol toggle plus an on-demand SSH preflight. It's a
 * controlled component: the container owns the form state and the actual
 * POST, so the same group serves Settings, the org-create configure step,
 * and the local Setup wizard without a parallel implementation.
 *
 * The PAT here is the simple default; the App-registration alternative
 * renders below via GitHubAppPanel when an orgId is available and the
 * caller opts in (showAppPanel).
 *
 * showBaseUrl (default true) lets a surface that owns the base URL elsewhere
 * suppress the field here without forking the group: the setup wizard
 * confirms the URL in its own reachability sub-step, then renders this group
 * in PAT-only mode (showBaseUrl false, showAppPanel false) so the App vs PAT
 * choice reads as two tabs rather than one stacked panel.
 */
export default function GitHubAccessGroup({
  value,
  onChange,
  hasToken,
  isLocal,
  orgId,
  showAppPanel = true,
  showBaseUrl = true,
  showHeading = true,
  showCloneProtocol = true,
  bare = false,
}: {
  value: GitHubAccessValue
  onChange: (patch: Partial<GitHubAccessValue>) => void
  hasToken: boolean
  isLocal: boolean
  orgId: string | null
  showAppPanel?: boolean
  showBaseUrl?: boolean
  // The setup wizard renders this under its own "GitHub access" step heading
  // and an App/PAT tab switcher, so the group's own "GitHub" title is redundant
  // there — suppress it. Default true keeps the Settings tab labelled.
  showHeading?: boolean
  // The setup wizard splits the clone-protocol choice into its own step, so the
  // PAT step renders the token alone (showCloneProtocol false). Default true
  // keeps Settings showing it inline.
  showCloneProtocol?: boolean
  // The setup wizard composes this flush (no Section card, glass fields) so it
  // matches the rest of the flow; Settings keeps the carded default.
  bare?: boolean
}) {
  const fieldCls = bare ? glassInputClass : inputClass
  const [sshTestState, setSshTestState] = useState<
    { kind: 'idle' } | { kind: 'running' } | { kind: 'ok' } | { kind: 'fail'; stderr: string }
  >({ kind: 'idle' })

  // Test SSH preflight on demand. Purely diagnostic — doesn't save
  // settings — so a user can confirm reachability before saving and
  // watching the bootstrap re-clone.
  const testSSH = async () => {
    setSshTestState({ kind: 'running' })
    try {
      const data = await apiJSON<{ ok: boolean; stderr?: string }>('/api/github/preflight-ssh', {
        method: 'POST',
      })
      if (data.ok) {
        setSshTestState({ kind: 'ok' })
      } else {
        setSshTestState({ kind: 'fail', stderr: data.stderr || 'Preflight failed.' })
      }
    } catch (err) {
      setSshTestState({ kind: 'fail', stderr: httpErrorMessage(err, 'Preflight failed.') })
    }
  }

  const inner = (
    <>
      {showHeading && <h2 className="text-body font-medium text-ink-2 mb-4">GitHub</h2>}
      <div className="space-y-3">
        {showBaseUrl && (
          <Field label="Base URL">
            <input
              type="url"
              placeholder="https://github.com"
              value={value.github_url}
              onChange={(e) => onChange({ github_url: e.target.value })}
              className={fieldCls}
            />
          </Field>
        )}
        <Field label={`Token${hasToken ? ' (leave blank to keep current)' : ''}`}>
          <input
            type="password"
            placeholder={hasToken ? '••••••••' : 'GitHub Personal Access Token'}
            value={value.github_pat}
            onChange={(e) => onChange({ github_pat: e.target.value })}
            className={fieldCls}
          />
          <p className="text-reported text-ink-3 mt-1">
            Requires a{' '}
            <a
              href="https://github.com/settings/tokens/new?scopes=repo,read:org,user:email&description=Triage+Factory"
              target="_blank"
              rel="noopener noreferrer"
              className="text-warm hover:underline"
            >
              classic PAT
            </a>{' '}
            with <code className="text-ink-2">repo</code>,{' '}
            <code className="text-ink-2">read:org</code>, and{' '}
            <code className="text-ink-2">user:email</code> scopes — the token Triage Factory&rsquo;s
            bots poll your organization with. <code className="text-ink-2">read:org</code> lets them
            resolve your organization&rsquo;s team memberships so review requests routed to teams
            (e.g. CODEOWNERS) surface as tasks — without it, only PRs that name a reviewer directly
            are visible.
          </p>
        </Field>
        {/* Clone protocol is local-mode-only: multi-mode deployments
              hardwire HTTPS (App-token credential; the container has no SSH
              machinery), so there's nothing to choose and no SSH to test. The
              setup wizard splits this into its own step (showCloneProtocol). */}
        {isLocal && showCloneProtocol && (
          <Field label="Clone protocol">
            <div className="flex items-center gap-3">
              <div className={segmentedWrap(bare)}>
                {(['ssh', 'https'] as const).map((p) => (
                  <button
                    key={p}
                    type="button"
                    onClick={() => {
                      onChange({ github_clone_protocol: p })
                      setSshTestState({ kind: 'idle' })
                    }}
                    className={segmentedBtn(value.github_clone_protocol === p, bare)}
                  >
                    {p.toUpperCase()}
                  </button>
                ))}
              </div>
              {/* The preflight probes the SSH host derived from the
                    *stored* GitHub base URL (server-side), not the form, so
                    it's only meaningful once credentials are saved. Hide it
                    until then — pre-save, the GitHub step's submit runs its
                    own server-side preflight against the URL being saved. */}
              {hasToken && (
                <button
                  type="button"
                  onClick={testSSH}
                  disabled={sshTestState.kind === 'running'}
                  className="text-reported text-warm hover:underline disabled:opacity-50"
                >
                  {sshTestState.kind === 'running' ? 'Testing...' : 'Test SSH connection'}
                </button>
              )}
            </div>
            <p className="text-reported text-ink-3 mt-1.5 leading-relaxed">
              Your token is still required for the GitHub API. The protocol sets the transport for
              every Git operation Triage Factory and its agents make; base-branch protection and
              push recording ride the HTTPS identity and do not apply on SSH. Saving the toggle
              re-clones bare repos with the new origin URL.
            </p>
            {sshTestState.kind === 'ok' && (
              <p className="text-reported text-warm mt-1.5">
                ✓ SSH preflight succeeded — git@
                {(() => {
                  try {
                    return new URL(value.github_url).hostname || 'github.com'
                  } catch {
                    return 'github.com'
                  }
                })()}{' '}
                is reachable with your key.
              </p>
            )}
            {sshTestState.kind === 'fail' && (
              <pre
                className="
                mt-1.5 max-h-[120px] overflow-auto rounded
                bg-[var(--color-alarm)]/10 p-2 text-reported
                text-[var(--color-alarm)] whitespace-pre-wrap break-words
              "
              >
                {sshTestState.stderr}
              </pre>
            )}
          </Field>
        )}
      </div>
    </>
  )

  return (
    <>
      {bare ? inner : <Section>{inner}</Section>}
      {/* The combined access group is a Settings-context surface (the wizard
          drives App vs PAT as separate steps with showAppPanel false), so the
          panel's post-registration callback returns to Settings. */}
      {showAppPanel && orgId && <GitHubAppPanel orgId={orgId} returnTo="settings" />}
    </>
  )
}
