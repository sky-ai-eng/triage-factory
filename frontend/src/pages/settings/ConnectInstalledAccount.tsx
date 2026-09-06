// The named-account way into the deployment App, for an account that already
// has it installed. GitHub's install page offers such an account nothing but
// Configure and never returns to Triage Factory, so the admin names the account
// here instead, authorizes on GitHub as themselves, and the callback finds the
// installation among the ones they can see. A reveal rather than an always-open
// field: Connect is the common path, and this is the one for an install that
// happened somewhere else first.
import { useState } from 'react'
import { startManagedGitHubConnectAccount } from '../../lib/githubApp'

export default function ConnectInstalledAccount({
  orgId,
  disabled = false,
}: {
  orgId: string
  disabled?: boolean
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
        className="rounded-xl border border-line-1 px-4 py-2 text-body font-medium text-ink-2 transition-colors hover:border-warm/40 hover:text-ink-1 disabled:opacity-40"
      >
        Connect an account that already has the App…
      </button>
    )
  }

  const submit = async () => {
    const login = account.trim()
    if (!login || busy) return
    setBusy(true)
    setError(null)
    try {
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
        <span className="text-label uppercase tracking-wide text-ink-3">
          GitHub account with the App installed
        </span>
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
        The organization or user the App is installed on. You&rsquo;ll confirm on GitHub as the
        account linked to your Triage Factory user, which has to be an owner of it.
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
