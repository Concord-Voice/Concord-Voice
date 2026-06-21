import { describe, it, expect, vi, beforeEach } from 'vitest';
import { renderHook } from '@testing-library/react';

let createStore: typeof import('@/renderer/utils/createStore').createStore;

describe('createStore', () => {
  beforeEach(async () => {
    vi.resetModules();
  });

  it('returns a working Zustand store', async () => {
    const mod = await import('@/renderer/utils/createStore');
    createStore = mod.createStore;

    const useStore = createStore<{ count: number; inc: () => void }>()((set) => ({
      count: 0,
      inc: () => set((s) => ({ count: s.count + 1 })),
    }));

    const { result } = renderHook(() => useStore((s) => s.count));
    expect(result.current).toBe(0);
  });

  it('warns in dev when no selector is provided', async () => {
    vi.stubEnv('NODE_ENV', 'development');
    const mod = await import('@/renderer/utils/createStore');
    createStore = mod.createStore;

    const useStore = createStore<{ count: number }>()((set) => ({
      count: 0,
    }));

    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    renderHook(() => useStore());
    expect(warnSpy).toHaveBeenCalledWith(expect.stringContaining('without a selector'));
    warnSpy.mockRestore();
    vi.unstubAllEnvs();
  });

  it('does not warn in production', async () => {
    vi.stubEnv('NODE_ENV', 'production');
    const mod = await import('@/renderer/utils/createStore');
    createStore = mod.createStore;

    const useStore = createStore<{ count: number }>()((set) => ({
      count: 0,
    }));

    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    renderHook(() => useStore());
    expect(warnSpy).not.toHaveBeenCalled();
    warnSpy.mockRestore();
    vi.unstubAllEnvs();
  });

  it('does not warn when a selector is provided', async () => {
    vi.stubEnv('NODE_ENV', 'development');
    const mod = await import('@/renderer/utils/createStore');
    createStore = mod.createStore;

    const useStore = createStore<{ count: number }>()((set) => ({
      count: 0,
    }));

    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
    renderHook(() => useStore((s) => s.count));
    expect(warnSpy).not.toHaveBeenCalled();
    warnSpy.mockRestore();
    vi.unstubAllEnvs();
  });
});
