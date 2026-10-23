// The install ceremony's return path: it comes back to the page it started
// from with the query that page's routing reads, minus a previous outcome.
import { describe, it, expect } from 'vitest'

import {
  linearInstallReturnTo,
  linearInstallStartURL,
  linearTeamNames,
  linearUnreadableText,
} from './linearConnect'

describe('linearInstallReturnTo', () => {
  it('keeps the routing query', () => {
    expect(linearInstallReturnTo('http://localhost:3000/orgs/o-1/org?tab=settings')).toBe(
      '/orgs/o-1/org?tab=settings',
    )
    expect(linearInstallReturnTo('http://localhost:3000/settings?tab=org')).toBe(
      '/settings?tab=org',
    )
  })

  it('drops a previous outcome and the fragment', () => {
    expect(
      linearInstallReturnTo(
        'http://localhost:3000/settings?linear=installed&tab=org&linear_error=denied#linear',
      ),
    ).toBe('/settings?tab=org')
    expect(linearInstallReturnTo('http://localhost:3000/setup?linear_error=denied')).toBe('/setup')
  })

  it('rides the start URL encoded', () => {
    expect(linearInstallStartURL('o 1', '/orgs/o-1/org?tab=settings')).toBe(
      '/api/orgs/o%201/linear/install/start?return_to=%2Forgs%2Fo-1%2Forg%3Ftab%3Dsettings',
    )
  })
})

describe('linearTeamNames', () => {
  const a = { id: 'a', key: 'ENG', name: 'Engineering' }
  const b = { id: 'b', key: 'OPS', name: '' }
  const c = { id: 'c', key: '', name: '' }

  it('lists teams for a sentence', () => {
    expect(linearTeamNames([a])).toBe('Engineering (ENG)')
    expect(linearTeamNames([a, b])).toBe('Engineering (ENG) and OPS')
    expect(linearTeamNames([a, b, c])).toBe('Engineering (ENG), OPS and c')
  })
})

describe('linearUnreadableText', () => {
  const tfac = { id: 't', key: 'TFAC', name: 'Triage Factory' }
  const ops = { id: 'o', key: 'OPS', name: 'Operations' }

  it('points an installed app at sharing the team', () => {
    expect(linearUnreadableText([tfac], true)).toBe(
      "This connection can't read the tracked Linear team Triage Factory (TFAC). " +
        "Triage Factory isn't seeing changes to its issues or finding new ones. " +
        'Share it with the app on its page in Linear, or stop tracking it.',
    )
  })

  it('points a key at a member of the teams', () => {
    expect(linearUnreadableText([tfac, ops], false)).toBe(
      "This connection can't read the tracked Linear teams Triage Factory (TFAC) and Operations (OPS). " +
        "Triage Factory isn't seeing changes to their issues or finding new ones. " +
        'Connect a key whose owner is a member, or stop tracking them.',
    )
  })
})
