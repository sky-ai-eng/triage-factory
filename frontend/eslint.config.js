import js from '@eslint/js'
import globals from 'globals'
import reactHooks from 'eslint-plugin-react-hooks'
import reactRefresh from 'eslint-plugin-react-refresh'
import tseslint from 'typescript-eslint'
import prettier from 'eslint-config-prettier'
import { defineConfig, globalIgnores } from 'eslint/config'
import noGhostConversationStatus from './eslint-rules/no-ghost-conversation-status.js'
import noRawApiFetch from './eslint-rules/no-raw-api-fetch.js'
import noTestGlobalUnstub from './eslint-rules/no-test-global-unstub.js'
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
      'conversation-status': { rules: { 'no-ghost-conversation-status': noGhostConversationStatus } },
      // lib/apiClient is the only door to /api — see the rule's header for the
      // three behaviours a raw fetch opts out of.
      api: { rules: { 'no-raw-api-fetch': noRawApiFetch } },
      // The design-system boundary. src/ui/ knows tokens and nothing about
      // Triage Factory; the rule is a no-op for every file outside it, so it
      // is registered globally rather than in an override block.
      ui: { rules: { 'no-app-imports': uiNoAppImports } },
      // src/test/setup.ts owns the order of unmount-then-unstub; the rule is a
      // no-op for every file that is not a test, so it is registered globally
      // rather than in an override block.
      test: { rules: { 'no-global-unstub': noTestGlobalUnstub } },
    },
    rules: {
      'conversation-status/no-ghost-conversation-status': 'error',
      'api/no-raw-api-fetch': 'error',
      'ui/no-app-imports': 'error',
      'test/no-global-unstub': 'error',
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
