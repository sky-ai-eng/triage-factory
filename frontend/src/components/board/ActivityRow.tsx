import { useState, type CSSProperties, type ReactNode } from 'react'
import { Link } from 'react-router'
import { Scan } from '../../ui/scan/Scan'
import { Arrow } from './Arrow'

// ActivityRow — one row, always in the same place, always going to the same
// destination: the run. Only its words change.
//
// While work is live the command SCANS — `ui/scan`, which is emission applied
// to type. That motion means an agent acting, and nothing else in the product
// may use it. Overflow dissolves into the arrow: it never truncates with an
// ellipsis, and a card's height never shifts because a command was long.
//
// The row is a destination, not a report. It reads "View run" in every
// settled state; where the run got to is the status mark's job.

const DISSOLVE: CSSProperties = {
  flex: 1,
  minWidth: 0,
  overflow: 'hidden',
  whiteSpace: 'nowrap',
  WebkitMaskImage: 'linear-gradient(to right, #000 calc(100% - 26px), transparent)',
  maskImage: 'linear-gradient(to right, #000 calc(100% - 26px), transparent)',
}

export function ActivityRow({
  command,
  label = 'View run',
  href,
  variant = 'row',
  onClick,
  style,
}: {
  /** The agent's current command. Present tense while running. */
  command?: string
  /** Shown when there is no live command. */
  label?: string
  /** The run page. Renders a link; without one the row is a button. */
  href?: string
  /** `foot` sizes it to its words and types it as a readout, for a settled
   *  card's footer: there is no command left to report, so a row of its own
   *  would be a row holding two words. */
  variant?: 'row' | 'foot'
  onClick?: () => void
  style?: CSSProperties
}) {
  const [hover, setHover] = useState(false)
  const foot = variant === 'foot'
  const shell: CSSProperties = {
    display: 'flex',
    alignItems: 'center',
    gap: foot ? '6px' : '7px',
    width: foot ? 'auto' : '100%',
    flex: foot ? '0 0 auto' : undefined,
    border: 0,
    background: 'none',
    padding: foot ? 0 : '1px 0',
    textAlign: 'left',
    fontFamily: 'var(--font-sans)',
    textDecoration: 'none',
    cursor: 'pointer',
    ...style,
  }
  const inner: ReactNode = (
    <>
      {command ? (
        // The command is the agent's own words, so it is mono — one step below
        // the sans title it sits under.
        <Scan
          active
          style={{
            ...DISSOLVE,
            fontFamily: 'var(--font-mono)',
            fontSize: 'var(--text-reported)',
            letterSpacing: '-0.01em',
          }}
        >
          {command}
        </Scan>
      ) : (
        <span
          style={{
            // "View run" is interface copy a human wrote, so it stays sans —
            // except in the footer, where it is reporting a duration and joins
            // the readout it sits beside.
            ...(foot ? null : DISSOLVE),
            whiteSpace: 'nowrap',
            fontFamily: foot ? 'var(--font-mono)' : undefined,
            fontSize: foot ? 'var(--text-readout)' : 'var(--text-ui)',
            fontVariantNumeric: foot ? 'tabular-nums' : undefined,
            letterSpacing: foot ? 'var(--tracking-readout)' : undefined,
            color: hover
              ? 'var(--color-ink-1)'
              : foot
                ? 'color-mix(in srgb, var(--color-ink-3) 80%, transparent)'
                : 'var(--color-ink-3)',
            transition: 'color var(--dur-hover)',
          }}
        >
          {label}
        </span>
      )}
      <Arrow
        style={{
          flexShrink: 0,
          color: foot
            ? 'color-mix(in srgb, var(--color-ink-3) 80%, transparent)'
            : 'var(--color-ink-4)',
          opacity: hover ? 1 : 0.5,
          transform: hover ? 'translateX(2px)' : 'none',
          transition: 'opacity var(--dur-hover), transform var(--dur-hover) var(--ease-default)',
        }}
      />
    </>
  )
  return href ? (
    <Link
      to={href}
      onClick={onClick}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      style={shell}
    >
      {inner}
    </Link>
  ) : (
    <button
      type="button"
      onClick={onClick}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      style={shell}
    >
      {inner}
    </button>
  )
}

export default ActivityRow
