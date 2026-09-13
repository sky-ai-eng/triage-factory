import { describe, expect, it } from 'vitest'

// In every rule, a vendor-prefixed declaration comes BEFORE its unprefixed
// twin. The production build minifies with Lightning CSS, and given
// `backdrop-filter: X; -webkit-backdrop-filter: X` in that order it emits only
// the prefixed one — which Chrome does not honour, so the blur silently
// vanishes from the shipped bundle while the unminified dev server still
// shows it. Prefixed first, the minifier keeps both. The rule is held for
// every property rather than that one, because which pairs the minifier
// collapses is its decision, and this is the ordinary order anyway: the
// unprefixed declaration is the one meant to win where a browser understands
// both.

const SHEETS = import.meta.glob('../**/*.css', {
  query: '?raw',
  import: 'default',
  eager: true,
}) as Record<string, string>

const PREFIX = /^-(webkit|moz|ms|o)-/

/** Every prefixed-after-unprefixed pair in the sheet, as `line: property`. */
function offenders(css: string): string[] {
  const bad: string[] = []
  // Comments are blanked rather than removed so line numbers survive.
  const stripped = css.replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, ' '))
  for (const block of stripped.matchAll(/\{([^{}]*)\}/g)) {
    const seen = new Set<string>()
    let line = stripped.slice(0, block.index).split('\n').length
    for (const decl of block[1].split(';')) {
      const m = /^\s*([-a-zA-Z]+)\s*:/.exec(decl)
      if (m) {
        const prop = m[1]
        const bare = prop.replace(PREFIX, '')
        if (PREFIX.test(prop) && seen.has(bare)) bad.push(`${line}: ${prop} after ${bare}`)
        seen.add(prop)
      }
      line += (decl.match(/\n/g) ?? []).length
    }
  }
  return bad
}

describe('vendor-prefixed declarations precede their unprefixed twins', () => {
  it('detects a prefixed declaration that follows its unprefixed twin', () => {
    expect(offenders(`a{backdrop-filter:blur(1px);-webkit-backdrop-filter:blur(1px)}`)).toEqual([
      '1: -webkit-backdrop-filter after backdrop-filter',
    ])
    expect(offenders(`a{-webkit-backdrop-filter:blur(1px);backdrop-filter:blur(1px)}`)).toEqual([])
    expect(offenders(`a{backdrop-filter:blur(1px)}\nb{-webkit-backdrop-filter:blur(1px)}`)).toEqual(
      [],
    )
    expect(
      offenders(`/* mask-image: x;\n -webkit-mask-image */\na{\n  mask-image: none;\n}`),
    ).toEqual([])
  })

  it('holds across every stylesheet under src/', () => {
    // The glob has to have found the sheets, or the sweep below passes empty.
    expect(Object.keys(SHEETS)).toContain('../components/board/assignee-picker.css')
    const report = Object.entries(SHEETS)
      .flatMap(([file, css]) => offenders(css).map((o) => `${file}:${o}`))
      .sort()
    expect(report).toEqual([])
  })
})
