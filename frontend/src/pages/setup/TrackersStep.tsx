// The Trackers step body (optional step 2). Trackers are the issue/work
// integrations — Jira and Linear — distinct from GitHub (the backbone). The
// choice is org-level and one-at-a-time: None, Jira, or Linear. Linear is a
// disabled "coming soon" placeholder (no poller, no credentials, no ingestion —
// explicitly out of scope), so it can be looked at but never selected.
//
// Jira, when chosen, runs the same two-stage shape as GitHub: a URL
// reachability sub-step → the PAT-auth sub-step (the shared JiraAccessGroup in
// showBaseUrl=false mode; OAuth is still blocked, PAT-only). The step is
// optional, so None is a valid end state — but a half-picked Jira (selected,
// not connected) is caught by the step's validate so it can't silently advance.

import { useState } from 'react'
import { Clock } from 'lucide-react'
import JiraAccessGroup from '../settings/JiraAccessGroup'
import { checkJiraReachability } from '../../lib/reachability'
import ReachabilitySubStep from './ReachabilitySubStep'
import type { StepContext, TrackerKind } from './types'

interface TrackerCard {
  kind: TrackerKind
  title: string
  blurb: string
  disabled?: boolean
}

const CARDS: TrackerCard[] = [
  { kind: 'none', title: 'None', blurb: 'GitHub only — add a tracker later in Settings.' },
  { kind: 'jira', title: 'Jira', blurb: 'Track Jira issues alongside your PRs.' },
  { kind: 'linear', title: 'Linear', blurb: 'Coming soon.', disabled: true },
]

export default function TrackersStep({ state, patch }: StepContext) {
  // The Jira URL is "confirmed" once it has probed reachable. A connected org
  // skips to the connected state; otherwise the reachability sub-step leads.
  const [jiraUrlConfirmed, setJiraUrlConfirmed] = useState(() => state.jiraConnected)

  const select = (kind: TrackerKind) => {
    if (kind === 'linear') return
    patch({ tracker: kind })
  }

  return (
    <div className="space-y-4">
      <p className="text-[13px] leading-relaxed text-text-secondary">
        Optionally connect an issue tracker. You can skip this and add one later in Settings.
      </p>

      <div role="radiogroup" aria-label="Issue tracker" className="grid gap-2 sm:grid-cols-3">
        {CARDS.map((card) => {
          const selected = state.tracker === card.kind
          return (
            <button
              key={card.kind}
              type="button"
              role="radio"
              aria-checked={selected}
              aria-disabled={card.disabled}
              disabled={card.disabled}
              onClick={() => select(card.kind)}
              className={`flex flex-col items-start gap-1 rounded-xl border px-3.5 py-3 text-left transition-colors ${
                card.disabled
                  ? 'cursor-default border-border-subtle bg-black/[0.02] opacity-55'
                  : selected
                    ? 'border-accent/50 bg-accent/[0.06] shadow-sm shadow-black/[0.03]'
                    : 'border-border-subtle bg-white/40 hover:border-accent/30 hover:bg-white/70'
              }`}
            >
              <span className="flex items-center gap-1.5">
                <span
                  className={`text-[13px] font-medium ${
                    selected ? 'text-accent' : 'text-text-primary'
                  }`}
                >
                  {card.title}
                </span>
                {card.disabled && (
                  <span className="inline-flex items-center gap-0.5 rounded-full bg-black/[0.05] px-1.5 py-0.5 text-[9px] font-medium uppercase tracking-wide text-text-tertiary">
                    <Clock size={9} aria-hidden />
                    Soon
                  </span>
                )}
              </span>
              <span className="text-[11px] leading-snug text-text-tertiary">{card.blurb}</span>
            </button>
          )
        })}
      </div>

      {state.tracker === 'jira' && (
        <div className="space-y-3 border-t border-border-subtle pt-3">
          <ReachabilitySubStep
            label="Jira URL"
            prefill=""
            value={state.org.jira_url}
            confirmed={jiraUrlConfirmed}
            check={checkJiraReachability}
            onConfirm={(url) => {
              patch({ org: { ...state.org, jira_url: url } })
              setJiraUrlConfirmed(true)
            }}
            onEdit={() => setJiraUrlConfirmed(false)}
            helpText="Your Jira Cloud or Data Center base URL."
          />

          {jiraUrlConfirmed && (
            <JiraAccessGroup
              value={{ jira_url: state.org.jira_url, jira_pat: state.org.jira_pat }}
              onChange={(p) => patch({ org: { ...state.org, ...p } })}
              connected={state.jiraConnected}
              onConnected={(url) =>
                patch({ jiraConnected: true, org: { ...state.org, jira_url: url, jira_pat: '' } })
              }
              onDisconnected={() => {
                patch({ jiraConnected: false, org: { ...state.org, jira_url: '', jira_pat: '' } })
                // Re-open the reachability sub-step: the URL was just cleared,
                // so the collapsed "✓ Jira URL" bar would otherwise show an
                // empty host.
                setJiraUrlConfirmed(false)
              }}
              showBaseUrl={false}
            />
          )}
        </div>
      )}
    </div>
  )
}
