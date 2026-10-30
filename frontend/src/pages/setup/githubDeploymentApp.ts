// The connect step's done state, shared by the step's definition and its body:
// the org is on the deployment App with at least one account connected.

import type { WizardState } from './types'

export function deploymentAppConnected(s: WizardState): boolean {
  return s.githubAppManaged && s.githubAppInstalled
}
