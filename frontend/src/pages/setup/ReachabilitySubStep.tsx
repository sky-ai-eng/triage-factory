// The URL reachability sub-step shared by the GitHub and Jira steps: a base-URL
// field with a one-tap "Verify" that probes the host (URL-only, no creds) and,
// on success, collapses to a compact "✓ <label>: <host>" bar with an Edit
// affordance. Reachability is deliberately separate from auth — confirming the
// host answers before any credential is entered — so this owns ONLY the URL,
// then hands off to the access sub-step (PAT connect / App register).
//
// One component, two callers: GitHub prefills https://github.com (the common
// case becomes a confirm); Jira prefills nothing (every Jira host is bespoke).
// The probe fn is injected so the same chrome drives both endpoints.

import { useEffect, useState } from 'react'
import { Check, Pencil } from 'lucide-react'
import { inputClass } from '../settings/primitives'
import { hostOf, reachabilityMessage, type ReachabilityResult } from '../../lib/reachability'

export default function ReachabilitySubStep({
  label,
  prefill,
  value,
  confirmed,
  check,
  onConfirm,
  onEdit,
  helpText,
}: {
  // Display label for the host class, e.g. "GitHub URL".
  label: string
  // Seed value when the field is empty (GitHub: "https://github.com"; Jira: "").
  prefill: string
  // The currently-confirmed URL (from wizard state), shown in the collapsed bar.
  value: string
  // Whether the URL is confirmed (collapsed bar) vs. being entered.
  confirmed: boolean
  // URL-only reachability probe (checkGitHubReachability / checkJiraReachability).
  check: (url: string) => Promise<ReachabilityResult>
  // Called with the entered URL once it probes reachable.
  onConfirm: (url: string) => void
  // Re-expand the field to change a confirmed URL.
  onEdit: () => void
  helpText?: string
}) {
  const [draft, setDraft] = useState(value || prefill)
  const [checking, setChecking] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Re-seed the input from the parent's value while the field is open. The
  // parent can clear `value` as it re-opens — e.g. a Jira disconnect blanks
  // jira_url and drops confirmed — and the draft would otherwise keep showing
  // the stale URL. `value` only changes on a confirm (never mid-typing, which
  // updates the local draft alone), so this can't clobber what the user types.
  useEffect(() => {
    if (!confirmed) setDraft(value || prefill)
  }, [confirmed, value, prefill])

  if (confirmed) {
    return (
      <div className="flex items-center gap-2.5 rounded-xl border border-claim/15 bg-claim/[0.06] px-4 py-2.5">
        <Check size={14} strokeWidth={3} className="shrink-0 text-claim" aria-hidden />
        <span className="text-[12px] text-claim">
          {label}: {hostOf(value)}
        </span>
        <button
          type="button"
          onClick={onEdit}
          className="ml-auto inline-flex items-center gap-1 text-[11px] text-text-tertiary transition-colors hover:text-accent"
        >
          <Pencil size={11} aria-hidden />
          Edit
        </button>
      </div>
    )
  }

  const verify = async () => {
    const url = draft.trim()
    if (url === '') {
      setError('Enter a URL.')
      return
    }
    setChecking(true)
    setError(null)
    try {
      const result = await check(url)
      if (result.reachable) {
        onConfirm(url)
        return
      }
      setError(reachabilityMessage(result))
    } finally {
      setChecking(false)
    }
  }

  return (
    <div className="space-y-2">
      <label className="block">
        <span className="mb-1.5 block text-[11px] text-text-tertiary">{label}</span>
        <input
          type="url"
          value={draft}
          placeholder={prefill || 'https://…'}
          onChange={(e) => {
            setDraft(e.target.value)
            setError(null)
          }}
          onKeyDown={(e) => {
            if (e.key === 'Enter') {
              e.preventDefault()
              void verify()
            }
          }}
          className={inputClass}
        />
      </label>
      {helpText && <p className="text-[11px] leading-relaxed text-text-tertiary">{helpText}</p>}
      {error && <p className="text-[12px] text-[var(--color-dismiss)]">{error}</p>}
      <button
        type="button"
        onClick={verify}
        disabled={checking}
        className="rounded-xl border border-accent/30 bg-accent/[0.06] px-4 py-2 text-[13px] font-medium text-accent transition-colors hover:bg-accent/[0.12] disabled:opacity-50"
      >
        {checking ? 'Checking…' : 'Verify URL'}
      </button>
    </div>
  )
}
