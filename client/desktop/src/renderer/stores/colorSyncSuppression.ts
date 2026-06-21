/**
 * Color-scheme sync suppression flag — a tiny, dependency-free holder extracted
 * from settingsStore.
 *
 * Why this is its own module: `settingsStore` statically imports `userStore`
 * (settingsStore.ts), so `userStore` cannot statically import `settingsStore`
 * without forming a circular dependency. To flip this flag during `logout()`,
 * `userStore` previously reached the setter through a fire-and-forget,
 * **unawaited** `import('./settingsStore').then(...)`. Under vitest's lazy
 * Vite-SSR module transform, that in-flight load of `settingsStore` (and its
 * static `overlayColors` import) could still be resolving when a test worker's
 * jsdom environment was torn down — producing an intermittent
 * `EnvironmentTeardownError: Cannot load .../overlayColors.ts`: all tests pass,
 * but the shard exits 1 on the unhandled rejection. (Documented vitest 4.x
 * behavior — vitest-dev/vitest#9872 / #8649.)
 *
 * Housing the flag in this leaf module — which imports NOTHING — lets both
 * `userStore` and `settingsStore` import it statically, with no cycle and no
 * deferred load. The teardown race is eliminated by construction.
 *
 * Do NOT reintroduce a dynamic `import('./settingsStore')` to flip this flag.
 * A regression test in `tests/unit/stores/userStore.test.ts` asserts the
 * suppression reset happens synchronously during `logout()`.
 */

let syncSuppressed = false;

/** Suppress color-scheme server sync (used during draft-settings mode and logout). */
export function setSyncSuppressed(suppressed: boolean): void {
  syncSuppressed = suppressed;
}

/** Whether color-scheme server sync is currently suppressed. */
export function isSyncSuppressed(): boolean {
  return syncSuppressed;
}
