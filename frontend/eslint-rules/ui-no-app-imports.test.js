// @vitest-environment node
//
// Node, not the suite's default jsdom: the rule reasons about absolute file
// paths, and RuleTester's `filename` option is only meaningful outside the
// browser environment.
import { RuleTester } from 'eslint'
import tseslint from 'typescript-eslint'
import { describe, it } from 'vitest'
import rule from './ui-no-app-imports.js'

// The fixtures are the two things the boundary has to separate, and they look
// alike as strings: `../shared/useReducedMotion` from a component two levels
// down is legal, `../../lib/api` from the same file is not, and both are "one
// or two dots then a folder name". Only resolution tells them apart — which is
// the entire argument for this being a rule rather than a set of globs.
//
// RuleTester runs its assertions inline when no global describe/it exists
// (vitest runs with globals off), so each run() call throws inside its own it().
const ruleTester = new RuleTester({
  languageOptions: {
    parser: tseslint.parser,
    ecmaVersion: 2022,
    sourceType: 'module',
  },
})

const COMPONENT = '/repo/frontend/src/ui/table/Table.tsx'
const SHARED = '/repo/frontend/src/ui/shared/useReducedMotion.ts'
const FEATURE = '/repo/frontend/src/components/TaskCard.tsx'

describe('ui-no-app-imports', () => {
  it('allows third-party, sibling and ui-internal imports', () => {
    ruleTester.run('ui-no-app-imports', rule, {
      valid: [
        // Third-party is always fine — a design system may depend on a
        // library, just not on the product.
        { code: "import React from 'react'", filename: COMPONENT },
        { code: "import { Search } from 'lucide-react'", filename: COMPONENT },
        // Within the same component folder.
        { code: "import './table.css'", filename: COMPONENT },
        { code: "import { columnDef } from './columns'", filename: COMPONENT },
        // Across ui/, which is what src/ui/shared exists for.
        { code: "import { useReducedMotion } from '../shared/useReducedMotion'", filename: COMPONENT },
        { code: "import { Checkbox } from '../checkbox/Checkbox'", filename: COMPONENT },
        // A ui file that is itself one level down.
        { code: "import { Pager } from '../table/Pager'", filename: SHARED },
        // Outside src/ui/ the boundary does not apply at all: a feature
        // component importing the app is the normal direction of the arrow.
        { code: "import { api } from '../lib/api'", filename: FEATURE },
        { code: "import { Table } from '../ui/table/Table'", filename: FEATURE },
      ],
      invalid: [],
    })
  })

  it('rejects every way of reaching out of src/ui/', () => {
    ruleTester.run('ui-no-app-imports', rule, {
      valid: [],
      invalid: [
        // The data layer.
        {
          code: "import { api } from '../../lib/api'",
          filename: COMPONENT,
          errors: [{ messageId: 'escapes' }],
        },
        // App hooks and contexts.
        {
          code: "import { useTeams } from '../../hooks/useTeams'",
          filename: COMPONENT,
          errors: [{ messageId: 'escapes' }],
        },
        {
          code: "import { useOrg } from '../../contexts/OrgContext'",
          filename: COMPONENT,
          errors: [{ messageId: 'escapes' }],
        },
        // Domain types, type-only. Compiles away, still shapes the props.
        {
          code: "import type { Task } from '../../types'",
          filename: COMPONENT,
          errors: [{ messageId: 'escapes' }],
        },
        // Re-exports carry the same dependency as an import.
        {
          code: "export { api } from '../../lib/api'",
          filename: COMPONENT,
          errors: [{ messageId: 'escapes' }],
        },
        {
          code: "export * from '../../lib/api'",
          filename: COMPONENT,
          errors: [{ messageId: 'escapes' }],
        },
        // A dynamic import is still an import.
        {
          code: "const m = import('../../lib/api')",
          filename: COMPONENT,
          errors: [{ messageId: 'escapes' }],
        },
        // Climbing past src/ entirely.
        {
          code: "import { Shell } from '../../Shell'",
          filename: COMPONENT,
          errors: [{ messageId: 'escapes' }],
        },
        // From src/ui/shared, one fewer level is needed to escape — the case a
        // depth-based glob gets wrong.
        {
          code: "import { api } from '../../lib/api'",
          filename: SHARED,
          errors: [{ messageId: 'escapes' }],
        },
      ],
    })
  })
})
