// The Linear tracker body, shown only when Linear is the chosen tracker (the
// step is gated visible on tracker === 'linear' in steps.tsx):
//
//   LinearAccessStep — the API-key field (shared LinearAccessGroup). No URL
//                      step and no deployment step: Linear has one host and
//                      the workspace is learned from the key. The step's
//                      Continue performs the bind (steps.tsx), the same shape
//                      as the Jira access step; the group keeps only the
//                      disconnect.
//
// Reuse rule: composes the shared LinearAccessGroup — no parallel field UI.

import LinearAccessGroup from '../settings/LinearAccessGroup'
import type { StepContext } from './types'

export function LinearAccessStep({ state, patch, orgId, hold }: StepContext) {
  return (
    <div className="space-y-5">
      <LinearAccessGroup
        value={{ linear_api_key: state.org.linear_api_key }}
        onChange={(p) => patch({ org: { ...state.org, ...p } })}
        connected={state.linearConnected}
        boundAs={state.linearBoundAs}
        workspaceUrlKey={state.linearWorkspaceUrlKey}
        orgId={orgId}
        hold={hold}
        onDisconnected={() =>
          patch({
            linearConnected: false,
            linearWorkspaceUrlKey: '',
            linearBoundAs: '',
            org: { ...state.org, linear_api_key: '' },
          })
        }
        bare
      />
      {/* TODO(TFAC-1023): render the local-mode ReuseCredentialCheckbox for
          duplicateLinearToUser here ("Also use this API key as my own Linear
          identity") once the Linear user step exists to consume it. */}
    </div>
  )
}
