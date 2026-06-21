// DM-related utility helpers (#1209 plan task F4).
//
// Intentionally type-only and store-agnostic: this util lives in utils/ to
// keep the dependency direction utils → stores forbidden by the project
// architecture rule. Consumers pass in the participants array (which they
// already have from useDMStore) and the helper operates structurally.

/**
 * PeerInfo is the minimal participant shape peerName needs. It's a
 * structural subtype of `DMParticipant` from stores/dmStore, so callers
 * that already have a `DMParticipant[]` can pass it directly without a
 * cast. Defined inline (rather than imported) to keep utils/ free of
 * cross-layer imports.
 */
export interface PeerInfo {
  userId: string;
  username: string;
  displayName?: string;
}

/**
 * peerName picks the "other" participant from a 1:1 DM conversation and
 * returns their displayName (preferred) or username (fallback). Returns
 * 'Unknown' if no peer is found (shouldn't happen for 1:1 conversations
 * but defensive).
 *
 * Used by OutgoingCallModal (#1209) for the "Calling Bob" display and by
 * VoiceView (channelName synthesis path) so DM voice calls show the peer
 * name where server channels would show channel.name.
 *
 * For group DMs the caller should use `conversation.name` directly instead
 * of this helper (which is 1:1-shaped).
 */
export function peerName(participants: PeerInfo[], currentUserId: string): string {
  const peer = participants.find((p) => p.userId !== currentUserId);
  return peer?.displayName ?? peer?.username ?? 'Unknown';
}
