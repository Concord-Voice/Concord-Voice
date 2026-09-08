/**
 * Identity-authority telemetry classification (#3136, corrected in PR #3157).
 *
 * A leaf module on purpose. `index.ts` is the server entrypoint, is excluded
 * from coverage in `vitest.config.ts`, and cannot be imported by a test without
 * standing the server up — so a decision left inline there is untestable by
 * construction. That is how the defect below shipped unnoticed.
 */
import type { ParticipantIdentity } from '../middleware/auth.js';

export type IdentityAuthorityReasonCode =
  | 'identity_authority_missing'
  | 'identity_authority_mismatch';

/**
 * True when the client actively supplied a value that conflicts with the
 * server-authoritative one.
 *
 * **Absence is not an assertion**, and that distinction is the whole fix. Under
 * CV-CAN-017 `resolveParticipantIdentity` coerces an absent server username to
 * `''` and leaves `displayName` / `avatarUrl` as `undefined` — it never re-opens
 * a field to the spoofable handshake value. A client that simply did not send
 * `displayName` therefore differs from the server on every ordinary join.
 */
function clientAsserted(
  supplied: string | undefined,
  authoritative: string | undefined
): boolean {
  if (supplied === undefined || supplied === '') return false;
  return supplied !== authoritative;
}

/**
 * Why a join's identity handling deserves a security event, or `undefined` when
 * it does not.
 *
 * `identity_authority_missing` means the control-plane returned no authoritative
 * identity at all, so the handshake values were used — the fail-open case worth
 * knowing about.
 *
 * `identity_authority_mismatch` means the client asserted a display identity the
 * server overrode. **It fires only on fields the client actually sent.** Diffing
 * all three fields unconditionally — the shape this replaces — reported a
 * `denied` / `medium` event on most ordinary joins, because a client that omits
 * `displayName` and `avatarUrl` differs from a server that has them. That is a
 * false-positive flood aimed at a brand-new telemetry pipeline, which is worse
 * than no signal: it trains the reader to ignore the channel.
 *
 * **What this deliberately does NOT separate:** a stale cached value and a
 * spoof attempt are indistinguishable from here — both arrive as a non-empty
 * field that disagrees with the server. Both still fire. The claim narrowed to
 * one the code can actually support: *the client asserted an identity and the
 * server did not honour it.* Whether that was malice is not decided here.
 */
export function getIdentityAuthorityReasonCode(
  authIdentityPresent: boolean | undefined,
  identity: ParticipantIdentity,
  handshake: ParticipantIdentity
): IdentityAuthorityReasonCode | undefined {
  if (!authIdentityPresent) return 'identity_authority_missing';
  if (
    clientAsserted(handshake.username, identity.username) ||
    clientAsserted(handshake.displayName, identity.displayName) ||
    clientAsserted(handshake.avatarUrl, identity.avatarUrl)
  ) {
    return 'identity_authority_mismatch';
  }
  return undefined;
}
