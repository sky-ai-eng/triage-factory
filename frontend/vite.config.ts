import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  // Vitest: jsdom component/integration tests under src/**/*.test.tsx. globals
  // off (explicit imports keep the app's type surface clean); CSS skipped (the
  // tests assert on text/roles, not Tailwind output). setup wires jest-dom
  // matchers + per-test cleanup.
  test: {
    environment: 'jsdom',
    globals: false,
    css: false,
    setupFiles: ['./src/test/setup.ts'],
    include: ['src/**/*.test.{ts,tsx}'],
  },
  server: {
    proxy: {
      '/api/ws': { target: 'ws://localhost:3000', ws: true },
      '/api': 'http://localhost:3000',
    },
  },
  optimizeDeps: {
    include: ['@babylonjs/core'],
    // Babylon's modular ESM build does internal `await import()` calls.
    // The dep optimizer (Rolldown in Vite 8) would otherwise split those
    // into separate chunks that race against namespace initialization,
    // crashing with "Cannot read properties of undefined (reading
    // 'MatrixTrackPrecisionChange')". Mirror the production manualChunks
    // workaround to force all Babylon code into a single chunk.
    rolldownOptions: {
      output: {
        manualChunks(id: string) {
          if (id.includes('node_modules/@babylonjs/')) {
            return 'babylon'
          }
        },
      },
    },
  },
  build: {
    // Keep all Babylon.js code in a single chunk. Babylon's modular
    // build uses internal `await import()` calls that the bundler
    // would otherwise split into separate chunks; those chunks register
    // onto namespaces that may not be initialized yet at load time,
    // producing runtime errors like "Cannot set properties of undefined
    // (setting 'DumpData')". Forcing a single babylon chunk eliminates
    // the race entirely.
    rolldownOptions: {
      output: {
        manualChunks(id: string) {
          if (id.includes('node_modules/@babylonjs/')) {
            return 'babylon'
          }
        },
      },
    },
  },
})
