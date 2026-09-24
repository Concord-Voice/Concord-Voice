// client/desktop/src/renderer/components/Voice/MediaPolicyDialog.tsx
import React, { useId, useRef } from 'react';
import { AlertOctagon } from 'lucide-react';
import Modal from '../ui/Modal';
import { useVoiceStore } from '../../stores/voice/voiceStore';
import { formatRejoinTime, roundUpToMinute } from '../../services/voice/mediaPolicyEvents';
import './MediaPolicyDialog.css';

/** Wall-clock, rounded UP to the minute, never a countdown (handoff T9). */
const RejoinTime: React.FC<{ rejoinAt: number }> = ({ rejoinAt }) => (
  <time dateTime={new Date(roundUpToMinute(rejoinAt)).toISOString()}>
    {formatRejoinTime(rejoinAt)}
  </time>
);

/**
 * #2153 eviction / cooldown explanation. Mounted at APP level beside UpdateSecurityBanner
 * because the voice UI is already torn down when it matters (handoff §1c, T8). Copy says
 * "voice", not "this call": the cooldown blocks every room on the node (T11).
 */
const MediaPolicyDialog: React.FC = () => {
  const interrupt = useVoiceStore((s) => s.mediaPolicyInterrupt);
  const dismiss = useVoiceStore((s) => s.clearMediaPolicyInterrupt);
  const closeRef = useRef<HTMLButtonElement>(null);
  const bodyId = useId();

  if (!interrupt) return null;

  const evicted = interrupt.reason === 'evicted';
  const when = interrupt.rejoinAt === null ? null : <RejoinTime rejoinAt={interrupt.rejoinAt} />;

  let body: React.ReactNode;
  if (evicted) {
    body = (
      <>
        Your connection kept sending more data than this call allows, so you were removed.{' '}
        {when ? <>You can join voice again after {when}.</> : 'You can join voice again later.'}
      </>
    );
  } else {
    body = when ? <>Try again after {when}.</> : 'Try again later.';
  }

  return (
    <Modal
      isOpen
      onClose={dismiss}
      title={evicted ? 'Removed from voice' : 'You can’t join voice yet'}
      width="small"
      initialFocusRef={closeRef}
      describedById={bodyId}
    >
      <div className="media-policy-dialog">
        {/* --danger on the glyph ONLY, never label text (fails as text in 9/30 combos). */}
        <AlertOctagon size={20} aria-hidden="true" className="media-policy-dialog__glyph" />
        <p id={bodyId} className="media-policy-dialog__body">
          {body}
        </p>
      </div>
      <div className="media-policy-dialog__actions">
        <button
          ref={closeRef}
          type="button"
          className="media-policy-dialog__close"
          onClick={dismiss}
        >
          Close
        </button>
      </div>
    </Modal>
  );
};

export default MediaPolicyDialog;
