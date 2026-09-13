import { useState, type CSSProperties, type ReactNode } from 'react'
import { Link } from 'react-router'
import { Glyph } from './Glyph'
import { Arrow } from './Arrow'

// PermissionRow — the agent has stopped and cannot continue until you answer.
//
// It BLOCKS, which is why it takes the activity row's slot rather than sitting
// beside it: there is no current command to report, because there is no
// current command. A card never shows both.
//
// Requests STACK but do not multiply. Only the head renders; the count rides
// inside the shield rather than becoming a "+N more" line, because the queue
// depth is a property of the ask, not a second ask.
//
// The verbs are absent, not disabled, for a reader who may not answer. A
// greyed Allow still tells them the button is theirs to earn.
//
// The head is a LINK to the run, not a disclosure: a command is rarely
// answerable from its own text, and the transcript around it is the thing that
// makes it answerable. Same arrow and same destination as the activity row it
// stands in for, because it stands in for it.

export function PermissionRow({
  command,
  count = 1,
  href,
  onAllow,
  onDeny,
  interactive = true,
  style,
}: {
  /** The head of the queue — the command the agent wants to run. */
  command: string
  /** Queue depth. Rides inside the shield; 1 shows no numeral. */
  count?: number
  /** The run page. The head row links there. */
  href?: string
  onAllow?: () => void
  onDeny?: () => void
  /** False removes the verbs entirely rather than disabling them. */
  interactive?: boolean
  style?: CSSProperties
}) {
  const [hover, setHover] = useState(false)
  const verb: CSSProperties = {
    flex: 1,
    display: 'inline-flex',
    alignItems: 'center',
    justifyContent: 'center',
    gap: '6px',
    border: 0,
    borderRadius: 'var(--radius-nested)',
    padding: '6px 10px',
    fontFamily: 'var(--font-mono)',
    fontSize: 'var(--text-label)',
    // DM Mono ships 300/400/500. Anything heavier is SYNTHESIZED by the
    // browser, which smears the strokes and reads as a different typeface
    // beside a real 400. 500 is the family's own bold.
    fontWeight: 500,
    textTransform: 'uppercase',
    letterSpacing: 'var(--tracking-label)',
    cursor: 'pointer',
  }
  const headStyle: CSSProperties = {
    display: 'flex',
    alignItems: 'center',
    gap: '8px',
    width: '100%',
    border: 0,
    background: 'none',
    padding: '8px 10px 6px',
    textAlign: 'left',
    textDecoration: 'none',
    cursor: 'pointer',
  }
  const head: ReactNode = (
    <>
      <span
        aria-hidden
        style={{
          position: 'relative',
          flexShrink: 0,
          display: 'inline-flex',
          alignItems: 'center',
          justifyContent: 'center',
          width: 16,
          height: 16,
          color: 'var(--color-warm)',
        }}
      >
        <Glyph kind="shield" size={16} style={{ position: 'absolute', inset: 0 }} />
        {count > 1 && (
          <span
            style={{
              position: 'relative',
              fontFamily: 'var(--font-mono)',
              fontSize: 8,
              fontWeight: 500,
              lineHeight: 1,
              fontVariantNumeric: 'tabular-nums',
              transform: 'translateY(0.5px)',
            }}
          >
            {count}
          </span>
        )}
      </span>
      <span
        style={{
          minWidth: 0,
          flex: 1,
          overflow: 'hidden',
          whiteSpace: 'nowrap',
          fontFamily: 'var(--font-mono)',
          fontSize: 'var(--text-secondary)',
          lineHeight: 'var(--leading-snug)',
          color: 'var(--color-ink-1)',
        }}
      >
        {command}
      </span>
      <Arrow
        style={{
          flexShrink: 0,
          color: 'var(--color-warm)',
          opacity: hover ? 1 : 0.55,
          transform: hover ? 'translateX(2px)' : 'none',
          transition: 'opacity var(--dur-hover), transform var(--dur-hover) var(--ease-default)',
        }}
      />
    </>
  )
  return (
    <div
      style={{
        borderRadius: 'var(--radius-inflow)',
        background: 'var(--color-warm-1)',
        boxShadow: 'inset 0 0 0 1px var(--color-warm-2)',
        ...style,
      }}
    >
      {href ? (
        <Link
          to={href}
          title={command}
          onMouseEnter={() => setHover(true)}
          onMouseLeave={() => setHover(false)}
          style={headStyle}
        >
          {head}
        </Link>
      ) : (
        <div title={command} style={{ ...headStyle, cursor: 'default' }}>
          {head}
        </div>
      )}
      {interactive && (
        <div style={{ display: 'flex', alignItems: 'stretch', gap: '8px', padding: '0 10px 9px' }}>
          <button
            type="button"
            onClick={onDeny}
            style={{
              ...verb,
              color: 'var(--color-alarm)',
              background: 'color-mix(in srgb, var(--color-alarm) 12%, transparent)',
            }}
          >
            <Glyph kind="cross" size={11} />
            Deny
          </button>
          <button
            type="button"
            onClick={onAllow}
            style={{
              ...verb,
              color: 'var(--color-warm-ink)',
              background: 'var(--color-warm)',
              boxShadow: '0 0 16px -4px var(--color-warm)',
            }}
          >
            <Glyph kind="check" size={11} />
            Allow
          </button>
        </div>
      )}
    </div>
  )
}

export default PermissionRow
