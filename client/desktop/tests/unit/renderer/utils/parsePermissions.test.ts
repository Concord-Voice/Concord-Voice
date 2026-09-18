import { describe, it, expect } from 'vitest';
import { ADMINISTRATOR, INVITE, parsePermissions } from '@/renderer/utils/policy/permissions';

/**
 * `parsePermissions` had NO test anywhere in the repo before #2372, while being
 * the sole fail-closed decode for every permission bitfield the renderer reads —
 * `permissionStore`, `computeChannelPermissions`, `RoleEditorPanel`,
 * `OverridePanel`, and now `InviteServerPicker`.
 *
 * The precision case lives HERE rather than in a component test on purpose.
 * `hasPermission` short-circuits on the ADMINISTRATOR bit, and bit 62 is the
 * only permission above 2^53 — so a float64 round-trip of `ADMINISTRATOR|INVITE`
 * loses the INVITE bit yet still renders the row through the bypass. At the
 * component layer the regression is invisible; at this layer it is exact.
 */
describe('parsePermissions', () => {
  it('decodes a decimal string without precision loss above 2^53', () => {
    const high = ADMINISTRATOR | INVITE;
    const wire = high.toString();

    // The hazard, stated rather than assumed: a JS number cannot hold this
    // value, and the bit it drops is the one that decides invite capability.
    expect(BigInt(Number(wire))).not.toBe(high);
    expect(BigInt(Number(wire)) & INVITE).toBe(0n);

    // The real decode keeps every bit.
    expect(parsePermissions(wire)).toBe(high);
    expect(parsePermissions(wire) & INVITE).toBe(INVITE);
  });

  // The docblock always CLAIMED this function fails closed; `BigInt()` alone did
  // not deliver it. `'-1'` is the one that matters: it does not throw, and a
  // negative sign-extends under BigInt's two's-complement `&`, setting bit 62 —
  // which `hasPermission` treats as ADMINISTRATOR and short-circuits to true for
  // every permission. A refusal that grants everything is the worst shape a
  // fail-closed contract can take.
  it('fails closed on a negative, which would otherwise grant ADMINISTRATOR', () => {
    expect(parsePermissions('-1')).toBe(0n);
    // The control: proves the hazard is real rather than assumed. Sign-extension
    // genuinely sets the administrator bit on the unguarded decode.
    expect(BigInt('-1') & ADMINISTRATOR).toBe(ADMINISTRATOR);
    // And the sign bit ALONE grants nothing — bits 0..62 are clear — so the
    // granting shape is sign-bit plus real bits, not any negative.
    expect(-(2n ** 63n) & ADMINISTRATOR).toBe(0n);
  });

  it('fails closed on non-decimal encodings the wire format forbids', () => {
    expect(parsePermissions('0x40')).toBe(0n);
    expect(parsePermissions('0o100')).toBe(0n);
    expect(parsePermissions('0b1000000')).toBe(0n);
    expect(parsePermissions('+64')).toBe(0n);
    expect(parsePermissions(' 64 ')).toBe(0n);
  });

  it('fails closed on input it cannot decode', () => {
    expect(parsePermissions('not-a-number')).toBe(0n);
    expect(parsePermissions('12.5')).toBe(0n);
    expect(parsePermissions(undefined)).toBe(0n);
  });

  // `BigInt('')` is 0n rather than a throw, which is why an empty string had to
  // be handled by the CALLER as "not computed" rather than caught here.
  it('decodes an empty string to zero rather than throwing', () => {
    expect(parsePermissions('')).toBe(0n);
  });

  it('decodes ordinary values and a numeric input', () => {
    expect(parsePermissions('0')).toBe(0n);
    expect(parsePermissions(INVITE.toString())).toBe(INVITE);
    expect(parsePermissions(64)).toBe(64n);
  });
});
