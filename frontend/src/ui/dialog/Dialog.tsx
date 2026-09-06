import { useEffect, useRef, useState, useId } from 'react'
import type { MouseEvent as ReactMouseEvent, ReactNode } from 'react'
import { prefersReducedMotion } from '../shared/useReducedMotion'
import { useFocusTrap } from '../shared/useFocusTrap'
import { Hold } from '../hold/Hold'
import './dialog.css'

export type DialogConsequence = string | { text: string; tone?: 'loss' | 'keep' }

export type DialogProps = {
  open?: boolean
  /** 'destructive' switches the ink to alarm. Never decoration — only real loss. */
  kind?: 'normal' | 'destructive'
  title?: ReactNode
  body?: ReactNode
  /** What will actually happen. A tone of 'loss' marks what goes. */
  consequences?: DialogConsequence[]
  note?: ReactNode
  /**
   * Three paces. 'flash' is the full sequence; 'quiet' drops the bright beat;
   * 'none' skips the build entirely and is there because a confirmation you meet
   * constantly should not make you wait to read it. The build earns its second
   * when the dialog is rare — never when it is routine.
   */
  build?: 'flash' | 'quiet' | 'none'
  confirmLabel?: string
  cancelLabel?: string
  onConfirm?: () => void
  onCancel?: () => void
  width?: number
  /**
   * A reading, laid between `body` and `note`: free markup for what the prose
   * slots cannot hold — a figure band, a grid of address ranges. It takes the
   * same entrance stagger as the slots around it.
   */
  children?: ReactNode
  /**
   * Milliseconds the confirm control must be held. The verb becomes a Hold —
   * the gesture every other irreversible verb in this system takes — and the
   * destructive keyboard rules follow: Enter does not confirm, focus lands on
   * Cancel. Alarm ink, whatever `kind` says, because a held verb is one that
   * cannot be taken back.
   */
  confirmHold?: number
  /** No confirm control at all: the dialog is only a reading, and Cancel is Close. */
  noConfirm?: boolean
}

// Dialog — a decision the product cannot make for you.
//
// Three rules, all of them about honesty rather than looks:
//
//   1. It states CONSEQUENCES, not a question. "Are you sure?" transfers the
//      decision without informing it; a list of what will actually happen is
//      the only reason to interrupt someone.
//   2. Destructive and ordinary confirmations are the same component with
//      different ink. The alarm palette is never decoration — it appears only
//      when something is lost.
//   3. It arrives, it does not perform. The build vocabulary answers "what just
//      happened", and what just happened is that you asked a question of
//      yourself.
//
// See Dialog.md, including the two port decisions recorded at the end of it.

export function Dialog({
  open = false,
  kind = 'normal',
  title = '',
  body = '',
  consequences = [],
  note = '',
  build = 'flash',
  confirmLabel = 'Confirm',
  cancelLabel = 'Cancel',
  onConfirm,
  onCancel,
  width = 420,
  children = null,
  confirmHold = 0,
  noConfirm = false,
}: DialogProps) {
  const panelRef = useRef<HTMLDivElement | null>(null)
  const goRef = useRef<HTMLButtonElement | null>(null)
  const cancelRef = useRef<HTMLButtonElement | null>(null)
  const uid = useId()
  const headId = uid + '-head'
  const bodyId = uid + '-body'

  // A held confirm is a destructive one by definition — the hold exists for
  // verbs that cannot be taken back — so it takes the same keyboard rules.
  const held = confirmHold > 0
  const bad = kind === 'destructive' || held

  // Tab containment, initial focus and focus return all come from the house
  // primitive rather than being hand-rolled here — the design bundle recommends
  // Radix for this, but @radix-ui/react-dialog is not a dependency and
  // useFocusTrap is what the project's other modals already use.
  //
  // WHERE FOCUS LANDS IS A SAFETY DECISION, not a convenience one. On an
  // ordinary confirmation it lands on Confirm, so a keyboard user is not hunting
  // for the verb. On a DESTRUCTIVE one it lands on Cancel, because everywhere
  // else in this system an irreversible verb is a press-and-hold precisely so a
  // stray keypress cannot fire it — and a focused, Enter-bound destructive
  // button is the exact hazard Hold exists to remove.
  useFocusTrap(panelRef, {
    active: open,
    initialFocus: bad || noConfirm ? cancelRef : goRef,
  })

  // The trap moves focus onto a button the moment the dialog opens, and a
  // browser treats script focus as keyboard focus — so a reading opened by a
  // click arrived wearing a ring on Close. The ring is held back until a key
  // that moves focus is actually pressed; a mouse user never sees it.
  const [kb, setKb] = useState(false)
  useEffect(() => {
    if (!open) return
    // eslint-disable-next-line react-hooks/set-state-in-effect -- reset per open, before any key can arrive
    setKb(false)
    const on = (e: globalThis.KeyboardEvent) => {
      if (e.key === 'Tab' || e.key.startsWith('Arrow')) setKb(true)
    }
    window.addEventListener('keydown', on, true)
    return () => window.removeEventListener('keydown', on, true)
  }, [open])

  // The build is a STATE MACHINE on timers, not one long keyframe track. Percent
  // based keyframes drift: they depend on the animation actually starting when
  // the element mounts, and a re-opened dialog could inherit a partly-elapsed
  // clock — which is why it appeared to get quicker every time.
  const [phase, setPhase] = useState('frame')
  const built = phase === 'built'

  // Closing rewinds the build at render rather than in the effect: the effect
  // owns the timers and nothing else, so a re-opened dialog can never paint one
  // frame of the previous run's end state on its way in.
  const [wasOpen, setWasOpen] = useState(open)
  if (open !== wasOpen) {
    setWasOpen(open)
    if (!open) setPhase('frame')
  }

  useEffect(() => {
    if (!open) return
    // Being timers rather than keyframes is what puts this out of reach of the
    // blanket rule in tokens/motion.css. Under the preference the dialog is
    // simply built: a question you have been interrupted to answer should be
    // readable the instant it appears, and the beats are the interruption.
    if (build === 'none' || prefersReducedMotion()) {
      const t = setTimeout(() => setPhase('built'), 20)
      return () => clearTimeout(t)
    }
    // frame → [flash] → empty → measure → empty → built. Same shape either way;
    // quiet simply has no flash to spend time on.
    const steps: Array<[number, string]> =
      build === 'quiet'
        ? [
            [175, 'measure'],
            [470, 'settle'],
            [630, 'built'],
          ]
        : [
            [130, 'flash'],
            [215, 'frame2'],
            [265, 'measure'],
            [560, 'settle'],
            [715, 'built'],
          ]
    const ids = steps.map(([t, p]) => setTimeout(() => setPhase(p), t))
    return () => ids.forEach(clearTimeout)
  }, [open, build])

  useEffect(() => {
    if (!open) return
    const onKey = (e: globalThis.KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.preventDefault()
        e.stopPropagation()
        onCancel?.()
        return
      }
      // Enter confirms — except on a destructive dialog, where it does not
      // exist. Same reasoning as the focus target above: the one verb in this
      // system that cannot be taken back is never a single keystroke away. Tab
      // to it and press it deliberately. A reading has nothing to confirm.
      if (e.key === 'Enter' && !bad && !noConfirm) {
        e.preventDefault()
        onConfirm?.()
      }
    }
    // Capture, so a host that also claims Escape (the shell does) does not get
    // to close something else while this is the topmost layer.
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [open, bad, noConfirm, onConfirm, onCancel])

  if (!open) return null

  // The confirm control. Held, it is the trigger inside a surface-mode Hold —
  // the Hold owns the gesture and fires onConfirm when it completes, so the
  // button itself carries no click.
  const go = (
    <button
      type="button"
      className={'dlg-btn go' + (held ? ' held' : '')}
      ref={goRef}
      onClick={held ? undefined : onConfirm}
    >
      {confirmLabel}
    </button>
  )

  return (
    <div
      className="dlg-back"
      onMouseDown={(e: ReactMouseEvent<HTMLDivElement>) => {
        // The backdrop cancels — a dialog you cannot dismiss by looking away is
        // a trap, and every confirmation here is reversible by not confirming.
        if ((e.target as HTMLElement).classList.contains('dlg-back')) onCancel?.()
      }}
    >
      <div
        className={
          'dlg' +
          (bad ? ' bad' : '') +
          (build === 'quiet' ? ' quiet' : build === 'none' ? ' instant' : '') +
          (built ? ' built' : '')
        }
        data-phase={phase}
        data-kb={kb || undefined}
        style={{ width: width + 'px' }}
        role="dialog"
        aria-modal="true"
        aria-labelledby={headId}
        aria-describedby={body ? bodyId : undefined}
        ref={panelRef}
        tabIndex={-1}
      >
        <div className="dlg-rule" />
        <div className="dlg-head" id={headId}>
          {title}
        </div>
        {body ? (
          <p className="dlg-body" id={bodyId}>
            {body}
          </p>
        ) : null}
        {consequences.length ? (
          <ul className="dlg-list">
            {consequences.map((c, i) => (
              <li
                key={i}
                className={typeof c === 'string' ? '' : c.tone === 'loss' ? 'loss' : 'keep'}
              >
                {typeof c === 'string' ? c : c.text}
              </li>
            ))}
          </ul>
        ) : null}
        {children ? <div className="dlg-slot">{children}</div> : null}
        {note ? <p className="dlg-note">{note}</p> : null}
        <div className="dlg-acts">
          <button type="button" className="dlg-btn" ref={cancelRef} onClick={onCancel}>
            {cancelLabel}
          </button>
          {noConfirm ? null : held ? (
            <Hold
              variant="surface"
              trigger=".dlg-btn.go"
              tone="alarm"
              ms={confirmHold}
              onConfirm={() => onConfirm?.()}
              className="dlg-hold"
            >
              {go}
            </Hold>
          ) : (
            go
          )}
        </div>
        {/* The measure beat: a hollow donut drawn over the empty frame, in the
            same window as the cross. Last child, so the content stagger's
            nth-child indices are unaffected. */}
        <svg className="dlg-cal" viewBox="0 0 200 96" preserveAspectRatio="none" aria-hidden="true">
          <ellipse cx={100} cy={48} rx={78} ry={38} pathLength={100} />
          <ellipse cx={100} cy={48} rx={52} ry={24} pathLength={100} className="inner" />
        </svg>
      </div>
    </div>
  )
}

export default Dialog
