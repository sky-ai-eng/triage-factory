// The Linear tracker body, shown only when Linear is the chosen tracker (the
// step is gated visible on tracker === 'linear' in steps.tsx):
//
//   LinearAccessStep — Install Triage Factory when a Linear OAuth app
//                      resolves, with a personal API key and an app of the
//                      org's own behind one link; else the API-key field,
//                      with the shared LinearOAuthAppCard offered to register
//                      an app, since an org still in setup cannot reach
//                      Settings; saving it makes Install available in place.
//                      No URL step and no deployment step: Linear has one host
//                      and the workspace is learned from the credential. The
//                      step's Continue binds a typed key (steps.tsx), the same
//                      shape as the Jira access step; the group keeps the
//                      install navigation and the disconnect.
//
// Reuse rule: composes the shared LinearAccessGroup and LinearOAuthAppCard —
// no parallel field UI.

import { useState } from 'react'
import LinearAccessGroup from '../settings/LinearAccessGroup'
import LinearOAuthAppCard from '../settings/LinearOAuthAppCard'
import { InsteadLink, ReuseCredentialCheckbox } from './parts'
import type { StepContext } from './types'

export function LinearAccessStep({ state, patch, orgId, hold, isLocal }: StepContext) {
  const [settingUpApp, setSettingUpApp] = useState(false)
  // With an app to install, Install is the step's one main action and the
  // alternatives wait behind a link. Local state, so a remount closes them.
  const [showAlternatives, setShowAlternatives] = useState(false)
  const connecting = !state.linearConnected
  const installAvailable = state.linearInstallAvailable
  const showKeyField = !installAvailable || showAlternatives
  const offerAlternatives = connecting && installAvailable && !showAlternatives
  const offerOwnApp = connecting && installAvailable && showAlternatives && !!orgId
  const offerApp = connecting && !installAvailable && !!orgId

  const appCard = orgId && (
    <div className="rounded-2xl border border-line-1 p-4">
      <LinearOAuthAppCard
        orgId={orgId}
        // "Set up your own Linear app" was already the deliberate click, so
        // the card opens on its form rather than behind its own button.
        openForm={offerOwnApp}
        onStatus={(st) => {
          if (st.install_available !== state.linearInstallAvailable) {
            patch({ linearInstallAvailable: st.install_available })
          }
        }}
      />
    </div>
  )
  const appButton = (label: string) => (
    <button
      type="button"
      onClick={() => setSettingUpApp(true)}
      className="text-reported text-warm transition-colors hover:underline"
    >
      {label}
    </button>
  )

  return (
    <div className="space-y-5">
      <LinearAccessGroup
        value={{ linear_api_key: state.org.linear_api_key }}
        onChange={(p) => patch({ org: { ...state.org, ...p } })}
        connected={state.linearConnected}
        authMethod={state.linearAuthMethod}
        boundAs={state.linearBoundAs}
        workspaceUrlKey={state.linearWorkspaceUrlKey}
        installAvailable={installAvailable}
        lastError={state.linearLastError}
        orgId={orgId}
        hold={hold}
        onDisconnected={() =>
          patch({
            linearConnected: false,
            linearWorkspaceUrlKey: '',
            linearBoundAs: '',
            linearAuthMethod: '',
            linearLastError: '',
            org: { ...state.org, linear_api_key: '' },
          })
        }
        bare
        showKeyField={showKeyField}
      />
      {offerAlternatives && (
        <InsteadLink
          label="Use a personal API key or your own Linear app instead"
          hint="A key acts as the person it belongs to. An app of your own is for a Linear workspace that needs one it controls."
          onClick={() => setShowAlternatives(true)}
        />
      )}
      {offerOwnApp && (settingUpApp ? appCard : appButton('Set up your own Linear app'))}
      {offerApp &&
        (settingUpApp
          ? appCard
          : appButton('Install Triage Factory as an app instead of using a personal key'))}
      {/* Local-mode only: also bind the just-typed org key as the operator's
          own Linear credential, so they don't paste it again on the User
          step. Shown only while the key field is (nothing to reuse once
          connected, and no key to type while it waits behind the link). */}
      {isLocal && connecting && showKeyField && (
        <ReuseCredentialCheckbox
          checked={state.duplicateLinearToUser}
          onChange={(v) => patch({ duplicateLinearToUser: v })}
          label="Also use this API key as my own Linear identity"
          hint="Saves re-entering it on the “Your Linear access” step. Your key is stored (not just your username) so Triage Factory can act as you in Linear."
        />
      )}
    </div>
  )
}
