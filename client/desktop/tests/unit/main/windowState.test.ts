import { describe, it, expect, beforeEach, vi } from 'vitest';

const {
  mockGetPath,
  mockReadFileSync,
  mockWriteFileSync,
  mockRenameSync,
  mockGetAllDisplays,
  mockIsWayland,
} = vi.hoisted(() => ({
  mockGetPath: vi.fn(() => '/tmp/concord-test-userdata'),
  mockReadFileSync: vi.fn(),
  mockWriteFileSync: vi.fn(),
  mockRenameSync: vi.fn(),
  mockGetAllDisplays: vi.fn(),
  mockIsWayland: vi.fn(() => false),
}));

vi.mock('electron', () => ({
  app: { getPath: mockGetPath },
  screen: { getAllDisplays: mockGetAllDisplays },
}));

vi.mock('node:fs', () => ({
  default: {
    readFileSync: mockReadFileSync,
    writeFileSync: mockWriteFileSync,
    renameSync: mockRenameSync,
  },
  readFileSync: mockReadFileSync,
  writeFileSync: mockWriteFileSync,
  renameSync: mockRenameSync,
}));

vi.mock('@/main/waylandDetect', () => ({
  isWayland: mockIsWayland,
}));

import { loadWindowState, saveWindowState, attachWindowState } from '@/main/windowState';

const display = { workArea: { x: 0, y: 0, width: 1920, height: 1080 } };

describe('loadWindowState', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockGetAllDisplays.mockReturnValue([display]);
    mockIsWayland.mockReturnValue(false);
  });

  it('returns default centered config when file does not exist', () => {
    mockReadFileSync.mockImplementation(() => {
      throw Object.assign(new Error('ENOENT'), { code: 'ENOENT' });
    });
    const result = loadWindowState();
    expect(result).toEqual({
      x: undefined,
      y: undefined,
      width: 1200,
      height: 800,
      isMaximized: false,
    });
  });

  it('returns default centered config when JSON is corrupted', () => {
    mockReadFileSync.mockReturnValue('not-json{{{');
    const result = loadWindowState();
    expect(result.width).toBe(1200);
    expect(result.height).toBe(800);
  });

  it('returns saved bounds when JSON is valid and passes validator', () => {
    mockReadFileSync.mockReturnValue(
      JSON.stringify({ x: 100, y: 100, width: 1200, height: 800, isMaximized: false })
    );
    const result = loadWindowState();
    expect(result.x).toBe(100);
    expect(result.y).toBe(100);
  });

  it('returns default config when validator rejects (off-screen)', () => {
    mockReadFileSync.mockReturnValue(
      JSON.stringify({ x: 9999, y: 9999, width: 1200, height: 800, isMaximized: false })
    );
    const result = loadWindowState();
    expect(result.x).toBeUndefined();
  });

  it('omits x/y on Wayland (compositor-controlled)', () => {
    mockIsWayland.mockReturnValue(true);
    mockReadFileSync.mockReturnValue(
      JSON.stringify({ x: 0, y: 0, width: 1200, height: 800, isMaximized: false })
    );
    const result = loadWindowState();
    expect(result.x).toBeUndefined();
    expect(result.y).toBeUndefined();
    expect(result.width).toBe(1200);
    expect(result.height).toBe(800);
  });
});

describe('saveWindowState', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockIsWayland.mockReturnValue(false);
  });

  it('writes JSON atomically (tmp + rename)', () => {
    saveWindowState({
      bounds: { x: 100, y: 100, width: 1200, height: 800 },
      isMaximized: false,
    });
    expect(mockWriteFileSync).toHaveBeenCalledTimes(1);
    expect(mockRenameSync).toHaveBeenCalledTimes(1);
    const [tmpPath] = mockWriteFileSync.mock.calls[0];
    const [renameFrom, renameTo] = mockRenameSync.mock.calls[0];
    expect(tmpPath).toBe(renameFrom);
    expect(renameTo).toContain('window-state.json');
  });

  it('omits x/y from saved JSON on Wayland', () => {
    mockIsWayland.mockReturnValue(true);
    saveWindowState({
      bounds: { x: 100, y: 100, width: 1200, height: 800 },
      isMaximized: false,
    });
    const [, json] = mockWriteFileSync.mock.calls[0];
    const parsed = JSON.parse(json as string);
    expect(parsed.x).toBeUndefined();
    expect(parsed.y).toBeUndefined();
    expect(parsed.width).toBe(1200);
    expect(parsed.height).toBe(800);
  });
});

describe('attachWindowState — debounce + lifecycle', () => {
  it('wires resize, move, maximize, unmaximize, close listeners', () => {
    const events: string[] = [];
    const mockWindow = {
      on: vi.fn((event: string) => {
        events.push(event);
      }),
      getBounds: vi.fn(() => ({ x: 0, y: 0, width: 1200, height: 800 })),
      isMaximized: vi.fn(() => false),
    };
    attachWindowState(mockWindow as never);
    expect(events).toContain('resize');
    expect(events).toContain('move');
    expect(events).toContain('maximize');
    expect(events).toContain('unmaximize');
    expect(events).toContain('close');
  });
});
