import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { isWayland } from '../../../src/main/waylandDetect';

describe('isWayland', () => {
  const originalEnv = { ...process.env };

  beforeEach(() => {
    delete process.env.XDG_SESSION_TYPE;
    delete process.env.WAYLAND_DISPLAY;
  });

  afterEach(() => {
    process.env = { ...originalEnv };
  });

  it('returns true when XDG_SESSION_TYPE is "wayland"', () => {
    process.env.XDG_SESSION_TYPE = 'wayland';
    expect(isWayland()).toBe(true);
  });

  it('returns true when WAYLAND_DISPLAY is set (even without XDG_SESSION_TYPE)', () => {
    process.env.WAYLAND_DISPLAY = 'wayland-0';
    expect(isWayland()).toBe(true);
  });

  it('returns false when XDG_SESSION_TYPE is "x11"', () => {
    process.env.XDG_SESSION_TYPE = 'x11';
    expect(isWayland()).toBe(false);
  });

  it('returns false when no env vars are set (macOS / Windows / unset Linux)', () => {
    expect(isWayland()).toBe(false);
  });

  it('returns false when XDG_SESSION_TYPE is set to an unexpected value', () => {
    process.env.XDG_SESSION_TYPE = 'tty';
    expect(isWayland()).toBe(false);
  });
});
