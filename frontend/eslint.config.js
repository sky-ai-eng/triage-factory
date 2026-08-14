import js from '@eslint/js'
import globals from 'globals'
import reactHooks from 'eslint-plugin-react-hooks'
import reactRefresh from 'eslint-plugin-react-refresh'
import tseslint from 'typescript-eslint'
import prettier from 'eslint-config-prettier'
import { defineConfig, globalIgnores } from 'eslint/config'
import noGhostRunStatus from './eslint-rules/no-ghost-run-status.js'
import uiNoAppImports from './eslint-rules/ui-no-app-imports.js'

export default defineConfig([
  globalIgnores(['dist']),
  {
    files: ['**/*.{ts,tsx}'],
    extends: [
      js.configs.recommended,
      tseslint.configs.recommended,
      reactHooks.configs.flat.recommended,
      reactRefresh.configs.vite,
      prettier,
    ],
    languageOptions: {
      ecmaVersion: 2020,
      globals: globals.browser,
    },
    // The conversation-status vocabulary's second enforcer. The Go mirror test
    // pins the arrays in src/types.ts; this pins the component code that
    // branches on them, which is where the retired statuses actually lived.
    plugins: {
      'run-status': { rules: { 'no-ghost-run-status': noGhostRunStatus } },
      // The design-system boundary. src/ui/ knows tokens and nothing about
      // Triage Factory; the rule is a no-op for every file outside it, so it
      // is registered globally rather than in an override block.
      ui: { rules: { 'no-app-imports': uiNoAppImports } },
    },
    rules: {
      'run-status/no-ghost-run-status': 'error',
      'ui/no-app-imports': 'error',
      '@typescript-eslint/no-unused-vars': [
        'error',
        {
          argsIgnorePattern: '^_',
          varsIgnorePattern: '^_',
          caughtErrorsIgnorePattern: '^_',
        },
      ],
    },
  },
])
