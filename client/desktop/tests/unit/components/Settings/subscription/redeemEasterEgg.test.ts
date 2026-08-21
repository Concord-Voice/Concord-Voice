import { describe, expect, it } from 'vitest';
import {
  EASTER_EGG_CODES,
  easterEggMessage,
} from '@/renderer/components/Settings/subscription/redeemEasterEgg';

const BOARD = 'AVYJANBUCLDSHKQVBYJWWHYDTLF';
const PICKUP = 'ZVUCDDSJCSFOAREL';

const BOARD_MESSAGE = "Not a code. Just something I haven't wiped off the board — yet.";
const PICKUP_MESSAGE = 'Pickup 2/19 1596 Paris Ave, ask for back lot';

describe('easterEggMessage (#2859)', () => {
  it('matches the board literal in its canonical form', () => {
    expect(easterEggMessage(BOARD)).toBe(BOARD_MESSAGE);
  });

  it('matches the pickup literal in its canonical form', () => {
    expect(easterEggMessage(PICKUP)).toBe(PICKUP_MESSAGE);
  });

  it.each([
    ['hyphen-grouped', 'AVYJA-NBUCL-DSHKQ-VBYJW-WHYDT-LF'],
    ['space-separated', 'AVYJA NBUCL DSHKQ VBYJW WHYDT LF'],
    ['lower-case', 'avyjanbucldshkqvbyjwwhydtlf'],
    ['lower-case hyphenated', 'avyja-nbucl-dshkq-vbyjw-whydt-lf'],
    ['surrounding whitespace', '  AVYJANBUCLDSHKQVBYJWWHYDTLF  '],
  ])('matches the board literal when %s', (_label, input) => {
    expect(easterEggMessage(input)).toBe(BOARD_MESSAGE);
  });

  it.each([
    ['hyphen-grouped', 'ZVUCD-DSJCS-FOARE-L'],
    ['lower-case', 'zvucddsjcsfoarel'],
  ])('matches the pickup literal when %s', (_label, input) => {
    expect(easterEggMessage(input)).toBe(PICKUP_MESSAGE);
  });

  it.each([
    ['a partial group', 'AVYJA'],
    ['a truncated literal', 'AVYJANBUCLDSHKQVBYJWWHYDT'],
    ['a one-character near-miss', 'AVYJANBUCLDSHKQVBYJWWHYDTLX'],
    ['the empty string', ''],
    ['whitespace only', '   '],
    ['an ordinary code', 'CV-ABCDE-FGHIJ'],
    // Deliberately unlike the server, which discards a leading prefix run before
    // validating: this matcher is exact, so a prefixed literal does not match.
    ['a prefixed board literal', 'CV-AVYJANBUCLDSHKQVBYJWWHYDTLF'],
    // Deliberately unlike the server, which folds I/L to 1 and O to 0. The server
    // would treat this as identical to the board literal; the matcher must not.
    ['the server-folded board literal', 'AVYJANBUC1DSHKQVBYJWWHYDT1F'],
  ])('returns null for %s', (_label, input) => {
    expect(easterEggMessage(input)).toBeNull();
  });

  // These pin the NORMALIZER, not the Map: '__proto__' loses its underscores and
  // every other key is upper-cased, so none can ever equal an Object.prototype key
  // by the time the lookup runs. Swapping the Map for a frozen Record leaves them
  // all green — do not read them as proof the Map is load-bearing.
  it.each(['__proto__', 'constructor', 'toString', 'valueOf', 'hasOwnProperty'])(
    'returns null for %s, which normalization can never turn into a prototype key',
    (key) => {
      expect(easterEggMessage(key)).toBeNull();
    }
  );

  // Moved here from RedeemCodeForm.test.tsx: this tests the module's data, and
  // asserting on EASTER_EGG_CODES rather than a local copy means an edit to the
  // literals cannot leave the invariant passing against a stale string.
  describe('collision invariant with issued redemption codes', () => {
    // Mirrors services/control-plane/internal/redemption/code.go normalizeSymbols:
    // upper-case, fold I/L to 1 and O to 0, strip separators.
    const fold = (v: string) =>
      v
        .toUpperCase()
        .replace(/[IL]/g, '1')
        .replace(/O/g, '0')
        .replace(/[^A-Z0-9]/g, '');
    const alphabet = new Set('0123456789ABCDEFGHJKMNPQRSTVWXYZ'); // pragma: allowlist secret
    const CANONICAL_LENGTH = 27; // payloadSymbols + 1

    it.each([...EASTER_EGG_CODES])('%s keeps a character the code alphabet rejects', (literal) => {
      const rejected = [...fold(literal)].filter((c) => !alphabet.has(c));
      expect(rejected.length).toBeGreaterThan(0);
    });

    it.each([...EASTER_EGG_CODES])(
      '%s is short enough that the server never trims its rejecting character away',
      (literal) => {
        // normalizeSymbols keeps only the TRAILING 27 symbols when longer. A future
        // literal over that length whose only rejecting character sat in the leading
        // run would be silently truncated to a valid shape, so bound the length too.
        expect(fold(literal).length).toBeLessThanOrEqual(CANONICAL_LENGTH);
      }
    );
  });
});
