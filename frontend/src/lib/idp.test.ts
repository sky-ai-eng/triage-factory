import { describe, expect, it } from 'vitest'
import { idpMarkSrc } from './idp'

describe('idpMarkSrc', () => {
  it('draws the vendors with a mark, and Okta in its dark-ground variant on dark', () => {
    expect(idpMarkSrc('entra', 'dark')).toBe('/idp/entra.svg')
    expect(idpMarkSrc('google', 'light')).toBe('/idp/google.svg')
    expect(idpMarkSrc('okta', 'light')).toBe('/idp/okta.svg')
    expect(idpMarkSrc('okta', 'dark')).toBe('/idp/okta-dark.svg')
  })

  it('has no mark for a provider without one, other, or not known', () => {
    for (const idp of ['onelogin', 'ping', 'other', undefined]) {
      expect(idpMarkSrc(idp, 'light')).toBeNull()
    }
  })
})
