import React, { useRef, useEffect, useMemo } from 'react';
import { useVoiceStore } from '../../stores/voiceStore';
import { useUserStore } from '../../stores/userStore';
import { useVoiceMagnification } from './useVoiceMagnification';
import ParticipantTile from './ParticipantTile';
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
  const scales = useVoiceMagnification(participants);
  const scrollRef = useRef<HTMLDivElement>(null);
  const scrollPosRef = useRef(0);

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
        {sortedParticipants.map((p) => (
          <ParticipantTile
            key={p.userId}
            participant={p}
            isLocal={p.userId === localUserId}
            compact
            magnificationScale={scales[p.userId]}
          />
        ))}
      </div>
    </div>
  );
};

export default UserFrameBar;
