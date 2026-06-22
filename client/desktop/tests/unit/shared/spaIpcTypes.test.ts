import { describe, it, expect } from 'vitest';
// Relative import (not @/ alias) so Istanbul coverage instrumentation tracks the file correctly.
import {
  isRendererSelfHealRequest,
  RENDERER_SELF_HEAL_REASONS,
  MAIN_PROCESS_SELF_HEAL_REASONS,
} from '../../../src/shared/spaIpcTypes';

/**
 * Unit tests for the SPA-self-heal IPC trust-boundary types and runtime guard.
 *
 * The renderer/main IPC boundary erases TypeScript types at runtime, so the
 * `isRendererSelfHealRequest` guard is the load-bearing security check for
 * preventing a malicious or buggy renderer from passing main-process-only
 * reasons (`'main-frame-load'`, `'sub-resource'`) and corrupting recovery-
 * flow state. These tests pin the closed-union behavior.
 */

describe('RENDERER_SELF_HEAL_REASONS', () => {
  it('contains exactly the two renderer-allowed reasons', () => {
    expect(RENDERER_SELF_HEAL_REASONS).toEqual(['chunk-load', 'chunk-import-rejected']);
  });

  it('does not include any main-process-only reasons', () => {
    const renderer: readonly string[] = RENDERER_SELF_HEAL_REASONS;
    expect(renderer).not.toContain('main-frame-load');
    expect(renderer).not.toContain('sub-resource');
  });
});

describe('MAIN_PROCESS_SELF_HEAL_REASONS', () => {
  it('contains exactly the two main-process-only reasons', () => {
    expect(MAIN_PROCESS_SELF_HEAL_REASONS).toEqual(['main-frame-load', 'sub-resource']);
  });

  it('does not include any renderer-allowed reasons (closed-union disjoint check)', () => {
    const main: readonly string[] = MAIN_PROCESS_SELF_HEAL_REASONS;
    expect(main).not.toContain('chunk-load');
    expect(main).not.toContain('chunk-import-rejected');
  });
});

describe('isRendererSelfHealRequest — runtime trust-boundary guard', () => {
  it('accepts {reason: chunk-load}', () => {
    expect(isRendererSelfHealRequest({ reason: 'chunk-load' })).toBe(true);
  });

  it('accepts {reason: chunk-import-rejected}', () => {
    expect(isRendererSelfHealRequest({ reason: 'chunk-import-rejected' })).toBe(true);
  });

  it('accepts {reason, url} when url is a string', () => {
    expect(
      isRendererSelfHealRequest({
        reason: 'chunk-load',
        url: 'https://api.concordvoice.chat/spa/abc1234/assets/x.js',
      })
    ).toBe(true);
  });

  it('accepts {reason} when url is omitted (url is optional)', () => {
    expect(isRendererSelfHealRequest({ reason: 'chunk-load' })).toBe(true);
  });

  it('rejects main-process-only reason: main-frame-load', () => {
    // Trust-boundary check — a malicious renderer should not be able to pass
    // a main-process reason. Critical for the recovery flow's reason-context
    // integrity.
    expect(isRendererSelfHealRequest({ reason: 'main-frame-load' })).toBe(false);
  });

  it('rejects main-process-only reason: sub-resource', () => {
    expect(isRendererSelfHealRequest({ reason: 'sub-resource' })).toBe(false);
  });

  it('rejects unknown reason string', () => {
    expect(isRendererSelfHealRequest({ reason: 'arbitrary-string' })).toBe(false);
  });

  it('rejects when reason is not a string', () => {
    expect(isRendererSelfHealRequest({ reason: 123 })).toBe(false);
    expect(isRendererSelfHealRequest({ reason: null })).toBe(false);
    expect(isRendererSelfHealRequest({ reason: { nested: true } })).toBe(false);
  });

  it('rejects when reason is missing', () => {
    expect(isRendererSelfHealRequest({})).toBe(false);
    expect(
      isRendererSelfHealRequest({ url: 'https://api.concordvoice.chat/spa/abc1234/assets/x.js' })
    ).toBe(false);
  });

  it('rejects when url is present but not a string', () => {
    expect(isRendererSelfHealRequest({ reason: 'chunk-load', url: 123 })).toBe(false);
    expect(isRendererSelfHealRequest({ reason: 'chunk-load', url: null })).toBe(false);
    expect(isRendererSelfHealRequest({ reason: 'chunk-load', url: { nested: true } })).toBe(false);
  });

  it('rejects null', () => {
    expect(isRendererSelfHealRequest(null)).toBe(false);
  });

  it('rejects undefined', () => {
    expect(isRendererSelfHealRequest(undefined)).toBe(false);
  });

  it('rejects primitive values', () => {
    expect(isRendererSelfHealRequest('string')).toBe(false);
    expect(isRendererSelfHealRequest(42)).toBe(false);
    expect(isRendererSelfHealRequest(true)).toBe(false);
  });

  it('rejects arrays', () => {
    expect(isRendererSelfHealRequest(['chunk-load'])).toBe(false);
    expect(isRendererSelfHealRequest([{ reason: 'chunk-load' }])).toBe(false);
  });

  it('narrows the type on accept (TypeScript compile-time check)', () => {
    const value: unknown = { reason: 'chunk-load', url: 'https://x/spa/abc1234/assets/y.js' };
    if (isRendererSelfHealRequest(value)) {
      // Inside the guard, value is narrowed to RendererSelfHealRequest.
      // This is a compile-time check — the test passes if the file
      // typechecks at all. The runtime expectation is the same as above.
      expect(value.reason === 'chunk-load' || value.reason === 'chunk-import-rejected').toBe(true);
      expect(typeof value.url === 'string' || value.url === undefined).toBe(true);
    } else {
      throw new Error('expected guard to accept');
    }
  });
});
