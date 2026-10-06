import { Section, Field, inputClass, glassInputClass } from './primitives'
import { toast } from '../../components/Toast/toastStore'
import { disconnectLinear } from './linearConnect'

/**
 * LinearAccessGroup is the org-level Linear credential field group — the
 * sibling of JiraAccessGroup, and controlled the same way: the container owns
 * the typed key and the connection, and its Continue (setup wizard) / Save
 * (Settings) performs the bind via connectLinear. The group carries no Connect
 * button.
 *
 * What it owns is the disconnect, an inline action on the connected state, and
 * `onReplace` — a request to rebind, which the container answers by rendering
 * this group unconnected against the still-connected org, so rotating a key
 * never opens a window with no credential and the poller stopped.
 *
 * One field: a personal API key. Linear has one host, so there is no URL, and
 * one org-credential shape this build can bind, so there is no deployment
 * choice. The connected state names who the key validated as and the
 * workspace it belongs to, both learned from the key at bind time.
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
  boundAs = '',
  workspaceUrlKey = '',
  orgId,
  onReplace,
  envProvided = false,
  onDisconnected,
  bare = false,
}: {
  value: { linear_api_key: string }
  onChange: (patch: { linear_api_key: string }) => void
  connected: boolean
  /** Who the bound key validated as, and the url key of its workspace — the
   *  connected line. Either may be empty: a key the local environment
   *  supplies was never bound, so neither was learned. */
  boundAs?: string
  workspaceUrlKey?: string
  /** Org the credential belongs to — the DELETE is org-scoped by path. */
  orgId: string | null
  onReplace?: () => void
  /** TRIAGE_FACTORY_LINEAR_API_KEY supplies the key (local mode only). It wins
   *  on read, so a key typed here would be stored and then ignored — the group
   *  says so and withholds the rebind rather than offering a control that
   *  appears to work and doesn't. */
  envProvided?: boolean
  onDisconnected?: () => void
  bare?: boolean
}) {
  const field = bare ? glassInputClass : inputClass
  const canReplace = !!onReplace && !envProvided

  const disconnect = async () => {
    if (!orgId) {
      toast.error('No organization context — reload and try again.')
      return
    }
    const res = await disconnectLinear(orgId)
    if (!res.ok) {
      toast.error(res.error)
      return
    }
    // Local mode only: the env overlay keeps supplying the key after the
    // stored one is gone.
    if (res.warning) toast.error(res.warning)
    onChange({ linear_api_key: '' })
    onDisconnected?.()
  }

  const statusLine = [
    boundAs ? `Connected as ${boundAs}` : 'Connected',
    workspaceUrlKey ? `in linear.app/${workspaceUrlKey}` : '',
  ]
    .filter(Boolean)
    .join(' ')

  const actions = (
    <div className="flex items-center gap-3">
      {canReplace && (
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

      {!connected ? (
        <div className="space-y-3">
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
          {envProvided && (
            <p className="text-reported leading-relaxed text-ink-3">
              This key comes from the <code>TRIAGE_FACTORY_LINEAR_API_KEY</code> environment
              variable, which takes precedence over anything stored here — change it where the
              server is started, or unset it to manage the key from Settings.
            </p>
          )}
        </div>
      )}
    </>
  )

  return bare ? body : <Section>{body}</Section>
}
