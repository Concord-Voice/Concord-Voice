import { describe, it, expect } from 'vitest';
import {
  FREE_ATTACHMENT_BYTES,
  PREMIUM_ATTACHMENT_BYTES,
  INTERIM_CLIENT_ATTACHMENT_CEILING_BYTES,
  MAX_DECRYPTABLE_ATTACHMENT_BYTES,
  resolveAttachmentLimit,
  formatLimitBytes,
} from '@/renderer/utils/entitlementLimits';

describe('attachment limit constants', () => {
  it('free mirrors the Go free floor (entitlements.go:111)', () => {
    expect(FREE_ATTACHMENT_BYTES).toBe(33_554_432);
  });

  // Regression for the wrong-axis bug (#2157): 536_870_912 is the Mach 3
  // SERVER-wide number, not the personal premium ceiling. Using it made the
  // composer promise free users that Premium raises their limit to 512 MB.
  it('premium mirrors the Go premium user axis, not the Mach 3 server axis', () => {
    expect(PREMIUM_ATTACHMENT_BYTES).toBe(268_435_456);
    expect(PREMIUM_ATTACHMENT_BYTES).not.toBe(536_870_912);
  });

  it('the download guard sits at the premium entitlement plus multipart slack', () => {
    expect(MAX_DECRYPTABLE_ATTACHMENT_BYTES).toBe(268_435_456 + 4096);
  });

  // The invariant that makes PR 1 forward-compatible with PR 2's larger files.
  it('the upload ceiling never exceeds the download capability', () => {
    expect(INTERIM_CLIENT_ATTACHMENT_CEILING_BYTES).toBeLessThanOrEqual(
      MAX_DECRYPTABLE_ATTACHMENT_BYTES
    );
  });
});

describe('resolveAttachmentLimit', () => {
  it('returns the free entitlement untouched (below the ceiling)', () => {
    const r = resolveAttachmentLimit({ userMaxAttachmentBytes: FREE_ATTACHMENT_BYTES });
    expect(r).toEqual({
      limitBytes: FREE_ATTACHMENT_BYTES,
      source: 'entitlement',
      entitlementBytes: FREE_ATTACHMENT_BYTES,
    });
  });

  it('clamps premium down to the interim ceiling and reports the clamp', () => {
    const r = resolveAttachmentLimit({ userMaxAttachmentBytes: PREMIUM_ATTACHMENT_BYTES });
    expect(r.limitBytes).toBe(INTERIM_CLIENT_ATTACHMENT_CEILING_BYTES);
    expect(r.source).toBe('client-ceiling');
    // The verbatim entitlement survives so the copy can name BOTH numbers.
    expect(r.entitlementBytes).toBe(PREMIUM_ATTACHMENT_BYTES);
  });

  it('a free user can never reach the client-ceiling branch', () => {
    const r = resolveAttachmentLimit({ userMaxAttachmentBytes: FREE_ATTACHMENT_BYTES });
    expect(r.source).toBe('entitlement');
  });

  it.each([0, -1, Number.NaN, Number.POSITIVE_INFINITY])(
    'falls back to the free floor for a nonsense entitlement (%p) and never escalates',
    (bad) => {
      const r = resolveAttachmentLimit({ userMaxAttachmentBytes: bad as number });
      expect(r.limitBytes).toBe(FREE_ATTACHMENT_BYTES);
      expect(r.entitlementBytes).toBe(FREE_ATTACHMENT_BYTES);
    }
  );

  it('treats an unhydrated store (FREE_ENTITLEMENT) as the free floor with no hydration branch', () => {
    // subscriptionStore's FREE_ENTITLEMENT.maxAttachmentBytes IS 33_554_432 when
    // unhydrated, so the resolver needs no `hydrated` argument at all. If this
    // test ever needs a hydration flag passed in, the design has regressed (R1)
    // and re-introduced the #2172 premium-downgrade bug on a new axis.
    const r = resolveAttachmentLimit({ userMaxAttachmentBytes: 33_554_432 });
    expect(r.limitBytes).toBe(FREE_ATTACHMENT_BYTES);
  });

  it('honours a degraded-but-hydrated premium entitlement instead of clamping to free', () => {
    // #2172: a reconnect blip preserves last-known-good, so the store still
    // reports premium. The resolver must not second-guess it.
    const r = resolveAttachmentLimit({ userMaxAttachmentBytes: PREMIUM_ATTACHMENT_BYTES });
    expect(r.entitlementBytes).toBe(PREMIUM_ATTACHMENT_BYTES);
    expect(r.limitBytes).toBeGreaterThan(FREE_ATTACHMENT_BYTES);
  });

  describe('#1556 server-axis seam (parameter exists, unused in PR 1)', () => {
    it('ignores an absent server value', () => {
      const r = resolveAttachmentLimit({ userMaxAttachmentBytes: FREE_ATTACHMENT_BYTES });
      expect(r.entitlementBytes).toBe(FREE_ATTACHMENT_BYTES);
    });

    it('takes the larger axis when a server value is supplied', () => {
      const r = resolveAttachmentLimit({
        userMaxAttachmentBytes: FREE_ATTACHMENT_BYTES,
        serverMaxUploadBytes: 100_000_000,
      });
      expect(r.entitlementBytes).toBe(100_000_000);
    });

    it('keeps the user axis when the server grant is smaller', () => {
      const r = resolveAttachmentLimit({
        userMaxAttachmentBytes: FREE_ATTACHMENT_BYTES,
        serverMaxUploadBytes: 1_000_000,
      });
      expect(r.entitlementBytes).toBe(FREE_ATTACHMENT_BYTES);
    });

    it.each([0, Number.NaN])('treats %p as absent', (v) => {
      const r = resolveAttachmentLimit({
        userMaxAttachmentBytes: FREE_ATTACHMENT_BYTES,
        serverMaxUploadBytes: v as number,
      });
      expect(r.entitlementBytes).toBe(FREE_ATTACHMENT_BYTES);
    });

    // VULN-003 (#2157 adversarial review): the caller passes an object literal,
    // so a bare property read walks the prototype chain.
    it('ignores a polluted Object.prototype.serverMaxUploadBytes', () => {
      const proto = Object.prototype as unknown as Record<string, unknown>;
      try {
        proto.serverMaxUploadBytes = 1e12;
        const r = resolveAttachmentLimit({ userMaxAttachmentBytes: FREE_ATTACHMENT_BYTES });
        expect(r.entitlementBytes).toBe(FREE_ATTACHMENT_BYTES);
        expect(r.limitBytes).toBe(FREE_ATTACHMENT_BYTES);
      } finally {
        delete proto.serverMaxUploadBytes;
      }
    });

    // `max` is the documented composition rule, NOT an escapable escalation:
    // the server-wide Mach grant lifts every member above their personal cap.
    it('keeps max() semantics when the axis is genuinely supplied', () => {
      const r = resolveAttachmentLimit({
        userMaxAttachmentBytes: FREE_ATTACHMENT_BYTES,
        serverMaxUploadBytes: 100_000_000,
      });
      expect(r.entitlementBytes).toBe(100_000_000);
    });

    it('does NOT honour the negative "unlimited (selfhost)" sentinel — fail closed', () => {
      // -1 genuinely means unlimited on the Go side (entitlements.go:211-215)
      // and in the client mirror's header. PR 1 deliberately declines to act on
      // it: the server enforces the USER axis alone today, so treating a
      // selfhost server as unlimited would let the client accept a file the
      // server answers with 413. #1556 must translate the sentinel to a real
      // byte ceiling before passing it in.
      const r = resolveAttachmentLimit({
        userMaxAttachmentBytes: FREE_ATTACHMENT_BYTES,
        serverMaxUploadBytes: -1,
      });
      expect(r.entitlementBytes).toBe(FREE_ATTACHMENT_BYTES);
      expect(r.limitBytes).toBe(FREE_ATTACHMENT_BYTES);
    });
  });
});

describe('formatLimitBytes', () => {
  it('renders whole-megabyte limits exactly as the pricing page writes them', () => {
    expect(formatLimitBytes(33_554_432)).toBe('32 MB');
    expect(formatLimitBytes(268_435_456)).toBe('256 MB');
    expect(formatLimitBytes(134_217_728)).toBe('128 MB');
  });

  it('leaves a fractional size alone', () => {
    expect(formatLimitBytes(189_267_148)).toBe('180.5 MB');
  });

  it('does not mangle sub-megabyte units', () => {
    expect(formatLimitBytes(512)).toBe('512 B');
    expect(formatLimitBytes(2048)).toBe('2 KB');
  });
});
