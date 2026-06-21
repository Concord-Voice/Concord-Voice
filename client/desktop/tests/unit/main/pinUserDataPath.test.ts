// @vitest-environment node
import { describe, it, expect, vi, beforeEach } from 'vitest';
import path from 'node:path';

const mockGetPath = vi.hoisted(() => vi.fn());
const mockSetPath = vi.hoisted(() => vi.fn());

vi.mock('electron', () => ({
  app: { getPath: mockGetPath, setPath: mockSetPath },
}));

describe('pinUserDataPath', () => {
  beforeEach(() => {
    mockGetPath.mockReset();
    mockSetPath.mockReset();
    mockGetPath.mockImplementation((key: string) =>
      key === 'appData' ? '/fake/AppData' : '/fake/other'
    );
  });

  it('pins userData to <appData>/ConcordVoice regardless of productName', async () => {
    vi.resetModules();
    const { pinUserDataPath, PINNED_USER_DATA_DIR } =
      await import('../../../src/main/pinUserDataPath');
    pinUserDataPath();
    expect(PINNED_USER_DATA_DIR).toBe('ConcordVoice');
    expect(mockSetPath).toHaveBeenCalledWith(
      'userData',
      path.join('/fake/AppData', 'ConcordVoice')
    );
  });

  it('runs the pin as an import-time side effect (before any consumer reads userData)', async () => {
    vi.resetModules();
    mockSetPath.mockClear();
    await import('../../../src/main/pinUserDataPath');
    // The module-load side effect must have already called setPath exactly once.
    expect(mockSetPath).toHaveBeenCalledWith(
      'userData',
      path.join('/fake/AppData', 'ConcordVoice')
    );
  });
});
