// SpendRing's default name shortener, its own module because react-refresh
// objects to a component file that also exports functions.
//
// A model id is long for two reasons a reader does not need in a 132px hole: a
// vendor prefix everything shares, and a build date. Strip both and
// `claude-sonnet-4-20250514` becomes `sonnet-4`, which fits.
//
// This is the real fix for a name too long for the aperture, and it took two
// wrong ones to find. Wrapping to two lines broke at a hyphen and left the
// label reading "SONNET-". Letting the name cross the ring behind a masked
// channel bit a visible gap out of the silhouette — which is the ring's whole
// readability — and 8.5px tracked type over a saturated band is a smear at any
// ink. Both were treatments for a string that should not have been that long
// on screen.
//
// A caller with a real display name should pass `shorten={(s) => s}` and hand
// in the name it wants. Anything still too long ellipses in the hole.
export const shortModelName = (s: string): string =>
  String(s)
    .replace(/^(claude|anthropic)-/i, '')
    .replace(/-\d{8}$/, '')
