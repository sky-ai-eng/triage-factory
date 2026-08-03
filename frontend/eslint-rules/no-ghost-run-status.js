import { readFileSync } from 'node:fs'

// no-ghost-run-status — fail the lint when component code branches on a
// conversation status the backend cannot emit.
//
// Why this exists on top of the Go mirror test: that test pins the three
// exported arrays in src/types.ts against internal/domain/run_status.go, and
// component code never reads those arrays. It compares a status against a bare
// literal — `run.Status === 'cancelled'`, `case 'task_unsolvable':` — and no
// amount of array-pinning can see a literal in a switch arm. Three retired
// statuses outlived their backend states that way; two of them came back within
// days of the mirror test landing.
//
// SCOPE — what counts as "a conversation status" here, and why the two
// look-alike vocabularies are safe:
//
//   1. `<expr>.Status` — the Conversation projection is PascalCase (it comes
//      through a handler-side map of the Go struct), while every other DTO in
//      types.ts spells its own status lowercase: a curator turn's
//      `request.status` and a blueprint run's `blueprint_run.status` both
//      legitimately terminate `cancelled`. The case alone separates them, with
//      no type information and no false positives.
//   2. Anything annotated `RunStatusValue` — the alias exists so a status that
//      travels one hop from that property into a helper (`runStatusColor(status)`,
//      a tone switch) stays visible. Without it the guard would stop at the
//      function boundary, which is exactly where two of the retired arms hid.
//
// The alternative scoping — type information, via a `Conversation['Status']`
// -typed expression — buys nothing extra here: that property is typed `string`
// on purpose (an unrecognized wire value must reach the closed-world predicates
// in lib/runStatus, not blow up at the boundary), so the checker would answer
// "string" for the curator and blueprint statuses too, and the rule would have
// to fall back on names anyway. This way the check costs no type-aware lint
// pass.
//
// Positions checked: `===`/`!==` comparisons and `case` arms — i.e. every place
// a status name decides something. ASSIGNMENT positions are deliberately not
// checked: the board and run view mint placeholder rows for un-spawned chain
// steps whose `Status` is a client-side sentinel the server never sends, and
// those are honest — nothing branches on the name.

const VOCABULARY_SOURCE = new URL('../src/types.ts', import.meta.url)
const VOCABULARY_DECLARATIONS = ['CLAIM_PHASES', 'TERMINAL_RUN_STATUSES', 'RUN_STATUSES']
const STATUS_PROPERTY = 'Status'
const STATUS_TYPE = 'RunStatusValue'

// readVocabulary lifts the status names out of the types.ts mirror, so this
// rule and the Go parity test read the same declaration and a status added in
// Go reaches both by the same edit. Textual for the same reason the Go side is:
// the arrays are flat literals by convention, and a rule that needed a TS
// program to start would be a lint pass of its own.
//
// Every failure here throws rather than returning an empty set: a guard that
// silently passes when it cannot find what it guards is worse than no guard.
function readVocabulary() {
  const source = readFileSync(VOCABULARY_SOURCE, 'utf8')
  const statuses = new Set()
  for (const declaration of VOCABULARY_DECLARATIONS) {
    const open = `export const ${declaration} = [`
    const start = source.indexOf(open)
    if (start < 0) {
      throw new Error(
        `no-ghost-run-status: src/types.ts has no \`${open}…]\` declaration — the run-status vocabulary lint has nothing to check against`,
      )
    }
    const body = source.slice(start + open.length)
    const end = body.indexOf(']')
    if (end < 0) {
      throw new Error(`no-ghost-run-status: src/types.ts declaration ${declaration} is unterminated`)
    }
    for (const match of body.slice(0, end).matchAll(/'([a-z_]+)'/g)) {
      statuses.add(match[1])
    }
  }
  if (statuses.size === 0) {
    throw new Error(
      'no-ghost-run-status: parsed the run-status declarations in src/types.ts but found no status names',
    )
  }
  return statuses
}

// Exported for the rule's own tests, which assert on the parsed set rather
// than a second hand-written copy of it.
export const RUN_STATUS_VOCABULARY = readVocabulary()

const VOCABULARY = [...RUN_STATUS_VOCABULARY].join(', ')

const COMPARISON_OPERATORS = new Set(['===', '!==', '==', '!='])

/** @type {import('eslint').Rule.RuleModule} */
export default {
  meta: {
    type: 'problem',
    docs: {
      description:
        'disallow comparing a conversation status against a name outside the run-status vocabulary',
    },
    schema: [],
    messages: {
      ghost:
        "'{{status}}' is not a conversation status, so this branch can never be taken. The vocabulary is owned by internal/domain/run_status.go and mirrored in src/types.ts (CLAIM_PHASES / TERMINAL_RUN_STATUSES / RUN_STATUSES): {{vocabulary}}.",
    },
  },

  create(context) {
    const sourceCode = context.sourceCode
    // Checks are gathered during the traversal and evaluated at Program:exit,
    // because resolving a `RunStatusValue` annotation walks up from the
    // declaration through `parent` links that ESLint only fills in as it
    // traverses — a helper declared below its caller would otherwise be
    // invisible.
    const checks = []

    function unwrap(node) {
      if (!node) return null
      if (node.type === 'ChainExpression') return unwrap(node.expression)
      if (node.type === 'TSNonNullExpression') return unwrap(node.expression)
      return node
    }

    function stringLiteral(node) {
      const inner = unwrap(node)
      return inner?.type === 'Literal' && typeof inner.value === 'string' ? inner : null
    }

    function resolveVariable(identifier) {
      for (let scope = sourceCode.getScope(identifier); scope; scope = scope.upper) {
        const reference = scope.references.find((r) => r.identifier === identifier)
        if (reference) return reference.resolved
      }
      return null
    }

    function typeReferenceName(annotation) {
      const type = annotation?.type === 'TSTypeAnnotation' ? annotation.typeAnnotation : annotation
      if (type?.type === 'TSTypeReference' && type.typeName?.type === 'Identifier') {
        return type.typeName.name
      }
      return null
    }

    // The annotation on an identifier's declaration: a plain `status: T`
    // parameter or variable, or a destructured `{ status }: { status: T }`
    // parameter — the shape every component prop bag in this codebase uses.
    function declaredTypeName(identifier) {
      const variable = resolveVariable(identifier)
      const definition = variable?.defs[0]
      const name = definition?.name
      if (!name) return null
      if (name.typeAnnotation) return typeReferenceName(name.typeAnnotation)
      const property = name.parent
      if (property?.type !== 'Property' || property.parent?.type !== 'ObjectPattern') return null
      const shape = property.parent.typeAnnotation?.typeAnnotation
      if (shape?.type !== 'TSTypeLiteral') return null
      const member = shape.members.find(
        (m) =>
          m.type === 'TSPropertySignature' &&
          !m.computed &&
          m.key?.type === 'Identifier' &&
          m.key.name === name.name,
      )
      return member ? typeReferenceName(member.typeAnnotation) : null
    }

    function isRunStatusExpression(node) {
      const inner = unwrap(node)
      if (!inner) return false
      if (inner.type === 'MemberExpression') {
        return (
          !inner.computed &&
          inner.property.type === 'Identifier' &&
          inner.property.name === STATUS_PROPERTY
        )
      }
      if (inner.type === 'Identifier') return declaredTypeName(inner) === STATUS_TYPE
      return false
    }

    return {
      BinaryExpression(node) {
        if (!COMPARISON_OPERATORS.has(node.operator)) return
        const left = stringLiteral(node.left)
        const right = stringLiteral(node.right)
        if (left && !right) checks.push({ literal: left, subject: node.right })
        else if (right && !left) checks.push({ literal: right, subject: node.left })
      },

      SwitchStatement(node) {
        for (const branch of node.cases) {
          const literal = branch.test ? stringLiteral(branch.test) : null
          if (literal) checks.push({ literal, subject: node.discriminant })
        }
      },

      'Program:exit'() {
        for (const { literal, subject } of checks) {
          if (RUN_STATUS_VOCABULARY.has(literal.value)) continue
          if (!isRunStatusExpression(subject)) continue
          context.report({
            node: literal,
            messageId: 'ghost',
            data: { status: literal.value, vocabulary: VOCABULARY },
          })
        }
      },
    }
  },
}
