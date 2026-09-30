import { describe, it, expect } from 'vitest';
import {
  ADMINISTRATOR,
  INVITE,
  MANAGE_CHANNELS,
  countBits,
  hasPermission,
  parseEffectivePermissions,
  parsePermissions,
} from '@/renderer/utils/policy/permissions';

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

// A JSON number above 2^53 has already lost its low bits; decoding it would
// yield a different, still plausible bitfield, so it fails closed (#3406
// review). The one exception, an administrator's value from an older server,
// is pinned in its own block below.
describe('parsePermissions — unsafe integers fail closed (#3406)', () => {
  it('refuses a number above Number.MAX_SAFE_INTEGER that no bitfield can produce', () => {
    expect(parsePermissions(Number.MAX_SAFE_INTEGER + 2)).toBe(0n);
  });
  it('still decodes a safe integer', () => {
    expect(parsePermissions(Number.MAX_SAFE_INTEGER)).toBe(BigInt(Number.MAX_SAFE_INTEGER));
  });
});

// Red-team finding on #3406: the strict-decimal check ran only for strings and
// numbers, so any other runtime shape reached BigInt(), which stringifies an
// array first. ['-1'] became -1n, and hasPermission(-1n, …) short-circuits on
// the sign-extended bit 62. No wire field can carry such a value today; the
// decode must still fail closed on it.
describe('parsePermissions — non-string, non-number input fails closed (#3406)', () => {
  it.each<[string, unknown]>([
    ['an array holding a negative', ['-1']],
    ['an array holding a decimal', ['1024']],
    ['true', true],
    ['an object', { toString: () => '-1' }],
  ])('%s decodes to 0n', (_label, input) => {
    expect(parsePermissions(input as string)).toBe(0n);
  });
});

/**
 * V9 (#3406 regression): before #3406 `countBits` decoded with raw
 * `BigInt(n || 0)` rather than routing through `parsePermissions`'s fail-closed
 * decode, so it accepted the exact non-decimal and non-integer shapes
 * `parsePermissions` above refuses. `OverridePanel`'s override-summary counts
 * (`{allowCount} allowed`) are the only caller, so a malformed wire value
 * renders a wrong count instead of failing closed.
 *
 * Before the fix these were red for two different reasons:
 *   - `countBits('0x40')` returned 1: `BigInt('0x40')` accepts hex, and 64
 *     has one set bit.
 *   - `countBits(1.5)` threw (`BigInt(1.5)` is not an integer) instead of
 *     returning 0.
 */
describe('countBits — V9 (#3406 regression)', () => {
  it('fails closed on a hex string the wire format never emits', () => {
    expect(countBits('0x40')).toBe(0);
  });

  it('fails closed on a negative decimal string', () => {
    expect(countBits('-1')).toBe(0);
  });

  it('fails closed on a non-integer number rather than throwing', () => {
    expect(countBits(1.5)).toBe(0);
  });

  it('counts an ordinary decimal string', () => {
    expect(countBits('1024')).toBe(1);
  });

  it('counts an ordinary bigint', () => {
    expect(countBits(5n)).toBe(2);
  });
});

// Codex's third round on #3406: a pre-#3406 control plane sends effective
// permissions as JSON numbers. A number in [2^62, 2^63] carries bit 62, and
// float rounding has scrambled its low bits. Refusing it outright took an
// administrator's controls away on an older server; the Administrator grant is
// the one fact it still states, so that is what it decodes to. A number above
// 2^53 without bit 62 can exist (an owner may store undefined high bits through
// UpdateRole) but states no grant, so it still fails closed.
//
// Codex's seventh round moved that decode out of parsePermissions: it is sound
// only for EFFECTIVE permissions, where the Administrator bit subsumes every
// other, and parsePermissions also decodes override and role masks. So these
// values decode to the grant through parseEffectivePermissions, and every one
// of them is refused by parsePermissions.
describe('parseEffectivePermissions — a pre-#3406 server numeric Administrator value (#3406)', () => {
  it.each<[string, number]>([
    ['2^62 with low bits the float kept', 2 ** 62 + 2 ** 20],
    ['2^62 with every low permission bit, as rounded', 2 ** 62 + 2 ** 30 - 1],
    // Codex, round five: every int64 at or above 2^63 - 512 rounds to exactly
    // 2^63 as a float, and every one of them carries bit 62. An owner can store
    // such a value through UpdateRole, so an administrator's effective
    // permissions can arrive as exactly 2^63.
    ['2^63, the float every int64 at or above 2^63 - 512 rounds to', 2 ** 63],
    ['int64 max as JSON.parse reads it', JSON.parse('9223372036854775807') as number],
  ])('decodes %s to the Administrator grant alone', (_label, value) => {
    expect(parseEffectivePermissions(value)).toBe(ADMINISTRATOR);
    expect(parsePermissions(value)).toBe(0n);
  });

  it('keeps every permission check open for that administrator', () => {
    expect(hasPermission(parseEffectivePermissions(2 ** 62 + 2 ** 20), MANAGE_CHANNELS)).toBe(true);
  });

  it.each<[string, number]>([
    ['a value between 2^53 and 2^62, which carries no Administrator bit', 2 ** 55],
    // Codex, round six: every value from 2^62 - 256 up rounds to exactly 2^62,
    // so 2^62 itself can be a role with bits 8 through 61 all set and no bit 62.
    ['2^62 exactly', 2 ** 62],
    ['the next float above 2^63, which no int64 rounds to', 2 ** 63 + 2 ** 11],
    ['2^64, beyond int64', 2 ** 64],
    ['a negative Administrator-sized value', -(2 ** 62)],
  ])('still refuses %s', (_label, value) => {
    expect(parseEffectivePermissions(value)).toBe(0n);
    expect(parsePermissions(value)).toBe(0n);
  });
});

// Codex's sixth round on #3406: the lower edge of that range is ambiguous.
// Every int64 from 2^62 - 256 to 2^62 - 1 rounds UP to exactly 2^62, and none
// of them carries bit 62: they are a role with bits 8 through 61 all set, which
// an owner can write through UpdateRole. A real administrator never rounds to
// exactly 2^62, because every member holds the managed @all role, whose
// permissions cannot be changed and whose view and send bits alone keep an
// administrator's value at least 2^62 + 1024. So only a number strictly above
// 2^62 states the Administrator grant.
describe('parseEffectivePermissions — the rounded lower edge of the Administrator range (#3406)', () => {
  it.each<[string, number]>([
    ['2^62 - 256, bits 8 through 61, as JSON.parse reads it', JSON.parse('4611686018427387648')],
    ['2^62 - 1, as JSON.parse reads it', JSON.parse('4611686018427387903')],
  ])('refuses %s, which carries no Administrator bit', (_label, value) => {
    expect(value).toBe(2 ** 62);
    expect(parseEffectivePermissions(value)).toBe(0n);
  });

  it('reads the next float above 2^62 as the Administrator grant', () => {
    // 2^62 + 1024, which only a value carrying bit 62 can round to. A bound
    // tightened above it would drop administrators the float still identifies.
    const value = JSON.parse('4611686018427388928') as number;
    expect(value).toBe(2 ** 62 + 1024);
    expect(parseEffectivePermissions(value)).toBe(ADMINISTRATOR);
    // A mask of that exact value is 2^62 + 1024 or a rounded neighbour, never
    // the Administrator bit alone, so parsePermissions refuses it.
    expect(parsePermissions(value)).toBe(0n);
  });

  it('still reads an administrator holding the @all role as the Administrator grant', () => {
    // 2^62 | BasePermissions (484687360), the smallest value an administrator
    // who holds @all and nothing else can have.
    const value = JSON.parse('4611686018912075264') as number;
    expect(value).toBeGreaterThan(2 ** 62);
    expect(parseEffectivePermissions(value)).toBe(ADMINISTRATOR);
  });
});
