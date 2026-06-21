import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import Modal from '../ui/Modal';
import { useFriendStore, type Friend } from '../../stores/friendStore';
import { useSendInviteToFriend, type SendInviteResult } from '../../hooks/useSendInviteToFriend';
import { resolveMediaUrl } from '../../utils/resolveMediaUrl';
import './SendToFriendModal.css';

interface SendToFriendModalProps {
  serverId: string;
  serverName: string;
  open: boolean;
  onClose: () => void;
}

function friendLabel(f: Friend): string {
  return f.displayName || f.username;
}

function errorFor(reason: Extract<SendInviteResult, { ok: false }>['reason'], f: Friend): string {
  switch (reason) {
    case 'not_ready':
      return 'Still connecting — try again in a moment.';
    case 'dm_blocked':
      // openDM throws for the privacy gate (dm_disabled/privacy_blocked) AND for
      // generic failures (network/5xx). We can't reliably distinguish them here, so
      // the copy must be honest for both rather than always blaming the recipient.
      return `Couldn’t message ${friendLabel(f)} — they may not be accepting DMs, or there was a connection problem.`;
    case 'mint_failed':
      return 'Couldn’t create the invite — please try again.';
  }
}

export function SendToFriendModal({
  serverId,
  serverName,
  open,
  onClose,
}: Readonly<SendToFriendModalProps>) {
  const navigate = useNavigate();
  const friends = useFriendStore((s) => s.friends);
  const fetchFriends = useFriendStore((s) => s.fetchFriends);
  const { send } = useSendInviteToFriend(serverId);

  const [query, setQuery] = useState('');
  const [sendingId, setSendingId] = useState<string | null>(null);
  const [errorById, setErrorById] = useState<Record<string, string>>({});

  // Load friends on first open if not already loaded.
  useEffect(() => {
    if (open && friends.length === 0) {
      fetchFriends();
    }
  }, [open, friends.length, fetchFriends]);

  if (!open) return null;

  const q = query.trim().toLowerCase();
  const filtered = q
    ? friends.filter(
        (f) =>
          f.username.toLowerCase().includes(q) ||
          (f.displayName?.toLowerCase().includes(q) ?? false)
      )
    : friends;

  const handleSend = async (friend: Friend) => {
    // Re-entry guard: only one send in flight at a time. Without this (and the
    // all-rows `disabled` below), a fast second click — same row before re-render,
    // or a different row — would mint a second invite and fire a second navigate.
    if (sendingId !== null) return;
    setSendingId(friend.userId);
    setErrorById((prev) => {
      const next = { ...prev };
      delete next[friend.userId];
      return next;
    });
    const result = await send(friend);
    setSendingId(null);
    if (result.ok) {
      navigate('/app/dms');
      onClose();
    } else {
      setErrorById((prev) => ({ ...prev, [friend.userId]: errorFor(result.reason, friend) }));
    }
  };

  // Escape-to-close, overlay-click-to-close, title + close button are provided by <Modal>.
  return (
    <Modal isOpen={open} onClose={onClose} title={`Send a ${serverName} invite`} width="small">
      {friends.length === 0 ? (
        <p className="send-to-friend-modal__empty">
          No friends yet — add friends to send them server invites.
        </p>
      ) : (
        <>
          <input
            className="send-to-friend-modal__search"
            type="text"
            placeholder="Search friends…"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            aria-label="Search friends"
          />
          <ul className="send-to-friend-modal__list">
            {filtered.map((friend) => {
              const avatar = resolveMediaUrl(friend.avatarUrl);
              return (
                <li key={friend.userId} className="send-to-friend-modal__row">
                  <button
                    type="button"
                    className="send-to-friend-modal__friend"
                    onClick={() => handleSend(friend)}
                    disabled={sendingId !== null}
                    aria-busy={sendingId === friend.userId}
                  >
                    {avatar ? (
                      <img className="send-to-friend-modal__avatar" src={avatar} alt="" />
                    ) : (
                      <span
                        className="send-to-friend-modal__avatar send-to-friend-modal__avatar--placeholder"
                        aria-hidden="true"
                      />
                    )}
                    <span className="send-to-friend-modal__name">{friendLabel(friend)}</span>
                    {sendingId === friend.userId && (
                      <span className="send-to-friend-modal__status">Sending…</span>
                    )}
                  </button>
                  {errorById[friend.userId] && (
                    <p className="send-to-friend-modal__error" role="alert">
                      {errorById[friend.userId]}
                    </p>
                  )}
                </li>
              );
            })}
          </ul>
        </>
      )}
    </Modal>
  );
}
