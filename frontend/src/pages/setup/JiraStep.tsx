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
//                    suppressed). No separate Connect button: the step's
//                    Continue performs the connect (steps.tsx), the same shape
//                    as the GitHub PAT step (OAuth is still blocked, PAT-only).
//                    JiraAccessGroup keeps only the disconnect affordance.
//
// Reuse rule: composes the shared JiraAccessGroup — no parallel field UI. The
// URL field is the shared UrlField, the same one the GitHub URL step uses.

import JiraAccessGroup from '../settings/JiraAccessGroup'
import { UrlField, ReuseCredentialCheckbox } from './parts'
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

// JiraAccessStep body — just the PAT field (base URL suppressed). The connect
// is the step's Continue (steps.tsx calls connectJira and patches
// jiraConnected on success), so there's no Connect button here. A disconnect
// clears the connection and re-opens the URL step (jiraUrlConfirmed drops, so
// the upstream Jira URL step becomes incomplete again).
export function JiraAccessStep({ state, patch, isLocal }: StepContext) {
  return (
    <div className="space-y-5">
      <JiraAccessGroup
        value={{ jira_url: state.org.jira_url, jira_pat: state.org.jira_pat }}
        onChange={(p) => patch({ org: { ...state.org, ...p } })}
        connected={state.jiraConnected}
        onDisconnected={() =>
          // JiraAccessGroup blanks jira_url via its onChange immediately before
          // this fires; rebuild org from this render's state (which still holds
          // the URL) and drop only the PAT, so a same-session reconnect keeps the
          // URL pre-filled. jiraUrlConfirmed:false re-opens the URL step to
          // re-probe before reconnecting.
          patch({
            jiraConnected: false,
            jiraUrlConfirmed: false,
            org: { ...state.org, jira_pat: '' },
          })
        }
        showBaseUrl={false}
        bare
      />
      {/* Local-mode only: reuse the org Jira PAT as the operator's own STORED
          Jira credential so they don't paste it again on the User step. Shown
          only while connecting (no PAT to reuse once connected). */}
      {isLocal && !state.jiraConnected && (
        <ReuseCredentialCheckbox
          checked={state.duplicateJiraToUser}
          onChange={(v) => patch({ duplicateJiraToUser: v })}
          label="Also use this token as my own Jira identity"
          hint="Saves re-entering it on the “Your Jira access” step. Like the org connection, this token is stored so Triage Factory can act as you on Jira."
        />
      )}
    </div>
  )
}
