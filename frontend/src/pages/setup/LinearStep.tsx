// The Linear tracker body, shown only when Linear is the chosen tracker (the
// step is gated visible on tracker === 'linear' in steps.tsx):
//
//   LinearAccessStep — Install Triage Factory when a Linear OAuth app
//                      resolves, else (or as the alternative) the API-key
//                      field, both in the shared LinearAccessGroup. With no
//                      app to install yet, the step offers the shared
//                      LinearOAuthAppCard to register one, since an org still
//                      in setup cannot reach Settings; saving it makes Install
//                      available in place. No URL step and no deployment
//                      step: Linear has one host and the workspace is learned
//                      from the credential. The step's Continue binds a typed
//                      key (steps.tsx), the same shape as the Jira access
//                      step; the group keeps the install navigation and the
//                      disconnect.
//
// Reuse rule: composes the shared LinearAccessGroup and LinearOAuthAppCard —
// no parallel field UI.

import { useState } from 'react'
import LinearAccessGroup from '../settings/LinearAccessGroup'
import LinearOAuthAppCard from '../settings/LinearOAuthAppCard'
import type { StepContext } from './types'

export function LinearAccessStep({ state, patch, orgId, hold }: StepContext) {
  const [settingUpApp, setSettingUpApp] = useState(false)
  const offerApp = !state.linearConnected && !state.linearInstallAvailable && !!orgId

  return (
    <div className="space-y-5">
      <LinearAccessGroup
        value={{ linear_api_key: state.org.linear_api_key }}
        onChange={(p) => patch({ org: { ...state.org, ...p } })}
        connected={state.linearConnected}
        authMethod={state.linearAuthMethod}
        boundAs={state.linearBoundAs}
        workspaceUrlKey={state.linearWorkspaceUrlKey}
        installAvailable={state.linearInstallAvailable}
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
      />
      {offerApp &&
        (settingUpApp ? (
          <div className="rounded-2xl border border-line-1 p-4">
            <LinearOAuthAppCard
              orgId={orgId}
              onStatus={(st) => {
                if (st.install_available !== state.linearInstallAvailable) {
                  patch({ linearInstallAvailable: st.install_available })
                }
              }}
            />
          </div>
        ) : (
          <button
            type="button"
            onClick={() => setSettingUpApp(true)}
            className="text-reported text-warm transition-colors hover:underline"
          >
            Install Triage Factory as an app instead of using a personal key
          </button>
        ))}
      {/* TODO(TFAC-1023): render the local-mode ReuseCredentialCheckbox for
          duplicateLinearToUser here ("Also use this API key as my own Linear
          identity") once the Linear user step exists to consume it. */}
    </div>
  )
}
