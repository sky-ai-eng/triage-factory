// How the app names and draws the identity-provider product behind an SSO
// connection (the `idp` vocabulary in types.ts). Shared by the personal page,
// where a login wears its door's mark, and the org SSO settings, where an admin
// corrects the derived value, so both name a provider the same way.

/** The product's name. `other` has none — it names no product — so a caller
 *  falls back to its own provider-neutral wording. */
export const IDP_NAMES: Partial<Record<string, string>> = {
  entra: 'Microsoft Entra',
  okta: 'Okta',
  google: 'Google',
  onelogin: 'OneLogin',
  ping: 'Ping',
}

/** The vendor mark's path under `public/idp/`, or null for a provider with no
 *  mark there (OneLogin, Ping, other, not known). Okta ships a dark-ground
 *  variant, so it follows the theme; the others are legible on both grounds. */
export function idpMarkSrc(idp: string | undefined, theme: 'light' | 'dark'): string | null {
  switch (idp) {
    case 'entra':
    case 'google':
      return `/idp/${idp}.svg`
    case 'okta':
      return theme === 'dark' ? '/idp/okta-dark.svg' : '/idp/okta.svg'
    default:
      return null
  }
}
