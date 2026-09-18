import { useState } from 'react';
import { useInviteStore } from '@/renderer/stores/chat/inviteStore';
import { useInvitePreview } from '@/renderer/hooks/messaging/useInvitePreview';
import { useIsServerMember } from '@/renderer/hooks/messaging/useIsServerMember';
import { resolveMediaUrl } from '@/renderer/utils/ui/resolveMediaUrl';
import './InviteEmbed.css';

/**
 * Renders a server-invite card from a code. Resolves authoritative server
 * metadata via the existing preview endpoint (the skeleton is NEUTRAL — never
 * sender-controlled text). Join reuses the existing authenticated join endpoint.
 */
export function InviteEmbed({ code }: Readonly<{ code: string }>) {
  const preview = useInvitePreview(code);
  // Both hooks run before any early return, so the id is derived here rather
  // than after the loading/invalid branches below.
  const previewServerId = preview.status === 'ready' ? preview.info.server_id : undefined;
  const alreadyMember = useIsServerMember(previewServerId);
  const joinServer = useInviteStore((s) => s.joinServer);
  const [joining, setJoining] = useState(false);
  const [joined, setJoined] = useState(false);
  const [joinError, setJoinError] = useState<string | null>(null);

  if (preview.status === 'loading') {
    return (
      <div className="invite-embed invite-embed--loading" aria-busy="true">
        Loading invite…
      </div>
    );
  }
  if (preview.status === 'invalid') {
    return (
      <div className="invite-embed invite-embed--invalid">
        This invite is invalid or has expired.
      </div>
    );
  }

  const { server_name, server_icon, member_count } = preview.info;
  const icon = resolveMediaUrl(server_icon);

  // `alreadyMember === true` only — an `undefined` (older control plane, no
  // `server_id`) means we cannot tell, and the safe reading of that is to offer
  // Join and let the server answer. See useIsServerMember.
  const showJoined = joined || alreadyMember === true;

  const onJoin = async () => {
    setJoining(true);
    setJoinError(null);
    const outcome = await joinServer(code);
    setJoining(false);
    if (outcome.status === 'joined') {
      setJoined(true);
      return;
    }
    // A different account owns the session now. The join did happen for the
    // ORIGINAL user, so there is nothing to tell whoever is sitting here.
    if (outcome.status === 'abandoned') return;
    // Surface the server's own reason rather than guessing at expiry. The join
    // route answers 409 "You are already a member of this server" for the
    // membership case, and this component used to overwrite that with an
    // expiry message — telling the user to find a fresh invite for a server
    // they are already in.
    //
    // Read from the OUTCOME, never from `inviteStore.error`. That field is one
    // shared value owned by the newest join, and a chat can hold several invite
    // cards, so an older card reading it back after its own await gets either
    // the newer join's message or the `null` that join's start wrote — landing
    // back on the generic guess this change exists to remove (Gitar, PR #3353).
    if (outcome.status === 'failed') {
      setJoinError(outcome.reason);
      return;
    }
    // Exhaustiveness sink — see JoinServerModal. Relying on `.reason` being
    // absent from a future member is a property of that member, not of this
    // call site.
    const unreachable: never = outcome;
    return unreachable;
  };

  return (
    <div className="invite-embed">
      {icon ? (
        <img className="invite-embed__icon" src={icon} alt="" />
      ) : (
        <div className="invite-embed__icon invite-embed__icon--placeholder" />
      )}
      <div className="invite-embed__body">
        <div className="invite-embed__label">Invite to a server</div>
        <div className="invite-embed__name">{server_name}</div>
        <div className="invite-embed__meta">{member_count} members</div>
      </div>
      {showJoined ? (
        <span className="invite-embed__joined">Joined</span>
      ) : (
        <button type="button" className="invite-embed__join" onClick={onJoin} disabled={joining}>
          {joining ? 'Joining…' : 'Join'}
        </button>
      )}
      {joinError ? <div className="invite-embed__error">{joinError}</div> : null}
    </div>
  );
}
