import { useState } from 'react';
import { useInviteStore } from '@/renderer/stores/inviteStore';
import { useInvitePreview } from '@/renderer/hooks/useInvitePreview';
import { resolveMediaUrl } from '@/renderer/utils/resolveMediaUrl';
import './InviteEmbed.css';

/**
 * Renders a server-invite card from a code. Resolves authoritative server
 * metadata via the existing preview endpoint (the skeleton is NEUTRAL — never
 * sender-controlled text). Join reuses the existing authenticated join endpoint.
 */
export function InviteEmbed({ code }: Readonly<{ code: string }>) {
  const preview = useInvitePreview(code);
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

  const onJoin = async () => {
    setJoining(true);
    setJoinError(null);
    const res = await joinServer(code);
    setJoining(false);
    if (res) setJoined(true);
    else setJoinError('Could not join — the invite may have expired.');
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
      {joined ? (
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
