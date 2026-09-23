// @vitest-environment node
//
// #2367 part 2 — the preload's UI zoom bridge.
//
// The main world may be remote-SPA code, so the preload re-validates every
// factor instead of trusting the renderer's clamp. Per tests.md § "Test the
// consumer, not the handshake", the sanitizer's verdict is paired with what
// `webFrame` is actually asked to do: a rejected value must never reach it.
//
// NOTE ON COVERAGE. `src/preload/**` is in `sonar.coverage.exclusions` and
// outside the Istanbul `include`, exactly as for `audiocapRelay.test.ts`. This
// file exists because the bridge is a trust boundary, not because a number
// needs it.
import { describe, it, expect, vi } from 'vitest';

import {
  UI_ZOOM_MAX,
  UI_ZOOM_MIN,
  createSetZoomFactor,
  sanitizeZoomFactor,
} from '../../../src/preload/uiZoom';

/** Values a hostile or buggy main world could hand the bridge. None is a finite
 *  number, so every one must be ignored — not clamped, not defaulted. */
const REJECTED: Array<[string, unknown]> = [
  ['NaN', Number.NaN],
  ['Infinity', Number.POSITIVE_INFINITY],
  ['-Infinity', Number.NEGATIVE_INFINITY],
  ['a numeric string', '1.5'],
  ['an object', {}],
  ['an object with valueOf', { valueOf: () => 1.5 }],
  ['an array', [1.5]],
  ['undefined', undefined],
  ['null', null],
  ['a boolean', true],
  ['a bigint', 2n],
];

describe('sanitizeZoomFactor', () => {
  it('passes an in-range factor through unchanged', () => {
    expect(sanitizeZoomFactor(0.5)).toBe(0.5);
    expect(sanitizeZoomFactor(1)).toBe(1);
    expect(sanitizeZoomFactor(1.6)).toBe(1.6);
    expect(sanitizeZoomFactor(2)).toBe(2);
  });

  it('clamps an out-of-range factor to 50–200%', () => {
    expect(UI_ZOOM_MIN).toBe(0.5);
    expect(UI_ZOOM_MAX).toBe(2);
    expect(sanitizeZoomFactor(0.1)).toBe(0.5);
    expect(sanitizeZoomFactor(5)).toBe(2);
    // webFrame throws for <= 0; the clamp is what keeps that out of the main world.
    expect(sanitizeZoomFactor(0)).toBe(0.5);
    expect(sanitizeZoomFactor(-1)).toBe(0.5);
  });

  it.each(REJECTED)('rejects %s as null', (_label, value) => {
    expect(sanitizeZoomFactor(value)).toBeNull();
  });
});

describe('createSetZoomFactor (the bridge)', () => {
  it('asks webFrame for the sanitized factor', () => {
    const frame = { setZoomFactor: vi.fn<(factor: number) => void>() };
    const setZoomFactor = createSetZoomFactor(frame);
    setZoomFactor(1.6);
    setZoomFactor(5);
    setZoomFactor(0.1);
    expect(frame.setZoomFactor.mock.calls).toEqual([[1.6], [2], [0.5]]);
  });

  it.each(REJECTED)('never calls webFrame for %s', (_label, value) => {
    const frame = { setZoomFactor: vi.fn<(factor: number) => void>() };
    const setZoomFactor = createSetZoomFactor(frame);
    expect(() => setZoomFactor(value)).not.toThrow();
    expect(frame.setZoomFactor).not.toHaveBeenCalled();
  });
});
