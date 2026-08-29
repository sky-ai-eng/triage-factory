import type { CSSProperties, ElementType, ReactNode } from 'react'
import './scan.css'

// Scan — emission applied to TYPE.
//
// `readout/Emission` is the same meaning in a different medium: a dot, a bar or
// a spine in cool, saying an agent is working. This is that statement made by
// the words themselves, for the case where the row has no room for a mark and
// the sentence is already the thing being reported.
//
// It inherits Emission's whole doctrine, including the constraint: cool is only
// ever LINEAR or POINT, never an area. A crest travelling across a line of text
// is linear — a moving band, not a filled region — which is why this is allowed
// to exist at all.
//
// ONE meaning: an agent is emitting. Nothing else in the product may use it,
// and it must not be reached for as a loading state. A skeleton is for "no
// answer yet"; this is for "the answer is being written".
//
// THE CREST IS ALWAYS THE LIGHTER VALUE, AND THE BASE IS WHATEVER READS AT
// REST. One rule, both grounds — a light passing over text. What changes per
// ground is only where the base has to sit for the text to be readable while it
// waits: on dark it can sit mid-ramp at ink-3, and on light it has to be ink-1,
// because there is nowhere darker than ink-1 for a crest to go. The measured
// luminances and the failed alternatives are in scan.css.
//
// Rate and frequency are separate, and that separation is the whole timing
// design. The crest crosses in 2.2s; the rest between crossings is a hold at
// the head of the keyframe. A pulse every 4.8s reads as a heartbeat, one every
// 2.2s reads as a loading bar.

export type ScanProps = {
  children?: ReactNode
  /**
   * ink — luminance only, and the pure form of the rule: the crest is the
   *   lighter value and the base is whatever reads at rest.
   * cool — the same rule with hue in the crest, for a surface that wants the
   *   accent. Costs contrast, because a hue step is not a luminance step.
   */
  tone?: 'ink' | 'cool'
  /** False renders the text plainly, with no gradient and no animation. */
  active?: boolean
  className?: string
  as?: ElementType
  style?: CSSProperties
}

export function Scan({
  children,
  tone = 'ink',
  active = true,
  className = '',
  as: Tag = 'span',
  style,
}: ScanProps) {
  if (!active)
    return (
      <Tag className={className} style={style}>
        {children}
      </Tag>
    )
  return (
    <Tag className={('scan ' + className).trim()} data-tone={tone} style={style}>
      {children}
    </Tag>
  )
}

export default Scan
