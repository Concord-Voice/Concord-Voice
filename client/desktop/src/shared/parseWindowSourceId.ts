/**
 * ONE shape authority for Electron's `window:<handle>:<index>` source id.
 *
 * Lives in `shared/` — not in renderer policy — because the renderer decides what to
 * OFFER and main decides what to CAPTURE, and both must agree on what a window id is.
 * Main re-parses the string itself and never trusts a parse result the renderer sends;
 * sharing the function makes the two agree on the shape without making one trust the
 * other (ADR-0043 D4a; #3198 spec §5.1).
 *
 * THE CONTRACT IS "A HANDLE OR NOTHING". There is no best-effort arm. A malformed,
 * stale or hostile id must never silently widen a capture — "#2161's failure mode
 * wearing native clothes" (ADR-0043 D4a).
 *
 * CANONICAL FORM ONLY, AND IT DOES NOT MAKE THE HANDLE UNIQUE PER ID. `window:007:0` is
 * refused even though `007` parses as 7, so a given id has exactly ONE spelling and no two
 * spellings of the same id collide. That is the whole of the guarantee.
 *
 * It is NOT id-uniqueness of the RESULT, which an earlier draft of this docstring claimed
 * by naming "any caller that dedupes or caches by source id" — proven wrong by assertion in
 * #3198's Phase-8 adversarial pass. `window:5:0`, `window:5:1` and `window:5:42` are three
 * distinct CANONICAL ids that all return handle `5`, because the index is validated for
 * shape and then discarded. Discarding it is correct — Electron's `YY` is a same-process
 * disambiguator, not a second window identity — but it means **a caller that dedupes must
 * dedupe on the id string, never on the parse result.** This matters for #3198 PR 2, which
 * resolves a handle to a PID: a reader who believed the old sentence would think id
 * uniqueness implied handle uniqueness.
 *
 * WHAT THE HANDLE IS. `XX` in `window:XX:YY` is the `HWND` on Windows and the
 * `CGWindowID` on macOS. Both are 32-bit, and both platforms resolve one to an owning
 * PID with a single native call — which is #3198 PR 2's sixth `napi/` export, called in
 * the capture child. This function only produces the number; it never resolves it, and
 * no PID is ever handled here (I-PID).
 */

/**
 * `[1-9]\d*` for the handle: refuses `0` and any leading zero by SHAPE, before any
 * numeric conversion. The index is `0|[1-9]\d*` -- `0` is legitimate and Electron emits
 * it, but `00` and `000` are not. Both groups enforce canonical form, because the
 * docstring's dedupe argument above is about the WHOLE id and an index-only variation
 * produces exactly the same hazard one field to the right (found by #3198's pre-PR
 * adversarial pass, which noted the claim covered only the handle).
 * Linear, no backtracking — there is no catastrophic-input case in this pattern.
 */
const WINDOW_SOURCE_ID = /^window:([1-9]\d*):(0|[1-9]\d*)$/;

/**
 * Widest handle either platform produces: a Win32 `HWND` and a `CGWindowID` are both 32-bit.
 * Byte-grouped (`ff_ff_ff_ff`) rather than `ffff_ffff` — SonarCloud's `typescript:S7749`
 * enforces the two-digit hex grouping its upstream ESLint rule defaults to, even though the
 * rule's own prose blesses groups of four. Same value; cheaper to spell it the way the
 * analyser reads than to spend a false-positive register entry on a numeric separator.
 */
const HANDLE_MAX = 0xff_ff_ff_ff;

/**
 * Longest plausible id is `window:` + 10 digits + `:` + a few — well under this. The cap
 * exists so a hostile multi-megabyte string is refused by `length` rather than scanned.
 */
const MAX_SOURCE_ID_CHARS = 64;

export function parseWindowSourceId(sourceId: string | null | undefined): number | null {
  if (typeof sourceId !== 'string') return null;
  if (sourceId.length === 0 || sourceId.length > MAX_SOURCE_ID_CHARS) return null;

  const match = WINDOW_SOURCE_ID.exec(sourceId);
  if (match === null) return null;

  const handle = Number(match[1]);
  // The regex already guarantees digits with no leading zero, so this can only reject a
  // value too large for a u32.
  //
  // `isSafeInteger` IS BELT-AND-BRACES, NOT THE PRECISION GUARD AN EARLIER COMMENT HERE
  // CLAIMED. That claim was measured wrong twice over in #3198's Phase-8 review: rounding
  // past 2^53 only ever raises the value, and every double >= 2^53 already exceeds
  // `HANDLE_MAX` (2^32-1) by some twenty-one orders of magnitude — `Infinity` included. So
  // `handle > HANDLE_MAX` alone returns the identical answer on every input the regex
  // admits, and no input can reach this line where the two checks disagree. Kept because it
  // costs nothing and states the intended domain; the comment is corrected because a reader
  // would otherwise trust a mechanism that does not exist.
  if (!Number.isSafeInteger(handle) || handle > HANDLE_MAX) return null;

  return handle;
}
