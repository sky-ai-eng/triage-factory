import { describe, expect, it } from 'vitest'

// No two stylesheets lead their rules with the same class prefix. Every sheet
// lands in one global cascade, so two components that both style `.sr` style
// each other's markup too, and which declarations win depends on which page's
// sheet loaded last: the status board renders as designed until the Overview's
// spend ring has been visited, and then lays out as a block with a square
// aspect ratio. A component's prefix is its namespace, and it has one owner.

const SHEETS = import.meta.glob('../**/*.css', {
  query: '?raw',
  import: 'default',
  eager: true,
}) as Record<string, string>

// `tf-` is the project's own namespace, shared on purpose. index.css is the
// app-wide sheet, which reaches into components deliberately (the shell
// body's scaffold padding), so it owns no prefix.
const SHARED = new Set(['tf'])
const APP_WIDE = '../index.css'

/** The class prefixes a sheet's rules lead with: the first class of each
 *  selector, up to its first `-` or `_`. A class further into a selector is
 *  a descendant the rule reaches into, not one it defines. */
function prefixes(css: string): Set<string> {
  const out = new Set<string>()
  const stripped = css.replace(/\/\*[\s\S]*?\*\//g, '')
  for (const [, selectors] of stripped.matchAll(/([^{}]+)\{/g)) {
    for (const selector of selectors.split(',')) {
      const m = /^\s*[a-z]*\.([a-zA-Z][\w-]*)/.exec(selector)
      if (m) out.add(m[1].split(/[-_]/)[0])
    }
  }
  return out
}

describe('each stylesheet owns its class prefix', () => {
  it('reads the prefix a rule leads with', () => {
    expect(prefixes('.sr { }\na.sr:hover { }\n.sr-svg, .sr-seg { }')).toEqual(new Set(['sr']))
    expect(prefixes('.sp-board > .sr { }')).toEqual(new Set(['sp']))
    expect(prefixes('/* .xx { } */\n@media (min-width: 1px) { .tb__row { } }')).toEqual(
      new Set(['tb']),
    )
  })

  it('holds across every stylesheet under src/', () => {
    // The walk has to have found the sheets, with their text, or the sweep
    // below passes empty.
    expect(SHEETS['../ui/statusrules/status-rules.css']).toContain('.sr-board')
    const owners = new Map<string, string[]>()
    for (const [file, css] of Object.entries(SHEETS)) {
      if (file === APP_WIDE) continue
      for (const p of prefixes(css)) {
        if (!SHARED.has(p)) owners.set(p, [...(owners.get(p) ?? []), file])
      }
    }
    const shared = [...owners]
      .filter(([, files]) => files.length > 1)
      .map(([p, files]) => `${p}: ${files.sort().join(', ')}`)
    expect(shared).toEqual([])
  })
})
