// Maps a successful bring-your-own-App import to the wizard-state slice the
// manifest register path also patches, so the setup step and the Settings switch
// flow reflect a freshly-connected App identically (one mapping, no drift).

import type { GitHubAppImportResult } from '../../lib/githubApp'
import type { WizardState } from './types'

export function appImportedPatch(result: GitHubAppImportResult): Partial<WizardState> {
  const app = result.app
  return {
    githubAppSource: 'existing',
    githubAppRegistered: !!app,
    // active=false ⇒ staged (imported while an org PAT is live; the PAT stays the
    // live credential until cutover). active=true ⇒ immediately live.
    githubAppStaged: !!app && !app.active,
    githubAppSlug: app?.slug ?? '',
    githubAppOwnerType: app?.owner_type === 'org' ? 'org' : 'user',
    // No installations are mirrored at import (the install step reconciles), so
    // this is normally 0 — the install step's refresh-then-verify discovers them.
    githubAppInstalled: result.installations.length > 0,
    githubAppInstallCount: result.installations.length,
    githubReady: true,
  }
}
