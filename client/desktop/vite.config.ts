/// <reference types="vitest" />
import fs from 'node:fs';
import { defineConfig, type Plugin } from 'vite';
import react from '@vitejs/plugin-react';
import path from 'path';
import { cspProdStripPlugin } from './scripts/csp-prod-strip';

const legalDocumentModules = new Map([
  ['virtual:concord-legal/license', path.resolve(__dirname, '../../LICENSE')],
  ['virtual:concord-legal/notice', path.resolve(__dirname, '../../NOTICE.md')],
  ['virtual:concord-legal/privacy-policy', path.resolve(__dirname, '../../docs/privacy-policy.md')],
  [
    'virtual:concord-legal/terms-of-service',
    path.resolve(__dirname, '../../docs/legal/terms-of-service.md'),
  ],
]);

function legalDocumentPlugin(): Plugin {
  return {
    name: 'concord-legal-documents',
    resolveId(id) {
      return legalDocumentModules.has(id) ? id : null;
    },
    load(id) {
      const filePath = legalDocumentModules.get(id);
      if (!filePath) return null;
      return `export default ${JSON.stringify(fs.readFileSync(filePath, 'utf8'))};`;
    },
  };
}

export default defineConfig(() => {
  return {
    plugins: [react(), legalDocumentPlugin(), cspProdStripPlugin()],
    resolve: {
      alias: {
        '@': path.resolve(__dirname, './src'),
        '@shared': path.resolve(__dirname, '../../shared'),
      },
    },
    test: {
      globals: true,
      environment: 'jsdom',
      setupFiles: ['./tests/setup.ts'],
      include: [
        'tests/**/*.test.{ts,tsx}',
        'src/**/*.test.{ts,tsx}',
        'scripts/**/*.test.{ts,tsx}',
        '../../scripts/**/*.test.{ts,tsx}',
      ],
      testTimeout: 10000,
      hookTimeout: 10000,
      teardownTimeout: 5000,
      coverage: {
        provider: 'istanbul',
        reporter: ['text', 'html', 'lcov'],
        include: [
          'src/renderer/**/*.{ts,tsx}',
          'src/shared/**/*.{ts,tsx}',
          // #920 §5.5, §5.7 added typed constants + build-time generators that
          // SonarCloud QG measures as new code. Without these globs, SonarCloud
          // reports new_coverage=0% and the QG fails, even when the tests run
          // green (the LCOV report just doesn't include the files).
          'src/constants/**/*.{ts,mts}',
          'scripts/**/*.{ts,mts,mjs}',
        ],
        exclude: [
          // Keep type-only declarations out of coverage while allowing the
          // ws-events runtime contract to be measured by Sonar.
          'src/renderer/types/auth.ts',
          'src/renderer/types/chat.ts',
          'src/renderer/types/server.ts',
          '**/*.d.ts',
          // Test files are not coverage targets themselves.
          '**/*.test.{ts,tsx,mts}',
        ],
        // Coverage thresholds enforced by SonarQube Quality Gate, not here.
      },
      alias: {
        '@': path.resolve(__dirname, './src'),
        '@shared': path.resolve(__dirname, '../../shared'),
      },
    },
    server: {
      port: 3001,
      strictPort: true,
    },
    build: {
      outDir: 'dist/renderer',
      sourcemap: 'hidden',
      emptyOutDir: true,
      rollupOptions: {
        input: {
          main: path.resolve(__dirname, 'index.html'),
        },
        output: {
          manualChunks(id) {
            if (
              id.includes('node_modules/react-dom/') ||
              id.includes('node_modules/react/') ||
              id.includes('node_modules/react-router-dom/')
            ) {
              return 'vendor-react';
            }
            if (id.includes('node_modules/zustand/')) {
              return 'vendor-zustand';
            }
            if (id.includes('node_modules/lucide-react/')) {
              return 'vendor-icons';
            }
          },
        },
      },
    },
    base: './',
  };
});
