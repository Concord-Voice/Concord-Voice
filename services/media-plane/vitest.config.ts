import { defineConfig } from 'vitest/config';
import path from 'path';

export default defineConfig({
  resolve: {
    alias: {
      // Redirect deep mediasoup imports that bypass the exports map.
      // These are type-only in source but vitest still resolves the .js paths.
      'mediasoup/node/lib/rtpParametersTypes.js': path.resolve(
        import.meta.dirname,
        './tests/mocks/mediasoup-types-stub.ts'
      ),
      'mediasoup/node/lib/types.js': path.resolve(
        import.meta.dirname,
        './tests/mocks/mediasoup-types-stub.ts'
      ),
      '@': path.resolve(import.meta.dirname, './src'),
    },
  },
  test: {
    globals: true,
    environment: 'node',
    setupFiles: ['./tests/setup.ts'],
    include: ['tests/**/*.test.ts'],
    testTimeout: 10_000,
    hookTimeout: 10_000,
    coverage: {
      provider: 'istanbul',
      reporter: ['text', 'html', 'lcov'],
      include: ['src/**/*.ts'],
      exclude: ['src/index.ts'],
    },
  },
});
