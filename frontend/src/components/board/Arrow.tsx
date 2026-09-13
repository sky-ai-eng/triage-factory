import type { CSSProperties } from 'react'

// Arrow — the one glyph drawn in-system rather than read from the table,
// because it appears on every ask and every activity row and its weight must
// match the 1px hairline exactly. A request never shows elapsed time; it
// shows this arrow. One glyph, every ask — a clock on single asks and an
// arrow on aggregate ones would make two identical affordances look like
// different controls.

export function Arrow({ size = 11, style }: { size?: number; style?: CSSProperties }) {
  return (
    <svg width={size} height={size} viewBox="0 0 12 12" fill="none" aria-hidden style={style}>
      <path
        d="M2 6h7M6.5 3l3 3-3 3"
        stroke="currentColor"
        strokeWidth="1.3"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  )
}

export default Arrow
