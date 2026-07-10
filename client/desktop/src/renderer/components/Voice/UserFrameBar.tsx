import React, { useRef, useEffect, useMemo } from 'react';
import {
  useVoiceStore,
  type ActiveScreenShare,
  MAX_TUNED_SCREEN_SHARES,
} from '../../stores/voiceStore';
import { useUserStore } from '../../stores/userStore';
import { useVoiceMagnification } from './useVoiceMagnification';
import ParticipantTile from './ParticipantTile';
import ShareTunePill from './ShareTunePill';
import './UserFrameBar.css';

interface UserFrameBarProps {
  height: number;
}

/**
 * Horizontal strip of compact user frames (Mode B — top section).
 * Users with video on get left priority (sorted first).
 * Scrolls horizontally only when overflowing.
 */
const UserFrameBar: React.FC<UserFrameBarProps> = ({ height }) => {
  const participants = useVoiceStore((s) => s.participants);
  const localUserId = useUserStore((s) => s.user?.id);
  const activeScreenShares = useVoiceStore((s) => s.activeScreenShares);
  const tunedInScreenShares = useVoiceStore((s) => s.tunedInScreenShares);
  const scales = useVoiceMagnification(participants);
  const scrollRef = useRef<HTMLDivElement>(null);
  const scrollPosRef = useRef(0);

  // First announced remote share per producer user → Tune In/Out pill below
  // their frame (relocated from the retired ScreenShareControls dock).
  const shareByUser = useMemo(() => {
    const map: Record<string, ActiveScreenShare> = {};
    for (const share of Object.values(activeScreenShares)) {
      if (!share.isLocal && !(share.userId in map)) map[share.userId] = share;
    }
    return map;
  }, [activeScreenShares]);
  const atCap = Object.keys(tunedInScreenShares).length >= MAX_TUNED_SCREEN_SHARES;

  // Sort: video-on users first (left priority), then alphabetical
  const sortedParticipants = useMemo(() => {
    const list = Object.values(participants);
    return list.sort((a, b) => {
      if (a.isVideoOn && !b.isVideoOn) return -1;
      if (!a.isVideoOn && b.isVideoOn) return 1;
      const nameA = a.displayName || a.username;
      const nameB = b.displayName || b.username;
      return nameA.localeCompare(nameB);
    });
  }, [participants]);

  // Preserve scroll position when user list changes
  useEffect(() => {
    const el = scrollRef.current;
    if (el) {
      el.scrollLeft = scrollPosRef.current;
    }
  }, [sortedParticipants.length]);

  const handleScroll = () => {
    if (scrollRef.current) {
      scrollPosRef.current = scrollRef.current.scrollLeft;
    }
  };

  return (
    <div className="user-frame-bar" style={{ height }}>
      <div className="user-frame-bar__scroll" ref={scrollRef} onScroll={handleScroll}>
        {sortedParticipants.map((p) => {
          const share = shareByUser[p.userId];
          return (
            <div
              key={p.userId}
              className={`user-frame-bar__item${share ? ' user-frame-bar__item--pill' : ''}`}
            >
              <ParticipantTile
                participant={p}
                isLocal={p.userId === localUserId}
                compact
                magnificationScale={scales[p.userId]}
              />
              {share && (
                <ShareTunePill
                  share={share}
                  tunedIn={share.producerId in tunedInScreenShares}
                  atCap={atCap}
                  compact
                />
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
};

export default UserFrameBar;
