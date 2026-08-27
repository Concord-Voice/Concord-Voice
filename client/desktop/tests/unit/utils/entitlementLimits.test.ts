import { describe, it, expect } from 'vitest';
import {
  FREE_ATTACHMENT_BYTES,
  PREMIUM_ATTACHMENT_BYTES,
  LEGACY_UPLOAD_PATH_CEILING_BYTES,
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

  it('the download guard is derived from the v2 envelope, not a multipart guess', () => {
    // Was PREMIUM + 4096, with a comment claiming the 4096 mirrored the server's
    // multipart-header allowance. That derivation is wrong for this format: the
    // guard must admit a maximum-size v2 blob, whose overhead is the 28-byte
    // header plus 28 bytes per chunk.
    expect(MAX_DECRYPTABLE_ATTACHMENT_BYTES).toBe(268_435_456 + 28 + 28 * 32);
    expect(MAX_DECRYPTABLE_ATTACHMENT_BYTES).toBe(268_436_380);
  });

  it('still admits a maximum-size LEGACY blob', () => {
    // Existing attachments are [IV:12][ct+tag:16] = plaintext + 28.
    expect(MAX_DECRYPTABLE_ATTACHMENT_BYTES).toBeGreaterThanOrEqual(268_435_456 + 28);
  });

  // The invariant that makes PR 1 forward-compatible with PR 2's larger files.
  it('the upload ceiling never exceeds the download capability', () => {
    expect(LEGACY_UPLOAD_PATH_CEILING_BYTES).toBeLessThanOrEqual(MAX_DECRYPTABLE_ATTACHMENT_BYTES);
  });
});

describe('resolveAttachmentLimit', () => {
  it('returns the free entitlement untouched (below the ceiling)', () => {
    const r = resolveAttachmentLimit({
      chunkedUploadSupported: false,
      userMaxAttachmentBytes: FREE_ATTACHMENT_BYTES,
    });
    expect(r).toEqual({
      limitBytes: FREE_ATTACHMENT_BYTES,
      source: 'entitlement',
      entitlementBytes: FREE_ATTACHMENT_BYTES,
    });
  });

  it('clamps premium down to the legacy ceiling when the server lacks chunked upload', () => {
    const r = resolveAttachmentLimit({
      chunkedUploadSupported: false,
      userMaxAttachmentBytes: PREMIUM_ATTACHMENT_BYTES,
    });
    expect(r.limitBytes).toBe(LEGACY_UPLOAD_PATH_CEILING_BYTES);
    expect(r.source).toBe('legacy-upload-path');
    // The verbatim entitlement survives so the copy can name BOTH numbers.
    expect(r.entitlementBytes).toBe(PREMIUM_ATTACHMENT_BYTES);
  });

  it('does NOT clamp when the server supports chunked upload', () => {
    const r = resolveAttachmentLimit({
      chunkedUploadSupported: true,
      userMaxAttachmentBytes: PREMIUM_ATTACHMENT_BYTES,
    });
    expect(r.limitBytes).toBe(PREMIUM_ATTACHMENT_BYTES);
    expect(r.source).toBe('entitlement');
  });

  it('the ceiling constant survives — it is renamed, not deleted', () => {
    // Concord is self-hostable, so a current desktop build can be pointed at a
    // control plane that predates the session routes. On that fallback the
    // renderer-memory ceiling is still real, which is why the constant and the
    // source union both survive rather than collapsing.
    expect(LEGACY_UPLOAD_PATH_CEILING_BYTES).toBe(134_217_728);
  });

  it('a free user can never reach the legacy-upload-path branch', () => {
    const r = resolveAttachmentLimit({
      chunkedUploadSupported: false,
      userMaxAttachmentBytes: FREE_ATTACHMENT_BYTES,
    });
    expect(r.source).toBe('entitlement');
  });

  it.each([0, -1, Number.NaN, Number.POSITIVE_INFINITY])(
    'falls back to the free floor for a nonsense entitlement (%p) and never escalates',
    (bad) => {
      const r = resolveAttachmentLimit({
        chunkedUploadSupported: false,
        userMaxAttachmentBytes: bad as number,
      });
      expect(r.limitBytes).toBe(FREE_ATTACHMENT_BYTES);
      expect(r.entitlementBytes).toBe(FREE_ATTACHMENT_BYTES);
    }
  );

  it('treats an unhydrated store (FREE_ENTITLEMENT) as the free floor with no hydration branch', () => {
    // subscriptionStore's FREE_ENTITLEMENT.maxAttachmentBytes IS 33_554_432 when
    // unhydrated, so the resolver needs no `hydrated` argument at all. If this
    // test ever needs a hydration flag passed in, the design has regressed (R1)
    // and re-introduced the #2172 premium-downgrade bug on a new axis.
    const r = resolveAttachmentLimit({
      chunkedUploadSupported: false,
      userMaxAttachmentBytes: 33_554_432,
    });
    expect(r.limitBytes).toBe(FREE_ATTACHMENT_BYTES);
  });

  it('honours a degraded-but-hydrated premium entitlement instead of clamping to free', () => {
    // #2172: a reconnect blip preserves last-known-good, so the store still
    // reports premium. The resolver must not second-guess it.
    const r = resolveAttachmentLimit({
      chunkedUploadSupported: false,
      userMaxAttachmentBytes: PREMIUM_ATTACHMENT_BYTES,
    });
    expect(r.entitlementBytes).toBe(PREMIUM_ATTACHMENT_BYTES);
    expect(r.limitBytes).toBeGreaterThan(FREE_ATTACHMENT_BYTES);
  });

  describe('#1556 server-axis seam (parameter exists, unused in PR 1)', () => {
    it('ignores an absent server value', () => {
      const r = resolveAttachmentLimit({
        chunkedUploadSupported: false,
        userMaxAttachmentBytes: FREE_ATTACHMENT_BYTES,
      });
      expect(r.entitlementBytes).toBe(FREE_ATTACHMENT_BYTES);
    });

    it('takes the larger axis when a server value is supplied', () => {
      const r = resolveAttachmentLimit({
        chunkedUploadSupported: false,
        userMaxAttachmentBytes: FREE_ATTACHMENT_BYTES,
        serverMaxUploadBytes: 100_000_000,
      });
      expect(r.entitlementBytes).toBe(100_000_000);
    });

    it('keeps the user axis when the server grant is smaller', () => {
      const r = resolveAttachmentLimit({
        chunkedUploadSupported: false,
        userMaxAttachmentBytes: FREE_ATTACHMENT_BYTES,
        serverMaxUploadBytes: 1_000_000,
      });
      expect(r.entitlementBytes).toBe(FREE_ATTACHMENT_BYTES);
    });

    it.each([0, Number.NaN])('treats %p as absent', (v) => {
      const r = resolveAttachmentLimit({
        chunkedUploadSupported: false,
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
        const r = resolveAttachmentLimit({
          chunkedUploadSupported: false,
          userMaxAttachmentBytes: FREE_ATTACHMENT_BYTES,
        });
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
        chunkedUploadSupported: false,
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
        chunkedUploadSupported: false,
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

describe('resolveAttachmentLimit — three capability states, not two', () => {
  // Reading `chunkedAttachmentUpload === true` collapsed three facts into one
  // `false`: the server said no, the server predates the field, and WE COULD NOT
  // ASK. All three clamp to the legacy ceiling -- correctly, fail-closed -- but
  // only one of them is a fact about the server, and the user-facing copy for
  // the other blamed the server's release version for a network blip.
  const premium = { userMaxAttachmentBytes: PREMIUM_ATTACHMENT_BYTES };

  it('reports a fetch failure as capability-unknown, not as a server limitation', () => {
    const limit = resolveAttachmentLimit({
      ...premium,
      chunkedUploadSupported: false,
      capabilityUnknown: true,
    });
    expect(limit.limitBytes).toBe(LEGACY_UPLOAD_PATH_CEILING_BYTES);
    expect(limit.source).toBe('capability-unknown');
    expect(limit.entitlementBytes).toBe(PREMIUM_ATTACHMENT_BYTES);
  });

  it('reports a server that answered NO as legacy-upload-path', () => {
    const limit = resolveAttachmentLimit({
      ...premium,
      chunkedUploadSupported: false,
      capabilityUnknown: false,
    });
    expect(limit.limitBytes).toBe(LEGACY_UPLOAD_PATH_CEILING_BYTES);
    expect(limit.source).toBe('legacy-upload-path');
  });

  it('does not clamp at all when the capability is supported', () => {
    const limit = resolveAttachmentLimit({
      ...premium,
      chunkedUploadSupported: true,
      capabilityUnknown: false,
    });
    expect(limit.limitBytes).toBe(PREMIUM_ATTACHMENT_BYTES);
    expect(limit.source).toBe('entitlement');
  });
});

describe('the desktop never offers an upload it cannot open', () => {
  // The server-wide Mach grant lifts a member to 512 MiB, but
  // MAX_DECRYPTABLE_ATTACHMENT_BYTES is derived from the 256 MiB premium
  // entitlement -- and that constant is where MEASUREMENT put the renderer's
  // memory ceiling, not a guess. So a 512 MiB attachment would upload and then
  // open for nobody, its own author included.
  //
  // The mismatch was latent while the legacy ceiling capped uploads at 128 MiB.
  // The chunked path is what makes 512 MiB reachable, which is why it is this
  // change's job to hold the invariant the constant already claims.
  const MACH3_SERVER_BYTES = 536_870_912; // 512 MiB

  it('clamps a 512 MiB server grant to what this build can decrypt', () => {
    const limit = resolveAttachmentLimit({
      tier: 'premium',
      userMaxAttachmentBytes: PREMIUM_ATTACHMENT_BYTES,
      serverMaxUploadBytes: MACH3_SERVER_BYTES,
      chunkedUploadSupported: true,
    });

    expect(limit.limitBytes).toBe(MAX_DECRYPTABLE_ATTACHMENT_BYTES);
    expect(limit.limitBytes).toBeLessThan(MACH3_SERVER_BYTES);
    // The REASON has to survive, not just the number: the notice tells the user
    // it is this build refusing, not their plan and not the server.
    expect(limit.source).toBe('decryptable-ceiling');
    // The entitlement is still reported honestly — the clamp does not rewrite
    // what the user is entitled to, only what this client will attempt.
    expect(limit.entitlementBytes).toBe(MACH3_SERVER_BYTES);
  });

  it('POSITIVE CONTROL: a 256 MiB grant is NOT clamped', () => {
    // Without this, a clamp that fired on everything would pass the test above
    // while capping every premium user below what they bought.
    const limit = resolveAttachmentLimit({
      tier: 'premium',
      userMaxAttachmentBytes: PREMIUM_ATTACHMENT_BYTES,
      serverMaxUploadBytes: PREMIUM_ATTACHMENT_BYTES,
      chunkedUploadSupported: true,
    });

    expect(limit.limitBytes).toBe(PREMIUM_ATTACHMENT_BYTES);
    expect(limit.source).toBe('entitlement');
  });

  it('the legacy ceiling still wins when it is lower', () => {
    // Two clamps now stack. The legacy one is far lower, so it must still be
    // the one reported -- naming the decryptable ceiling here would tell the
    // user to blame the wrong thing.
    const limit = resolveAttachmentLimit({
      tier: 'premium',
      userMaxAttachmentBytes: PREMIUM_ATTACHMENT_BYTES,
      serverMaxUploadBytes: MACH3_SERVER_BYTES,
      chunkedUploadSupported: false,
    });

    expect(limit.limitBytes).toBe(LEGACY_UPLOAD_PATH_CEILING_BYTES);
    expect(limit.source).toBe('legacy-upload-path');
  });
});
