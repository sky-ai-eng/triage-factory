// The Jira tracker bodies — the mirror of GitHub's URL/mode/access split, shown
// only when Jira is the chosen tracker (the steps are gated visible on
// tracker === 'jira' in steps.tsx):
//
//   JiraUrlStep    — the Jira base-URL field. Continue runs the URL-only
//                    reachability probe (no separate "Verify" button); an
//                    unreachable host bounces Continue and turns the box red.
//                    No prefill — every Jira host is bespoke (Cloud or Data
//                    Center). The probe lives in the step's persist (steps.tsx).
//   JiraModeStep   — the deployment picker (Cloud vs Data Center), the Jira
//                    mirror of GitHub's access-method picker. Action-on-click
//                    (selfAdvancing): the choice gates which credential fields
//                    the access step shows and which scheme the connect sends,
//                    so the choice is explicit rather than sniffed from the URL.
//   JiraAccessStep — the credential sub-step (shared JiraAccessGroup, base URL
//                    suppressed). No separate Connect button: the step's
//                    Continue performs the connect (steps.tsx), the same shape
//                    as the GitHub PAT step. JiraAccessGroup keeps only the
//                    disconnect affordance; the deployment it received decides
//                    the fields (Cloud: email + API token; DC: PAT).
//
// Reuse rule: composes the shared JiraAccessGroup + ChoiceCards — no parallel
// field UI. The URL field is the shared UrlField, the same one the GitHub URL
// step uses; the picker is the shared ChoiceCards the GitHub mode step uses.

import JiraAccessGroup from '../settings/JiraAccessGroup'
import {
  isJiraCloudHost,
  JIRA_DEPLOYMENT_OPTIONS,
  type JiraDeployment,
} from '../settings/jiraConnect'
import { UrlField, ChoiceCards, ReuseCredentialCheckbox } from './parts'
import type { StepContext } from './types'

// JiraUrlStep body — just the base-URL field; the reachability probe is the
// step's Continue. `error` turns the box red on a failed probe, with the
// message in the host's shared error line.
export function JiraUrlStep({ state, patch, error }: StepContext) {
  return (
    <div className="space-y-4">
      <p className="text-[13px] leading-relaxed text-text-secondary">
        Where your Jira lives. We&rsquo;ll confirm it&rsquo;s reachable, then pick how to connect.
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

// JiraModeStep body — the deployment picker. Action-on-click (the step is
// selfAdvancing): clicking a panel records the choice and advances to the
// credential step, whose fields it determines. The card matching the URL's
// host shape carries a hint, so the common case (a *.atlassian.net URL → Cloud)
// is a one-tap confirm without forcing it.
export function JiraModeStep({ state, patch, advance }: StepContext) {
  const choose = (kind: JiraDeployment) => {
    patch({ jiraDeployment: kind })
    advance?.()
  }
  const detected: JiraDeployment = isJiraCloudHost(state.org.jira_url) ? 'cloud' : 'data_center'
  const options = JIRA_DEPLOYMENT_OPTIONS.map((o) => ({
    ...o,
    status: o.kind === detected ? 'Matches your Jira URL' : undefined,
  }))
  return (
    <div className="space-y-5">
      <div className="space-y-1.5">
        <h2 className="text-[19px] font-medium tracking-tight text-text-primary">
          Which Jira are you connecting?
        </h2>
        <p className="text-[13px] leading-relaxed text-text-tertiary">
          Atlassian Cloud and self-hosted Data Center use different credentials, so we tailor the
          next step to your deployment.
        </p>
      </div>
      <ChoiceCards
        ariaLabel="Jira deployment"
        options={options}
        selected={state.jiraDeployment}
        onChoose={choose}
      />
    </div>
  )
}

// JiraAccessStep body — the credential fields (base URL suppressed). The connect
// is the step's Continue (steps.tsx calls connectJira and patches jiraConnected
// on success), so there's no Connect button here. A disconnect clears the
// connection and re-opens the URL step (jiraUrlConfirmed drops). The deployment
// chosen in JiraModeStep selects which fields render.
export function JiraAccessStep({ state, patch, isLocal }: StepContext) {
  const deployment = state.jiraDeployment ?? 'data_center'
  return (
    <div className="space-y-5">
      <JiraAccessGroup
        value={{
          jira_url: state.org.jira_url,
          jira_pat: state.org.jira_pat,
          jira_email: state.org.jira_email,
          jira_api_token: state.org.jira_api_token,
        }}
        onChange={(p) => patch({ org: { ...state.org, ...p } })}
        connected={state.jiraConnected}
        deployment={deployment}
        onDisconnected={() =>
          // JiraAccessGroup blanks the credential fields via its onChange immediately
          // before this fires; rebuild org from this render's state (which still holds
          // the URL) and drop only the secrets, so a same-session reconnect keeps the
          // URL. jiraUrlConfirmed:false re-opens the URL step to re-probe, and
          // jiraDeployment:null re-opens the deployment picker so the operator
          // re-picks explicitly — host-shape inference can be wrong (Cloud custom
          // domains, a changed URL), and the backend treats the credential shape as
          // authoritative, so we never silently reconnect under a stale guess.
          patch({
            jiraConnected: false,
            jiraUrlConfirmed: false,
            jiraDeployment: null,
            org: { ...state.org, jira_pat: '', jira_email: '', jira_api_token: '' },
          })
        }
        showBaseUrl={false}
        bare
      />
      {/* Local-mode only: reuse the just-typed org Jira credential as the
          operator's own STORED Jira credential so they don't paste it again on
          the User step. Shown only while connecting (nothing to reuse once
          connected). The credential reused follows the deployment: Cloud reuses
          the email + API token, Data Center the PAT. */}
      {isLocal && !state.jiraConnected && (
        <ReuseCredentialCheckbox
          checked={state.duplicateJiraToUser}
          onChange={(v) => patch({ duplicateJiraToUser: v })}
          label={
            deployment === 'cloud'
              ? 'Also use this email + API token as my own Jira identity'
              : 'Also use this token as my own Jira identity'
          }
          hint="Saves re-entering it on the “Your Jira access” step. Your credential is stored (not just your username) so Triage Factory can act as you on Jira."
        />
      )}
    </div>
  )
}
