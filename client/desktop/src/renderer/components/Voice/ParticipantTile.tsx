import React, { useRef, useEffect, useMemo, useState, useId } from 'react';
import { resolveMediaUrl } from '../../utils/resolveMediaUrl';
import { MicOff, HeadphoneOff, Lock, Monitor, MoreVertical, Wrench } from 'lucide-react';
import { VoiceParticipantContextMenu } from './VoiceParticipantContextMenu';
import MemberProfileCard, { type ProfileCardMember } from '../Members/MemberProfileCard';
import { useVoiceStore, type VoiceParticipant } from '../../stores/voiceStore';
import { useMemberStore } from '../../stores/memberStore';
import { resolveUserAccentColors } from '../../utils/schemeColors';
import { useUserThemeScope } from '../../hooks/useUserThemeScope';
import './ParticipantTile.css';

interface ParticipantTileProps {
  participant: VoiceParticipant;
  isLocal?: boolean;
  compact?: boolean;
  magnificationScale?: number;
}

/**
 * Generate a consistent banner gradient from a userId.
 * Seeds two HSL hues 60deg apart from the char-code sum.
 */
function bannerGradient(userId: string): string {
  let hash = 0;
  for (let i = 0; i < userId.length; i++) {
    hash = Math.trunc((hash << 5) - hash + (userId.codePointAt(i) ?? 0));
  }
  const hue1 = (hash >>> 0) % 360;
  const hue2 = (hue1 + 60) % 360;
  return `linear-gradient(135deg, hsl(${hue1}, 60%, 35%), hsl(${hue2}, 60%, 25%))`;
}

function ParticipantStatusOverlay({
  participant,
  compact,
}: Readonly<{ participant: VoiceParticipant; compact: boolean }>) {
  const iconSize = compact ? 10 : 14;
  const lockSize = compact ? 6 : 8;

  if (participant.serverDeafened) {
    return (
      <div
        className="participant-tile__status participant-tile__status--server-deafened"
        title="Server Deafened"
      >
        <HeadphoneOff size={iconSize} />
        <Lock size={lockSize} className="participant-tile__lock-badge" />
      </div>
    );
  }
  if (participant.serverMuted) {
    return (
      <div
        className="participant-tile__status participant-tile__status--server-muted"
        title="Server Muted"
      >
        <MicOff size={iconSize} />
        <Lock size={lockSize} className="participant-tile__lock-badge" />
      </div>
    );
  }
  if (participant.isDeafened) {
    return (
      <div className="participant-tile__status participant-tile__status--deafened">
        <HeadphoneOff size={iconSize} />
      </div>
    );
  }
  if (participant.isMuted) {
    return (
      <div className="participant-tile__status participant-tile__status--muted">
        <MicOff size={iconSize} />
      </div>
    );
  }
  return null;
}

/**
 * The clickable display-name element shared by the video-overlay and the default
 * square frame. When non-local it is interactive: clicking (or pressing
 * Enter/Space) opens the member profile card. We keep a <span> rather than a
 * native <button> because the tile layout is visually sensitive and the name
 * sits inside flex/overlay chrome; role/tabIndex/onKeyDown supply the button
 * semantics a native element would otherwise provide (S6848/S1082).
 */
function InteractiveName({
  displayName,
  isLocal,
  className,
  onActivate,
}: Readonly<{
  displayName: string;
  isLocal: boolean;
  className: string;
  onActivate: (e: React.MouseEvent | React.KeyboardEvent) => void;
}>) {
  const interactiveClass = isLocal ? '' : ' voice-participant-name--interactive';
  return (
    <span
      className={`${className}${interactiveClass}`}
      role={isLocal ? undefined : 'button'}
      tabIndex={isLocal ? undefined : 0}
      onClick={isLocal ? undefined : onActivate}
      onKeyDown={
        isLocal
          ? undefined
          : (e) => {
              if (e.key === 'Enter' || e.key === ' ') {
                e.preventDefault();
                onActivate(e);
              }
            }
      }
    >
      {displayName}
      {isLocal && ' (You)'}
    </span>
  );
}

/**
 * The avatar block of the default (non-video) frame: either the participant's
 * avatar image or a single-letter fallback tinted with the user's accent color.
 * Extracted to keep the parent render's cognitive complexity low (S3776).
 */
function ParticipantAvatar({
  avatarUrl,
  displayName,
  userColors,
}: Readonly<{
  avatarUrl?: string;
  displayName: string;
  userColors: ReturnType<typeof resolveUserAccentColors>;
}>) {
  return (
    <div className="participant-tile__avatar">
      {resolveMediaUrl(avatarUrl) ? (
        <img
          src={resolveMediaUrl(avatarUrl)}
          alt={displayName}
          className="participant-tile__avatar-img"
        />
      ) : (
        <div
          className="participant-tile__avatar-fallback"
          style={userColors ? { background: userColors.gradient } : undefined}
        >
          {displayName.charAt(0).toUpperCase()}
        </div>
      )}
    </div>
  );
}

/**
 * Renders the participant's primary visual content — either the live video frame
 * with a name overlay, or the default banner+avatar+name square. Pulling the
 * two-branch frame out of the parent keeps ParticipantTile's cognitive
 * complexity under the S3776 threshold without changing behavior.
 */
function ParticipantTileContent({
  participant,
  isLocal,
  hasVideo,
  displayName,
  gradient,
  userColors,
  videoRef,
  onNameActivate,
}: Readonly<{
  participant: VoiceParticipant;
  isLocal: boolean;
  hasVideo: boolean;
  displayName: string;
  gradient: string;
  userColors: ReturnType<typeof resolveUserAccentColors>;
  videoRef: React.RefObject<HTMLVideoElement | null>;
  onNameActivate: (e: React.MouseEvent | React.KeyboardEvent) => void;
}>) {
  if (hasVideo) {
    // Video mode — fills the frame, name overlay at bottom.
    return (
      <>
        <video ref={videoRef} className="participant-tile__video" autoPlay playsInline muted />
        <div className="participant-tile__video-name">
          <InteractiveName
            displayName={displayName}
            isLocal={isLocal}
            className="participant-tile__video-name-text"
            onActivate={onNameActivate}
          />
        </div>
      </>
    );
  }
  // Default square frame — banner + avatar + name.
  return (
    <>
      <div className="participant-tile__banner" style={{ background: gradient }} />
      <div className="participant-tile__body">
        <ParticipantAvatar
          avatarUrl={participant.avatarUrl}
          displayName={displayName}
          userColors={userColors}
        />
        <InteractiveName
          displayName={displayName}
          isLocal={isLocal}
          className="participant-tile__name"
          onActivate={onNameActivate}
        />
      </div>
    </>
  );
}

const ParticipantTile: React.FC<ParticipantTileProps> = ({
  participant,
  isLocal = false,
  compact = false,
  magnificationScale = 1,
}) => {
  const videoRef = useRef<HTMLVideoElement>(null);
  const tileRef = useRef<HTMLDivElement>(null);

  // Attach video stream to video element
  useEffect(() => {
    const el = videoRef.current;
    if (el && participant.videoStream) {
      el.srcObject = participant.videoStream;
      el.play().catch(() => {});
    }
    return () => {
      // Copy ref value to a local so cleanup acts on the element from setup,
      // not whatever videoRef.current might point at when cleanup runs.
      if (el) {
        el.srcObject = null;
      }
    };
  }, [participant.videoStream]);

  const displayName = participant.displayName || participant.username;
  // Auto-pause only applies to screen shares (VoiceStage/StreamBar), not camera video
  const hasVideo = participant.isVideoOn && !!participant.videoStream;
  // Stable per-instance key: the same participant can render in several tiles at once
  // (grid + bar + PiP), so the voice service tracks visibility per tile, not per user.
  const tileId = useId();

  // #1541 visibility-pause: report this remote camera tile's render state to the
  // voice service, which pauses the SFU consumer (egress cut) when off-screen.
  // Declared after `hasVideo` so the deps array does not hit its temporal dead zone.
  useEffect(() => {
    if (isLocal || !hasVideo) return;
    const el = tileRef.current;
    if (!el) return;
    let disposed = false;
    let svc: {
      setRemoteVideoRenderState(
        userId: string,
        tileId: string,
        state: {
          visible: boolean;
          cssWidth: number;
          cssHeight: number;
          role: 'thumbnail' | 'grid' | 'focus';
          focusedWindow: boolean;
        }
      ): void;
      removeRemoteVideoTile(userId: string, tileId: string): void;
    } | null = null;
    const observer = new IntersectionObserver(
      ([entry]) => {
        if (disposed || !svc) return;
        const rect = el.getBoundingClientRect();
        svc.setRemoteVideoRenderState(participant.userId, tileId, {
          visible: entry.isIntersecting,
          cssWidth: rect.width,
          cssHeight: rect.height,
          role: compact ? 'thumbnail' : 'grid',
          focusedWindow: document.visibilityState !== 'hidden',
        });
      },
      { threshold: 0 }
    );
    void import('../../services/voiceService').then((m) => {
      if (disposed) return;
      const candidate = m.voiceService as Partial<NonNullable<typeof svc>>;
      if (
        typeof candidate.setRemoteVideoRenderState !== 'function' ||
        typeof candidate.removeRemoteVideoTile !== 'function'
      ) {
        return;
      }
      svc = {
        setRemoteVideoRenderState: candidate.setRemoteVideoRenderState.bind(candidate),
        removeRemoteVideoTile: candidate.removeRemoteVideoTile.bind(candidate),
      };
      observer.observe(el);
    });
    return () => {
      disposed = true;
      observer.disconnect();
      // Deregister this tile (NOT "report hidden") so a closing tile doesn't freeze
      // video still visible in another surface (grid / bar / PiP).
      svc?.removeRemoteVideoTile(participant.userId, tileId);
    };
  }, [compact, isLocal, hasVideo, participant.userId, tileId]);
  const memberColorScheme = useMemberStore((state) => {
    const member = state.members.find((m) => m.user_id === participant.userId);
    return member?.color_scheme;
  });
  const userColors = useMemo(() => resolveUserAccentColors(memberColorScheme), [memberColorScheme]);
  const { scopeProps } = useUserThemeScope(memberColorScheme);
  const gradient = useMemo(
    () => userColors?.gradient ?? bannerGradient(participant.userId),
    [userColors, participant.userId]
  );

  const scaleStyle =
    magnificationScale === 1 ? undefined : { transform: `scale(${magnificationScale})`, zIndex: 2 };

  // Voice channel/server context for the participant menu's move/disconnect
  // actions. Both come from the active call (voiceStore), not props.
  const activeServerId = useVoiceStore((s) => s.activeServerId);
  const activeChannelId = useVoiceStore((s) => s.activeChannelId);

  const [ctxMenu, setCtxMenu] = useState<{ x: number; y: number } | null>(null);
  const [profileCard, setProfileCard] = useState<{ x: number; y: number } | null>(null);

  const profileMember: ProfileCardMember = {
    user_id: participant.userId,
    username: participant.username,
    display_name: participant.displayName,
    avatar_url: participant.avatarUrl,
  };

  const handleNameClick = (e: React.MouseEvent | React.KeyboardEvent) => {
    e.stopPropagation();
    if (isLocal) return;
    // Mouse activation positions the card at the cursor; keyboard activation
    // (Enter/Space) has no clientX/Y, so anchor to the activated element.
    if ('clientX' in e) {
      setProfileCard({ x: e.clientX, y: e.clientY });
    } else {
      const rect = (e.currentTarget as HTMLElement).getBoundingClientRect();
      setProfileCard({ x: rect.left, y: rect.bottom });
    }
  };

  const handleContextMenu = (e: React.MouseEvent) => {
    // Always stop the browser default + propagation so right-clicks on a tile
    // don't bubble up to the channel/voice-channel context menu. Local users
    // don't get their own menu, but we still swallow the event.
    e.preventDefault();
    e.stopPropagation();
    if (isLocal) return;
    setCtxMenu({ x: e.clientX, y: e.clientY });
  };

  // Keyboard equivalent for the right-click menu: a native <button> trigger
  // rendered at the tile's corner. The button gets Enter/Space + focus for
  // free from native semantics — no role/tabIndex/onKeyDown on the outer div.
  const handleMenuButtonClick = (e: React.MouseEvent) => {
    e.stopPropagation();
    const rect = (e.currentTarget as HTMLElement).getBoundingClientRect();
    setCtxMenu({ x: rect.left, y: rect.bottom });
  };

  return (
    <div
      ref={tileRef}
      className={`participant-tile${hasVideo ? ' participant-tile--video' : ''}${compact ? ' participant-tile--compact' : ''}`}
      {...scopeProps}
      style={{ ...scopeProps.style, ...scaleStyle }}
      onContextMenu={handleContextMenu}
    >
      <ParticipantTileContent
        participant={participant}
        isLocal={isLocal}
        hasVideo={hasVideo}
        displayName={displayName}
        gradient={gradient}
        userColors={userColors}
        videoRef={videoRef}
        onNameActivate={handleNameClick}
      />

      {/* Status overlays */}
      <div className="participant-tile__overlays">
        <ParticipantStatusOverlay participant={participant} compact={compact} />
        {participant.isTesting && (
          <div
            className="participant-tile__status participant-tile__status--testing"
            title="Testing audio devices"
          >
            <Wrench size={compact ? 10 : 14} />
          </div>
        )}
        {participant.isScreenSharing && (
          <div className="participant-tile__status participant-tile__status--screen">
            <Monitor size={compact ? 10 : 14} />
          </div>
        )}
      </div>
      {!isLocal && (
        <button
          type="button"
          className="participant-tile__menu-trigger"
          onClick={handleMenuButtonClick}
          aria-haspopup="menu"
          aria-label={`Open menu for ${displayName}`}
        >
          <MoreVertical size={14} aria-hidden="true" />
        </button>
      )}
      {ctxMenu && activeServerId && activeChannelId && (
        <VoiceParticipantContextMenu
          participant={participant}
          serverId={activeServerId}
          channelId={activeChannelId}
          position={ctxMenu}
          showVolumeControl
          onClose={() => setCtxMenu(null)}
          onViewProfile={() => setProfileCard(ctxMenu)}
        />
      )}
      {profileCard && (
        <MemberProfileCard
          member={profileMember}
          position={profileCard}
          onClose={() => setProfileCard(null)}
        />
      )}
    </div>
  );
};

export default React.memo(ParticipantTile);
