// The one way a workspace connects a GitHub account to the deployment App: a
// reveal, then a login. Whether that account already has the App installed is
// the ceremony's business, not this form's — the panel may not know it, since
// the answer is per-viewer — so the copy promises only what is true either
// way: GitHub will ask to install if it has to, and the person confirms as
// themselves. beforeStart, when given, runs first and can call the whole thing
// off; the own-App card uses it to confirm and tear its App down before the
// ceremony starts.
import { useState } from 'react'
import { startManagedGitHubConnectAccount } from '../../lib/githubApp'

export default function ConnectGitHubAccount({
  orgId,
  label,
  primary = false,
  disabled = false,
  beforeStart,
}: {
  orgId: string
  label: string
  primary?: boolean
  disabled?: boolean
  // Runs after the account is entered and before the ceremony starts. A
  // false return cancels quietly; a throw is shown inline.
  beforeStart?: (account: string) => Promise<boolean>
}) {
  const [open, setOpen] = useState(false)
  const [account, setAccount] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  if (!open) {
    return (
      <button
        type="button"
        disabled={disabled}
        onClick={() => setOpen(true)}
        className={
          primary
            ? 'rounded-full bg-warm px-5 py-2 text-body font-medium text-warm-ink transition-colors hover:bg-warm/90 disabled:opacity-40'
            : 'rounded-xl border border-line-1 px-4 py-2 text-body font-medium text-ink-2 transition-colors hover:border-warm/40 hover:text-ink-1 disabled:opacity-40'
        }
      >
        {label}
      </button>
    )
  }

  const submit = async () => {
    const login = account.trim()
    if (!login || busy) return
    setBusy(true)
    setError(null)
    try {
      if (beforeStart && !(await beforeStart(login))) {
        setBusy(false)
        return
      }
      // Navigates away on success; control never comes back here.
      await startManagedGitHubConnectAccount(orgId, login)
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }

  return (
    <form
      className="w-full space-y-2 rounded-xl border border-line-1 bg-tint-2 p-4"
      onSubmit={(e) => {
        e.preventDefault()
        void submit()
      }}
    >
      <label className="block space-y-1.5">
        <span className="text-label uppercase tracking-wide text-ink-3">GitHub account</span>
        <input
          type="text"
          value={account}
          onChange={(e) => setAccount(e.target.value)}
          placeholder="acme-corp"
          autoComplete="off"
          spellCheck={false}
          className="w-full rounded-xl border border-line-1 bg-ground px-3 py-2 text-body text-ink-1 placeholder:text-ink-4 focus:border-warm/40 focus:outline-none"
        />
      </label>
      <p className="text-reported leading-relaxed text-ink-3">
        The organization or user to connect. If the App isn&rsquo;t installed there yet, GitHub will
        ask you to install it. You&rsquo;ll confirm on GitHub as the account linked to your Triage
        Factory user, which has to be an owner.
      </p>
      {error && <p className="text-ui text-alarm">{error}</p>}
      <div className="flex items-center gap-2">
        <button
          type="submit"
          disabled={busy || !account.trim()}
          className="rounded-full bg-warm px-5 py-2 text-body font-medium text-warm-ink transition-colors hover:bg-warm/90 disabled:opacity-40"
        >
          {busy ? 'Connecting…' : 'Connect'}
        </button>
        <button
          type="button"
          disabled={busy}
          onClick={() => {
            setOpen(false)
            setError(null)
          }}
          className="rounded-full px-4 py-2 text-body text-ink-3 transition-colors hover:text-ink-1"
        >
          Cancel
        </button>
      </div>
    </form>
  )
}
