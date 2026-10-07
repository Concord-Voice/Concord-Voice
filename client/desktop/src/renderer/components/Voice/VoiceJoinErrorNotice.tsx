import React, { useEffect } from 'react';
import { AlertTriangle } from 'lucide-react';
import { useTopLayerDialog } from '../../hooks/ui/useTopLayerDialog';
import { useVoiceStore } from '../../stores/voice/voiceStore';
import './VoiceJoinErrorNotice.css';

const VoiceJoinErrorNotice: React.FC = () => {
  const message = useVoiceStore((state) => state.joinError);
  const dismiss = useVoiceStore((state) => state.setJoinError);
  const { dialogRef, titleId, descriptionId } = useTopLayerDialog(!!message);

  useEffect(() => {
    const dialog = dialogRef.current;
    if (!message || !dialog) return;

    const handleClose = () => {
      // A queued close from an earlier opening must not dismiss a reopened dialog.
      if (!dialog.open) dismiss(null);
    };
    dialog.addEventListener('close', handleClose);
    return () => {
      dialog.removeEventListener('close', handleClose);
    };
  }, [dialogRef, dismiss, message]);

  if (!message) return null;

  return (
    <dialog
      ref={dialogRef}
      className="voice-join-error-notice"
      aria-labelledby={titleId}
      aria-describedby={descriptionId}
      onKeyDown={(event) => {
        if (event.key !== 'Escape') return;
        // Async dialogs can share a close-watcher group; native Escape closes both.
        event.preventDefault();
        event.stopPropagation();
        dismiss(null);
      }}
    >
      <div className="voice-join-error-notice__heading">
        <AlertTriangle size={20} aria-hidden="true" className="voice-join-error-notice__glyph" />
        <h2 id={titleId}>Unable to join voice</h2>
      </div>
      <p id={descriptionId} className="voice-join-error-notice__message">
        {message}
      </p>
      <button
        className="voice-join-error-notice__dismiss"
        type="button"
        onClick={() => dismiss(null)}
      >
        Dismiss
      </button>
    </dialog>
  );
};

export default VoiceJoinErrorNotice;
