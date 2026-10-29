import { afterEach, describe, expect, it, vi } from 'vitest';

type Listener = () => void;

function stubReducedMotion(matches: boolean) {
  const listeners: Listener[] = [];
  const query = {
    matches,
    media: '(prefers-reduced-motion: reduce)',
    addEventListener: (_: string, fn: Listener) => listeners.push(fn),
    removeEventListener: vi.fn(),
  };
  vi.stubGlobal(
    'matchMedia',
    vi.fn((q: string) => (q.includes('reduced-motion') ? query : { ...query, matches: false }))
  );
  return {
    set(next: boolean) {
      query.matches = next;
      listeners.forEach((fn) => fn());
    },
  };
}

async function loadStore() {
  vi.resetModules();
  return (await import('@/renderer/stores/ui/settingsStore')).useSettingsStore;
}

describe('settingsStore reduced motion', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('reduces animations when the OS asks, even with the setting off', async () => {
    stubReducedMotion(true);
    const store = await loadStore();
    store.getState().setReduceAnimations(false);
    expect(document.documentElement.dataset.reduceAnimations).toBe('true');
  });

  it('follows OS preference changes and keeps the setting as an override', async () => {
    const os = stubReducedMotion(false);
    const store = await loadStore();
    store.getState().setReduceAnimations(false);
    expect(document.documentElement.dataset.reduceAnimations).toBe('false');
    os.set(true);
    expect(document.documentElement.dataset.reduceAnimations).toBe('true');
    os.set(false);
    expect(document.documentElement.dataset.reduceAnimations).toBe('false');
    store.getState().setReduceAnimations(true);
    os.set(false);
    expect(document.documentElement.dataset.reduceAnimations).toBe('true');
  });
});
