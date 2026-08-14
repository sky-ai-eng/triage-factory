import { useState, useEffect, useRef, useLayoutEffect } from 'react'

// A head band holds one line. Names are capped here rather than trusted to be
// short, and the same cap is what the field will accept.
export const NAME_MAX = 32

// One canvas for the whole app, created on first use. Measuring text needs a
// 2d context and nothing else; making a new one per keystroke is a needless
// allocation on the hot path of typing.
let measureCanvas: HTMLCanvasElement | null = null

export function TitleField({ title, onSave }: { title: string; onSave: (name: string) => void }) {
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState(title)
  const [w, setW] = useState<number | null>(null)
  const [full, setFull] = useState(false)
  const input = useRef<HTMLInputElement | null>(null)
  const flash = useRef<ReturnType<typeof setTimeout> | null>(null)

  // Adjusted during render rather than in an effect: an effect would paint the
  // previous name once and then correct it, and React handles a set-during-
  // render by re-running before it commits anything.
  const [seenTitle, setSeenTitle] = useState(title)
  if (title !== seenTitle) {
    setSeenTitle(title)
    setDraft(title)
  }
  useEffect(() => {
    if (editing && input.current) input.current.select()
  }, [editing])

  // maxLength drops the character silently, which reads as the field having
  // stopped listening. Catch the drop as it happens — text is being inserted,
  // the field is already full, and there is no selection being replaced — and
  // say so for a fifth of a second.
  useEffect(() => {
    const el = input.current
    if (!editing || !el) return
    const onBefore = (e: Event) => {
      const data = (e as InputEvent).data
      if (data == null || data === '') return
      if (el.selectionStart !== el.selectionEnd) return
      if (el.value.length < NAME_MAX) return
      setFull(true)
      if (flash.current) clearTimeout(flash.current)
      flash.current = setTimeout(() => setFull(false), 220)
    }
    el.addEventListener('beforeinput', onBefore)
    return () => {
      el.removeEventListener('beforeinput', onBefore)
      if (flash.current) clearTimeout(flash.current)
    }
  }, [editing])

  // 'ch' is the width of a zero, which on a proportional face bears no relation
  // to what was typed — a field sized that way is too narrow for 'platformmmm'
  // and the text scrolls out of sight on the left. Measure the draft in the
  // field's own font instead, so the box is never behind the caret.
  useLayoutEffect(() => {
    if (!editing || !input.current) return
    const cs = getComputedStyle(input.current)
    const font = cs.font || `${cs.fontWeight} ${cs.fontSize} ${cs.fontFamily}`
    if (!measureCanvas) measureCanvas = document.createElement('canvas')
    const ctx = measureCanvas.getContext('2d')
    if (!ctx) return
    ctx.font = font
    const pad = parseFloat(cs.paddingLeft) + parseFloat(cs.paddingRight)
    setW(Math.max(72, Math.ceil(ctx.measureText(draft || ' ').width + pad + 3)))
  }, [editing, draft])

  const commit = () => {
    if (flash.current) clearTimeout(flash.current)
    setFull(false)
    setEditing(false)
    const next = draft.trim().slice(0, NAME_MAX)
    if (next && next !== title) onSave(next)
    else setDraft(title)
  }

  if (editing) {
    return (
      <input
        ref={input}
        className="sh-title sh-title-in"
        value={draft}
        maxLength={NAME_MAX}
        aria-label="Rename"
        data-full={full ? '1' : undefined}
        // The field is exactly as wide as what it holds, so opening it does not
        // shove the rest of the head band sideways.
        style={{ width: `${w || 72}px` }}
        onChange={(e) => setDraft(e.target.value)}
        onBlur={commit}
        onKeyDown={(e) => {
          if (e.key === 'Enter') commit()
          if (e.key === 'Escape') {
            // Consumed, so the shell's own Escape handling does not also close
            // a rail layer on the same press.
            e.preventDefault()
            e.stopPropagation()
            setDraft(title)
            setEditing(false)
          }
        }}
      />
    )
  }

  return (
    <div
      className="sh-titlewrap"
      onClick={() => setEditing(true)}
      title="Rename"
      tabIndex={0}
      role="button"
      onKeyDown={(e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault()
          setEditing(true)
        }
      }}
    >
      <div className="sh-title">{title}</div>
      <svg
        className="sh-pen"
        width={12}
        height={12}
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth={1.8}
        strokeLinecap="round"
        strokeLinejoin="round"
        aria-hidden="true"
      >
        <path d="M12 20h9" />
        <path d="M16.5 3.5a2.12 2.12 0 0 1 3 3L7 19l-4 1 1-4Z" />
      </svg>
    </div>
  )
}

export default TitleField
