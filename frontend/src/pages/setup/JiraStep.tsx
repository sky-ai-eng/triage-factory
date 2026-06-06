// The Jira tracker bodies — the mirror of GitHub's URL/access split, shown only
// when Jira is the chosen tracker (the steps are gated visible on
// tracker === 'jira' in steps.tsx):
//
//   JiraUrlStep    — the Jira base-URL field. Continue runs the URL-only
//                    reachability probe (no separate "Verify" button); an
//                    unreachable host bounces Continue and turns the box red.
//                    No prefill — every Jira host is bespoke (Cloud or Data
//                    Center). The probe lives in the step's persist (steps.tsx).
//   JiraAccessStep — the PAT-auth sub-step (shared JiraAccessGroup, base URL
//                    suppressed). JiraAccessGroup owns the connect/disconnect
//                    lifecycle and its own Connect button (OAuth is still
//                    blocked, PAT-only); the step's Continue gates on the
//                    connected flag it reports back via onConnected.
//
// Reuse rule: composes the shared JiraAccessGroup — no parallel field UI. The
// URL field is the shared UrlField, the same one the GitHub URL step uses.

import JiraAccessGroup from '../settings/JiraAccessGroup'
import { UrlField } from './parts'
import type { StepContext } from './types'

// JiraUrlStep body — just the base-URL field; the reachability probe is the
// step's Continue. `error` turns the box red on a failed probe, with the
// message in the host's shared error line.
export function JiraUrlStep({ state, patch, error }: StepContext) {
  return (
    <div className="space-y-4">
      <p className="text-[13px] leading-relaxed text-text-secondary">
        Where your Jira lives. We&rsquo;ll confirm it&rsquo;s reachable, then connect with a token.
      </p>
      <UrlField
        label="Jira URL"
        value={state.org.jira_url}
        onChange={(url) => patch({ org: { ...state.org, jira_url: url } })}
        placeholder="https://jira.yourcompany.com"
        helpText="Your Jira Cloud or Data Center base URL."
        invalid={!!error}
      />
    </div>
  )
}

// JiraAccessStep body — the PAT connect. JiraAccessGroup posts /api/jira/connect
// on its Connect button and reports success via onConnected; the step's
// Continue then advances (it gates on state.jiraConnected). A disconnect clears
// the connection and re-opens the URL step (jiraUrlConfirmed drops, so the
// upstream Jira URL step becomes incomplete again).
export function JiraAccessStep({ state, patch }: StepContext) {
  return (
    <JiraAccessGroup
      value={{ jira_url: state.org.jira_url, jira_pat: state.org.jira_pat }}
      onChange={(p) => patch({ org: { ...state.org, ...p } })}
      connected={state.jiraConnected}
      onConnected={(url) =>
        patch({
          jiraConnected: true,
          jiraUrlConfirmed: true,
          org: { ...state.org, jira_url: url, jira_pat: '' },
        })
      }
      onDisconnected={() =>
        patch({
          jiraConnected: false,
          jiraUrlConfirmed: false,
          org: { ...state.org, jira_url: '', jira_pat: '' },
        })
      }
      showBaseUrl={false}
    />
  )
}
