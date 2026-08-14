import { Section, Field, inputClass, glassInputClass } from './primitives'
import { toast } from '../../components/Toast/toastStore'
import { disconnectJira } from './orgCredentials'
import type { JiraDeployment } from './jiraConnect'

interface JiraAccessValue {
  jira_url: string
  jira_pat: string
  jira_email: string
  jira_api_token: string
}

/**
 * JiraAccessGroup is the org-level Jira credential field group. It's a
 * controlled component — the container owns the url/pat form values and the
 * `connected` flag, and the container's Continue (setup wizard) / Save
 * (Settings) performs the connect via the shared `connectJira` helper, exactly
 * as the GitHub PAT flow does. The group itself carries no Connect button.
 *
 * What it still owns is the disconnect half of the lifecycle (DELETE on the
 * credential resource): that's an inline action on the already-connected
 * state, with no Continue/Save to fold into. On a
 * successful disconnect it fires onDisconnected so the container can do its
 * own follow-up (clearing wizard URL-confirmation, resetting baseline).
 *
 * Sitting beside it is `onReplace` — a request to rebind, not a rebind. The
 * container answers by rendering this same group in its unconnected (fields +
 * Save) form, so rotating a credential doesn't have to start with a disconnect
 * and the window where the org has no credential and its poller is stopped
 * never opens.
 *
 * Project tracking + status rules are TEAM-level (a separate surface), so
 * they live outside this group.
 *
 * Which credential fields render is driven by the `deployment` prop, chosen
 * explicitly upstream (the wizard's deployment-picker step / the Settings
 * deployment toggle), not sniffed from the URL: Cloud shows the Atlassian
 * account email + API token (Basic auth); Data Center shows the single PAT
 * (Bearer). Cloud OAuth (one-click Connect) is a per-user flow that lives on
 * the identity surfaces (JiraUserAccessStep / the Settings identity section)
 * instead — this group is the org-level credential and only ever offers the
 * paste paths.
 *
 * showBaseUrl (default true) mirrors GitHubAccessGroup: the setup wizard
 * confirms the Jira URL in its own reachability sub-step and feeds it in via
 * `value.jira_url`, so it suppresses the field here and renders this group as
 * the PAT-auth sub-step alone.
 *
 * bare (default false) drops the carded `<Section>` chrome + heading and uses
 * the flush glass field material, so the setup wizard and the redesigned
 * Settings stack compose it flush like the other groups; Settings' legacy
 * carded look is the default.
 */
export default function JiraAccessGroup({
  value,
  onChange,
  connected,
  deployment,
  orgId,
  onReplace,
  envProvided = false,
  onDisconnected,
  showBaseUrl = true,
  bare = false,
}: {
  value: JiraAccessValue
  onChange: (patch: Partial<JiraAccessValue>) => void
  connected: boolean
  /** Org the credential belongs to — the DELETE is org-scoped by path. Null
   *  only while a multi-mode active org is still resolving, which the disconnect
   *  guards on rather than building a request against an unknown org. */
  orgId: string | null
  // The deployment chosen upstream (the wizard's deployment-picker step / the
  // Settings deployment toggle). It decides which credential fields render —
  // Cloud shows the Atlassian email + API token, Data Center the single PAT.
  // The choice is NOT inferred from the URL here: it's made explicitly so the
  // fields the user fills always match the scheme the connect sends.
  deployment: JiraDeployment
  // Asks the container to re-open its credential form against the still-
  // connected org (see the note above). Omit on surfaces with no form to
  // re-open — the control simply doesn't render.
  onReplace?: () => void
  // The connection's host and/or token come from TRIAGE_FACTORY_* env vars
  // (local mode only). Those win on read, so a credential typed here would be
  // stored and then ignored — the group states that and withholds the rebind
  // rather than offering a control that appears to work and doesn't.
  envProvided?: boolean
  onDisconnected?: () => void
  showBaseUrl?: boolean
  bare?: boolean
}) {
  const cloud = deployment === 'cloud'
  const field = bare ? glassInputClass : inputClass
  // An env-supplied connection can be reported but not replaced from here.
  const canReplace = !!onReplace && !envProvided

  const disconnect = async () => {
    // One request: the credential resource's DELETE clears the stored
    // credential AND the URL column in a single server-side transaction. This
    // used to be two calls (clear the secrets, then a sparse settings save to
    // clear the column) with a real "credentials were removed, but clearing the
    // saved URL failed" half-state to explain to the user.
    if (!orgId) {
      toast.error('No organization context — reload and try again.')
      return
    }
    const res = await disconnectJira(orgId)
    if (!res.ok) {
      toast.error(res.error)
      return
    }
    // Local mode only: TRIAGE_FACTORY_* env vars shadow the vault on read, so
    // the disconnect landed but the value keeps surfacing until they're unset.
    if (res.warning) toast.error(res.warning)
    onChange({ jira_url: '', jira_pat: '', jira_email: '', jira_api_token: '' })
    onDisconnected?.()
  }

  const body = (
    <>
      {/* The carded layout titles itself ("Jira connection") + carries the
          Disconnect on the heading row; bare relies on the host's section
          title and tucks Disconnect above the status line. */}
      {!bare && (
        <div className="mb-4 flex items-center justify-between">
          <h2 className="text-body font-medium text-ink-2">Jira connection</h2>
          {connected && (
            <div className="flex items-center gap-3">
              {canReplace && (
                <button
                  type="button"
                  onClick={onReplace}
                  className="text-reported text-warm transition-colors hover:underline"
                >
                  Replace credential
                </button>
              )}
              <button
                type="button"
                onClick={disconnect}
                className="text-reported text-alarm transition-colors hover:text-alarm/80"
              >
                Disconnect
              </button>
            </div>
          )}
        </div>
      )}

      {!connected ? (
        /* The credential fields. The container's Continue (setup) / Save
           (Settings) performs the connect — no button here. */
        <div className="space-y-3">
          {showBaseUrl && (
            <Field label="Base URL">
              <input
                type="url"
                placeholder="https://jira.yourcompany.com"
                value={value.jira_url}
                onChange={(e) => onChange({ jira_url: e.target.value })}
                className={field}
              />
            </Field>
          )}
          {cloud ? (
            <>
              <Field label="Account email">
                <input
                  type="email"
                  placeholder="bot@yourcompany.com"
                  autoComplete="email"
                  value={value.jira_email}
                  onChange={(e) => onChange({ jira_email: e.target.value })}
                  className={field}
                />
              </Field>
              <Field label="API token">
                <input
                  type="password"
                  placeholder="Atlassian API token"
                  value={value.jira_api_token}
                  onChange={(e) => onChange({ jira_api_token: e.target.value })}
                  className={field}
                />
                <p className="mt-1.5 text-reported leading-snug text-ink-3">
                  Create one at{' '}
                  <a
                    href="https://id.atlassian.com/manage-profile/security/api-tokens"
                    target="_blank"
                    rel="noreferrer"
                    className="text-warm hover:underline"
                  >
                    Atlassian API token settings
                  </a>
                  . Paired with the account email above for Basic auth.
                </p>
              </Field>
            </>
          ) : (
            <Field label="Personal Access Token">
              <input
                type="password"
                placeholder="Jira Personal Access Token"
                value={value.jira_pat}
                onChange={(e) => onChange({ jira_pat: e.target.value })}
                className={field}
              />
            </Field>
          )}
        </div>
      ) : (
        /* Connected — status + (bare) the Disconnect the carded layout puts on
           its heading row. */
        <div className="space-y-2">
          <div className="flex items-center gap-2 rounded-xl border border-line-1 bg-tint-2 px-4 py-2.5">
            <div className="h-1.5 w-1.5 shrink-0 rounded-full bg-tint-2" />
            <span className="text-ui text-ink-2">
              Connected to {value.jira_url.replace(/^https?:\/\//, '')}
            </span>
            {bare && (
              <div className="ml-auto flex items-center gap-3">
                {canReplace && (
                  <button
                    type="button"
                    onClick={onReplace}
                    className="text-reported text-warm transition-colors hover:underline"
                  >
                    Replace credential
                  </button>
                )}
                <button
                  type="button"
                  onClick={disconnect}
                  className="text-reported text-alarm transition-colors hover:text-alarm/80"
                >
                  Disconnect
                </button>
              </div>
            )}
          </div>
          {/* Says why there's no Replace here, rather than leaving its absence
              to be discovered. */}
          {envProvided && (
            <p className="text-reported leading-relaxed text-ink-3">
              This connection comes from <code>TRIAGE_FACTORY_JIRA_*</code> environment variables,
              which take precedence over anything stored here — change it where the server is
              started, or unset those variables to manage it from Settings.
            </p>
          )}
        </div>
      )}
    </>
  )

  return bare ? body : <Section>{body}</Section>
}
