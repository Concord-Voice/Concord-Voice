// client/desktop/src/renderer/components/Voice/MediaPolicyNotice.tsx
import React from 'react';
import { AlertTriangle } from 'lucide-react';
import { useVoiceStore } from '../../stores/voice/voiceStore';
import {
  MEDIA_POLICY_SOURCES,
  type MediaPolicySource,
} from '../../services/voice/mediaPolicyEvents';
import './MediaPolicyNotice.css';

/**
 * Fixed copy (#2153 handoff §1a, T4/T5/T19; spec A4). Each source names its own remedy
 * because only the mic needs a rejoin: toggling camera, share or share-sound re-produces.
 * "voice", never "this call", for the removal clause: the cooldown covers every room on
 * the node. Changing any of this needs a PR.
 */
export const MEDIA_POLICY_NOTICE_COPY: Readonly<Record<MediaPolicySource, string>> = {
  mic: 'Your microphone was paused because it sent more data than this call allows. Leave and rejoin to use it again. If it happens again, you may be removed from voice for a while.',
  camera:
    'Your camera was paused because it sent more data than this call allows. Turn your camera off and on to try again. If it happens again, you may be removed from voice for a while.',
  screen:
    'Your screen share was paused because it sent more data than this call allows. Stop and restart the share to try again. If it happens again, you may be removed from voice for a while.',
  'screen-audio':
    'Your shared sound was paused because it sent more data than this call allows. Turn sharing sound off and on to try again. If it happens again, you may be removed from voice for a while.',
};

/** Row id for aria-describedby. Unique because only one VoiceControls mounts at a time. */
export function mediaPolicyNoticeRowId(source: MediaPolicySource): string {
  return `media-policy-notice-${source}`;
}

/**
 * Owner strip. The <output> live region (implicit role="status") stays mounted while EMPTY so a
 * row inserted later is announced (the AttachmentNotice live-region pattern); empty, it has no
 * visual footprint. Rows are <span>s because <output> admits phrasing content only; the row CSS
 * sets display and margin, so they render as the <p>s did. Rows cannot be dismissed — a row
 * leaves only when its latch clears (T2).
 */
const MediaPolicyNotice: React.FC = () => {
  const paused = useVoiceStore((s) => s.mediaPolicyPaused);
  return (
    <output className="media-policy-notice">
      {MEDIA_POLICY_SOURCES.filter((source) => paused[source] !== undefined).map((source) => (
        <span key={source} id={mediaPolicyNoticeRowId(source)} className="media-policy-notice__row">
          <AlertTriangle size={16} aria-hidden="true" className="media-policy-notice__glyph" />
          <span>{MEDIA_POLICY_NOTICE_COPY[source]}</span>
        </span>
      ))}
    </output>
  );
};

export default MediaPolicyNotice;
