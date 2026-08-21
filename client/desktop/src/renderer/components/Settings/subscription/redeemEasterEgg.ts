// The intro video's blackboard (#2859) shows two Vigenère ciphertexts under a
// "BETA CODES" heading. Viewers who type them into the redeem form get a neutral
// in-app reply instead of the generic invalid-code error — and, crucially, no
// request is issued, so the /redeem rate-limit budget is not spent on traffic a
// marketing asset deliberately generates.
//
// Neither literal can collide with a real redemption code. Codes are Crockford
// Base32, and normalizeSymbols (services/control-plane/internal/redemption/code.go)
// FOLDS I/L to 1 and O to 0 before validating — so those three characters are legal,
// not rejecting. 'U' is the only one of the four ambiguous letters that is neither in
// the alphabet nor folded: crockfordValue returns -1 and the code is rejected pre-DB.
//
// Both literals contain a 'U', but the guarantee is not equally thin on both. The
// 27-symbol board literal is exactly payloadSymbols+1, so its 'U' is the whole
// barrier — drop it and only a 1-in-32 checksum remains. The 16-symbol pickup
// literal is additionally too short ever to equal an issued code, which is always
// 27 symbols. redeemEasterEgg.test.ts pins the invariant so editing a literal to
// drop its 'U' fails CI rather than failing silently.

/** The outcome of a redeem attempt. `error` renders assertively; the rest politely. */
export type RedeemResult =
  | { kind: 'success'; message: string }
  | { kind: 'error'; message: string }
  | { kind: 'notice'; message: string };

const BOARD_MESSAGE = "Not a code. Just something I haven't wiped off the board — yet.";
const PICKUP_MESSAGE = 'Pickup 2/19 1596 Paris Ave, ask for back lot';

// A Map, not a Record: `record[userInput]` is an object-injection shape that
// Semgrep and CodeQL flag on sight. It is safe in fact here — normalizeForMatch
// upper-cases, so no input can equal a lower/camel-case Object.prototype key — so
// this buys quiet scanners, not a closed hole. Map.get also types a miss honestly
// as `string | undefined`, where a Record index signature claims `string`.
const EASTER_EGGS = new Map<string, string>([
  ['AVYJANBUCLDSHKQVBYJWWHYDTLF', BOARD_MESSAGE],
  ['ZVUCDDSJCSFOAREL', PICKUP_MESSAGE],
]);

// Comparison-only canonicalization. This is deliberately NOT the server's
// normalizer (code.go normalizeSymbols) and must never be applied to the value
// sent to the API — canonicalization belongs where the hash is computed, and a
// second implementation that drifted would accept codes the server rejects.
// toUpperCase, not toLocaleUpperCase: under a Turkish locale the latter maps
// 'i' to 'İ' (dotted capital), which would not match. Neither literal contains
// an 'i' today, so this is defensive — the matcher must not depend on that.
function normalizeForMatch(raw: string): string {
  return raw.replace(/[^A-Za-z0-9]/g, '').toUpperCase();
}

/**
 * Returns the neutral message for one of the intro-video ciphertexts, or null
 * for every other input. Matching is forgiving about formatting (hyphens,
 * spaces, case) and strict about content — a partial group does not match.
 */
export function easterEggMessage(raw: string): string | null {
  return EASTER_EGGS.get(normalizeForMatch(raw)) ?? null;
}

/**
 * The normalized literals themselves, for the collision-invariant regression test.
 * Exported so that test asserts on the live keys rather than on its own copy —
 * a copy would keep passing after the module's literals were edited.
 */
export const EASTER_EGG_CODES: readonly string[] = [...EASTER_EGGS.keys()];
