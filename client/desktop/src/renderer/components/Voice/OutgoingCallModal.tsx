// OutgoingCallModal — caller-side centered modal showing outgoing-ring
// state with peer name + Cancel button (#1209 plan task F2). Per spec
// §7.8: deliberately asymmetric with IncomingCallBanner — caller initiated
// the action and wants to see their call's progress; the centered modal
// gives that visual weight.
//
// DM 1:1 shows "Calling <peer> / Ringing…". Group (#1219 R3) shows the
// group name + a per-decliner tally ("Ringing Charlie, Diana — Bob
// declined"). Decliner names are resolved LOCALLY from dmStore
// participants — the dm_voice_call_declined WS payload carries only
// decliner_user_id (no PII), so the tally never depends on wire-side names.

import { useVoiceStore } from '../../stores/voiceStore';
import { useDMStore, type DMParticipant } from '../../stores/dmStore';
import { useUserStore } from '../../stores/userStore';
import { cancelOutgoingCall } from '../../services/voiceService/callStateMachine';
import { peerName } from '../../utils/dm';
import './OutgoingCallModal.css';

/** Resolve a user ID to a display name via the conversation's participants. */
function nameOf(participants: DMParticipant[], userId: string): string {
  const p = participants.find((x) => x.userId === userId);
  return p?.displayName ?? p?.username ?? 'Unknown';
}

export function OutgoingCallModal() {
  const callState = useVoiceStore((s) => s.callState);
  const myUserId = useUserStore((s) => s.user?.id);

  // Subscribe to conversations array so the modal re-renders if the peer's
  // displayName updates mid-ring (rare but possible if a presence event
  // arrives during the outgoing ring window).
  const conversations = useDMStore((s) => s.conversations);

  if (callState.kind !== 'outgoing-ringing') {
    return null;
  }

  const conversation = conversations.find((c) => c.id === callState.conversationId);
  if (!conversation) {
    // Shouldn't happen — initiateDMCall validates this. Defensive null
    // render to avoid crash if the store evicted the conv mid-ring.
    return null;
  }

  // Group branch (#1219 R3): show the group name + ringing/declined tally.
  if (conversation.isGroup) {
    const title = conversation.name ?? 'Group voice call';
    const declined = new Set(callState.declinedUserIds);
    const stillRinging = callState.calleeUserIds.filter((id) => !declined.has(id));
    const ringingNames = stillRinging.map((id) => nameOf(conversation.participants, id));
    const declinedNames = callState.declinedUserIds.map((id) =>
      nameOf(conversation.participants, id)
    );

    return (
      <dialog className="outgoing-call-modal__backdrop" open aria-modal="true">
        <div className="outgoing-call-modal">
          <div className="outgoing-call-modal__avatar">
            <span className="outgoing-call-modal__initials" aria-hidden="true">
              {title.slice(0, 2).toUpperCase()}
            </span>
          </div>
          <div className="outgoing-call-modal__text">
            <div className="outgoing-call-modal__name">{title}</div>
            <div className="outgoing-call-modal__subtitle">
              {ringingNames.length > 0 ? (
                <span className="outgoing-call-modal__ringing">
                  Ringing {ringingNames.join(', ')}
                </span>
              ) : (
                <span className="outgoing-call-modal__ringing">Ringing…</span>
              )}
              {declinedNames.length > 0 && (
                <>
                  {' — '}
                  {declinedNames.map((name, i) => (
                    <span key={callState.declinedUserIds[i]}>
                      {i > 0 && ', '}
                      <span className="outgoing-call-modal__declinee">{name}</span>
                    </span>
                  ))}
                  {' declined'}
                </>
              )}
            </div>
          </div>
          <button
            type="button"
            className="outgoing-call-modal__cancel"
            aria-label="Cancel call"
            onClick={() => {
              void cancelOutgoingCall().catch((err: unknown) => {
                console.error(
                  'Cancel-call button handler:',
                  err instanceof Error ? err.message : 'non-Error thrown'
                );
              });
            }}
          >
            Cancel
          </button>
        </div>
      </dialog>
    );
  }

  // DM 1:1: pick the OTHER participant (peer).
  const displayName = myUserId ? peerName(conversation.participants, myUserId) : 'Unknown';

  return (
    <dialog className="outgoing-call-modal__backdrop" open aria-modal="true">
      <div className="outgoing-call-modal">
        <div className="outgoing-call-modal__avatar">
          <span className="outgoing-call-modal__initials" aria-hidden="true">
            {displayName.slice(0, 2).toUpperCase()}
          </span>
        </div>
        <div className="outgoing-call-modal__text">
          <div className="outgoing-call-modal__name">Calling {displayName}</div>
          <div className="outgoing-call-modal__subtitle">Ringing…</div>
        </div>
        <button
          type="button"
          className="outgoing-call-modal__cancel"
          aria-label="Cancel call"
          onClick={() => {
            void cancelOutgoingCall().catch((err: unknown) => {
              // cancelOutgoingCall swallows POST errors internally; .catch
              // here just prevents the unhandled-rejection warning at the
              // click boundary (Copilot #1231 finding C11).
              console.error(
                'Cancel-call button handler:',
                err instanceof Error ? err.message : 'non-Error thrown'
              );
            });
          }}
        >
          Cancel
        </button>
      </div>
    </dialog>
  );
}
