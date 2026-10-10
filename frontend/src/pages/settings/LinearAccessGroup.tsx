import { useEffect, useState } from 'react'
import { Section, Field, inputClass, glassInputClass } from './primitives'
import { toast } from '../../components/Toast/toastStore'
import {
  disconnectLinear,
  linearInstallErrorText,
  linearInstallReturnTo,
  linearInstallStartURL,
} from './linearConnect'

/**
 * LinearAccessGroup is the org-level Linear credential field group — the
 * sibling of JiraAccessGroup, and controlled the same way: the container owns
 * the typed key and the connection, and its Continue (setup wizard) / Save
 * (Settings) performs the bind via connectLinear.
 *
 * What it owns is the disconnect, an inline action on the connected state,
 * `onReplace` — a request to rebind, which the container answers by rendering
 * this group unconnected against the still-connected org, so rotating a key
 * never opens a window with no credential and the poller stopped — and the
 * Install button.
 *
 * Two ways in. When a Linear OAuth app resolves for the org
 * (`installAvailable`), Install Triage Factory sends a Linear workspace admin
 * through the install ceremony, after which TF acts as its own app user; a
 * personal API key is the alternative. The ceremony is a navigation away and
 * back, so its outcome arrives on this page's URL: an error code there is
 * shown as a banner and then dropped from the URL.
 *
 * Which Linear teams a team tracks is TEAM-level (LinearTeamRulesGroup), so it
 * lives outside this group.
 *
 * bare (default false) drops the carded `<Section>` chrome and uses the flush
 * glass field material, for the setup wizard and the Settings stack.
 */
export default function LinearAccessGroup({
  value,
  onChange,
  connected,
  authMethod = '',
  boundAs = '',
  workspaceUrlKey = '',
  installAvailable = false,
  lastError = '',
  orgId,
  onReplace,
  onDisconnected,
  hold,
  bare = false,
}: {
  value: { linear_api_key: string }
  onChange: (patch: { linear_api_key: string }) => void
  connected: boolean
  /** The connected credential's shape; '' reads as an API key. */
  authMethod?: '' | 'api_key' | 'app_install'
  /** Who the credential validated as — the key's owner, or the installed app
   *  user — and the url key of its workspace: the connected line. */
  boundAs?: string
  workspaceUrlKey?: string
  /** A Linear OAuth app resolves for the org, so Install can be offered. */
  installAvailable?: boolean
  /** The access read's last_error: why an unconnected org lost its credential. */
  lastError?: string
  /** Org the credential belongs to — the DELETE is org-scoped by path. */
  orgId: string | null
  onReplace?: () => void
  /** Runs after a successful disconnect, inside `hold` when one is given. */
  onDisconnected?: () => void | Promise<void>
  /** Runs the whole disconnect under the container's busy guard — the setup
   *  wizard's, so Continue cannot skip the bind on a connection this click is
   *  removing. */
  hold?: (work: () => Promise<void>) => Promise<void>
  bare?: boolean
}) {
  const field = bare ? glassInputClass : inputClass
  const installed = connected && authMethod === 'app_install'

  // The install ceremony reports its failure on the URL it returns to. Read it
  // once, then drop it from the URL so a reload does not show it again.
  const [returnedError, setReturnedError] = useState(() =>
    new URLSearchParams(window.location.search).get('linear_error'),
  )
  // Connecting afterwards answers the error: it was about getting connected,
  // and now the org is. An org that was already connected when the ceremony
  // came back (a key it kept) still reads why the install failed.
  const [wasConnected, setWasConnected] = useState(connected)
  if (connected !== wasConnected) {
    setWasConnected(connected)
    if (connected && returnedError) setReturnedError(null)
  }
  useEffect(() => {
    if (!returnedError) return
    const url = new URL(window.location.href)
    url.searchParams.delete('linear_error')
    window.history.replaceState(window.history.state, '', url.pathname + url.search + url.hash)
  }, [returnedError])
  const banner = linearInstallErrorText(returnedError || (connected ? '' : lastError))

  const install = () => {
    if (!orgId) {
      toast.error('No organization context — reload and try again.')
      return
    }
    window.location.href = linearInstallStartURL(orgId, linearInstallReturnTo(window.location.href))
  }

  const disconnect = async () => {
    if (!orgId) {
      toast.error('No organization context — reload and try again.')
      return
    }
    const work = async () => {
      const res = await disconnectLinear(orgId)
      if (!res.ok) {
        toast.error(res.error)
        return
      }
      onChange({ linear_api_key: '' })
      await onDisconnected?.()
    }
    await (hold ? hold(work) : work())
  }

  const statusLine = [
    installed
      ? boundAs
        ? `Installed as ${boundAs}`
        : 'Installed'
      : boundAs
        ? `Connected as ${boundAs}`
        : 'Connected',
    workspaceUrlKey ? `in linear.app/${workspaceUrlKey}` : '',
  ]
    .filter(Boolean)
    .join(' ')

  const actions = (
    <div className="flex items-center gap-3">
      {!installed && installAvailable && (
        <button
          type="button"
          onClick={install}
          className="text-reported text-warm transition-colors hover:underline"
        >
          Install app instead
        </button>
      )}
      {!installed && onReplace && (
        <button
          type="button"
          onClick={onReplace}
          className="text-reported text-warm transition-colors hover:underline"
        >
          Replace key
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
  )

  const body = (
    <>
      {!bare && (
        <div className="mb-4 flex items-center justify-between">
          <h2 className="text-body font-medium text-ink-2">Linear connection</h2>
          {connected && actions}
        </div>
      )}

      {banner && (
        <p
          role="alert"
          className="mb-3 rounded-xl border border-alarm/30 bg-alarm/5 px-4 py-2.5 text-ui leading-snug text-alarm"
        >
          {banner}
        </p>
      )}

      {!connected ? (
        <div className="space-y-3">
          {installAvailable && (
            <div className="space-y-2">
              <button
                type="button"
                onClick={install}
                className="rounded-full bg-warm px-6 py-2.5 text-body font-medium text-warm-ink shadow-[0_10px_28px_-10px_var(--color-warm)] transition-all hover:bg-warm/90"
              >
                Install Triage Factory
              </button>
              <p className="text-reported leading-snug text-ink-3">
                A Linear workspace admin installs Triage Factory as an app. It then polls and acts
                in Linear as its own app user, which holds no one&rsquo;s personal key and takes no
                seat. Private teams have to be shared with the app on its page in Linear.
              </p>
              <p className="pt-1 text-reported font-medium uppercase tracking-wide text-ink-3">
                Or paste a personal API key
              </p>
            </div>
          )}
          <Field label="API key">
            <input
              type="password"
              placeholder="lin_api_…"
              autoComplete="off"
              value={value.linear_api_key}
              onChange={(e) => onChange({ linear_api_key: e.target.value })}
              className={field}
            />
            <p className="mt-1.5 text-reported leading-snug text-ink-3">
              Create a personal API key in Linear under{' '}
              <a
                href="https://linear.app/settings/account/security"
                target="_blank"
                rel="noreferrer"
                className="text-warm hover:underline"
              >
                Settings → Account → Security &amp; Access
              </a>{' '}
              with Read and Write access. Triage Factory polls and acts in Linear as the person the
              key belongs to.
            </p>
          </Field>
        </div>
      ) : (
        <div className="space-y-2">
          <div className="flex items-center gap-2 rounded-xl border border-line-1 bg-tint-2 px-4 py-2.5">
            <div className="h-1.5 w-1.5 shrink-0 rounded-full bg-warm" />
            <span className="text-ui text-ink-2">{statusLine}</span>
            {bare && <div className="ml-auto">{actions}</div>}
          </div>
          {installed && (
            <p className="text-reported leading-snug text-ink-3">
              Disconnecting revokes Triage Factory&rsquo;s access. The app user stays in the
              workspace&rsquo;s member list until a Linear admin removes the app in Linear.
            </p>
          )}
        </div>
      )}
    </>
  )

  return bare ? body : <Section>{body}</Section>
}
