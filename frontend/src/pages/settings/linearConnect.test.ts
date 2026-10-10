// The install ceremony's return path: it comes back to the page it started
// from with the query that page's routing reads, minus a previous outcome.
import { describe, it, expect } from 'vitest'

import { linearInstallReturnTo, linearInstallStartURL } from './linearConnect'

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
