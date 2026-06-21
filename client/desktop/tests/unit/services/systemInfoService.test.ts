import { beforeEach, describe, expect, it, vi } from 'vitest';
import { collect } from '@/renderer/services/systemInfoService';

// Mock connection store before importing it via the service.
vi.mock('@/renderer/stores/connectionStore', () => ({
  useConnectionStore: {
    getState: () => ({ phase: 'stable' }),
  },
}));

describe('systemInfoService.collect', () => {
  beforeEach(() => {
    // Reset electron mock between tests so probe failures don't leak.
    (globalThis as Record<string, unknown>).electron = {
      getVersion: vi.fn().mockResolvedValue('0.2.0'),
      getMachineId: vi.fn().mockResolvedValue('4c33734c-aaaa-bbbb-cccc-dddddddddddd'),
    };
  });

  it('returns app version from the electron IPC', async () => {
    const info = await collect();
    expect(info.appVersion).toBe('0.2.0');
  });

  it('truncates the machine ID to the first 8 chars', async () => {
    const info = await collect();
    expect(info.machineIdPrefix).toBe('4c33734c');
    expect(info.machineIdPrefix).toHaveLength(8);
  });

  it('returns "unknown" for app version when the IPC is unavailable', async () => {
    (globalThis as Record<string, unknown>).electron = {};
    const info = await collect();
    expect(info.appVersion).toBe('unknown');
  });

  it('returns "unknown" for machine ID when the IPC is unavailable', async () => {
    (globalThis as Record<string, unknown>).electron = {};
    const info = await collect();
    expect(info.machineIdPrefix).toBe('unknown');
  });

  it('returns "unknown" for app version when the IPC throws', async () => {
    (globalThis as Record<string, unknown>).electron = {
      getVersion: vi.fn().mockRejectedValue(new Error('IPC error')),
      getMachineId: vi.fn().mockResolvedValue('aaaa11112222333344445555'),
    };
    const info = await collect();
    expect(info.appVersion).toBe('unknown');
  });

  it('includes connectionPhase from the connection store', async () => {
    const info = await collect();
    expect(info.connectionPhase).toBe('stable');
  });

  it('includes display info shaped from screen + devicePixelRatio', async () => {
    const info = await collect();
    expect(info.display).toBeDefined();
    // JSDOM's screen returns 0 for width/height by default — we don't
    // assert non-zero, only that the shape is well-formed. Real-renderer
    // testing happens in Playwright E2E.
    expect(typeof info.display?.width).toBe('number');
    expect(typeof info.display?.height).toBe('number');
    expect(info.display?.scaleFactor).toBeGreaterThan(0);
  });

  it('includes platform from navigator', async () => {
    const info = await collect();
    // JSDOM may return an empty string for navigator.platform on some
    // versions; we only assert it's a string. The full collection is
    // exercised by Playwright E2E in #159.
    expect(typeof info.platform).toBe('string');
  });

  it('does NOT expose the full machine ID (privacy invariant)', async () => {
    const info = await collect();
    expect(info.machineIdPrefix).not.toContain('-');
    expect(info.machineIdPrefix.length).toBeLessThanOrEqual(8);
  });
});
