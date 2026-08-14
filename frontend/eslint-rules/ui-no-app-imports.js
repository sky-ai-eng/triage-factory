import path from 'node:path'

// ui-no-app-imports — fail the lint when a design-system component reaches into
// the application.
//
// src/ui/ holds components that know about tokens and nothing about Triage
// Factory. That boundary is the whole reason the directory exists: a component
// that cannot name a task, a run or an org is one that can be mounted on
// /dev/ui with hand-written props, reviewed for fidelity in isolation, and
// reused without dragging a context provider behind it. A component that
// fetches is a feature component and belongs in src/components/.
//
// This is a rule rather than `no-restricted-imports` because the app uses
// relative imports throughout, and a glob cannot tell `../shared/x` (fine, and
// two directories deep inside ui/) from `../../lib/api` (not fine) without
// knowing how deep the importing file sits. Depth-based patterns break the
// first time a component gains or loses a nesting level, which is exactly when
// nobody is looking. Resolving the specifier against the file's own directory
// and asking whether the result is still inside src/ui/ cannot be fooled that
// way, and it needs no configuration listing the directories to forbid: the
// answer is "everything outside", permanently.
//
// Bare specifiers (react, lucide-react, motion) are always allowed — the
// boundary is about the app's own modules, not about third-party code. A
// design system may depend on a library; it may not depend on the product.
//
// Type-only imports are checked too. `import type { Task } from '../../types'`
// compiles away, so it costs nothing at runtime — but it means the component's
// props are shaped by a domain type, and the next edit reaches for the value
// side because the name is already in scope. The prop should be a structural
// type the component declares itself.

const UI_DIR = `${path.sep}src${path.sep}ui${path.sep}`

/** Where the boundary sits for a given file: the .../src/ui prefix it lives under. */
function uiRootFor(filename) {
  const at = filename.lastIndexOf(UI_DIR)
  if (at === -1) return null
  return filename.slice(0, at + UI_DIR.length)
}

const rule = {
  meta: {
    type: 'problem',
    docs: {
      description:
        'Disallow imports from outside src/ui/ within src/ui/, keeping the design system free of application knowledge.',
    },
    schema: [],
    messages: {
      escapes:
        "src/ui/ may not import '{{source}}' — it resolves outside the design system. A ui component takes data as props and reports events as callbacks; if it needs {{what}}, it belongs in src/components/ instead. See src/ui/CONVENTIONS.md.",
    },
  },

  create(context) {
    const filename = context.filename ?? context.getFilename()
    const uiRoot = uiRootFor(filename)
    // Not a design-system file, so the boundary does not apply.
    if (!uiRoot) return {}

    const dir = path.dirname(filename)

    function check(node, source) {
      // Bare and absolute-URL specifiers are third-party or built-in; only a
      // relative path can walk out of the directory.
      if (!source.startsWith('.')) return

      const resolved = path.resolve(dir, source)
      if (resolved.startsWith(uiRoot)) return

      // Name the thing being reached for, so the message says what to do
      // rather than only what not to.
      const what = /\/(lib|hooks|contexts)\//.test(`/${source}/`)
        ? 'application state or data'
        : /types$/.test(source)
          ? 'a domain type'
          : 'that module'

      context.report({ node, messageId: 'escapes', data: { source, what } })
    }

    return {
      ImportDeclaration(node) {
        check(node, node.source.value)
      },
      ExportNamedDeclaration(node) {
        if (node.source) check(node, node.source.value)
      },
      ExportAllDeclaration(node) {
        if (node.source) check(node, node.source.value)
      },
      ImportExpression(node) {
        if (node.source.type === 'Literal' && typeof node.source.value === 'string') {
          check(node, node.source.value)
        }
      },
    }
  },
}

export default rule
