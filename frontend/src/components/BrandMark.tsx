// BrandMark is the Triage Factory mark, drawn as strokes rather than shipped as
// an image so it inherits the ink of whatever it sits in. The reference set has
// a light and a dark cut of it, and their two colors are exactly
// --color-ink-1's light and dark values — so `currentColor` under a text-ink-1
// parent reproduces both cuts with no theme branch here.
//
// The geometry is authored on a 16-unit grid at stroke-width 2 and is meant to
// be read small; `size` scales the box, never the weight, so the mark thickens
// relative to nothing as it grows. The same path is the favicon
// (public/favicon.svg), which carries its own colors because a browser tab
// cannot reach our tokens.
export default function BrandMark({ size = 16, className }: { size?: number; className?: string }) {
  return (
    <svg
      xmlns="http://www.w3.org/2000/svg"
      width={size}
      height={size}
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
      // Decorative: every surface that places it also names the product in text
      // beside it, so announcing it again would double the name.
      aria-hidden="true"
      focusable="false"
      className={className}
    >
      <path d="M4 6.7 L8 3 L12 6.7 M8 6.7 V13.2 M8.2 9.9 H11.9" />
    </svg>
  )
}
