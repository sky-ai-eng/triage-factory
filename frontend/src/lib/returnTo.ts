// safeReturnTo is raw when it is a path on this origin, and fallback
// otherwise. It applies the server's rules (normalizeReturnTo) and then the one
// that decides: the URL the browser builds from raw has this origin.
//
// A page that navigates to a return_to it read from its own query string needs
// this even though the server checks the same value on its own redirects. React
// Router hands a URL that pushState refuses as cross-origin to
// window.location.assign, so a path the browser resolves elsewhere leaves the
// app: `/\evil.example` (a backslash is a slash to the URL parser) and
// `/<tab>/evil.example` (tabs and newlines are stripped before parsing) both
// resolve to `//evil.example`.
export function safeReturnTo(raw: string | null, fallback: string): string {
  if (!raw || !raw.startsWith('/') || raw.startsWith('//') || raw.includes('\\')) {
    return fallback
  }
  let decoded: string
  try {
    decoded = decodeURIComponent(raw)
  } catch {
    return fallback
  }
  if (decoded.includes('\\')) return fallback
  // eslint-disable-next-line no-control-regex
  if (/[\u0000-\u001f\u007f]/.test(raw)) return fallback
  const base = 'https://return-to.invalid'
  try {
    if (new URL(raw, base).origin !== base) return fallback
  } catch {
    return fallback
  }
  return raw
}
