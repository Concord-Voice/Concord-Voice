import React from 'react';
import { AlertTriangle } from 'lucide-react';
import { useVoiceStore } from '../../stores/voice/voiceStore';
import './VoiceJoinErrorNotice.css';

const VoiceJoinErrorNotice: React.FC = () => {
  const message = useVoiceStore((state) => state.joinError);
  const dismiss = useVoiceStore((state) => state.setJoinError);

  if (!message) return null;

  return (
    <div className="voice-join-error-notice" role="alert">
      <AlertTriangle size={16} aria-hidden="true" className="voice-join-error-notice__glyph" />
      <p className="voice-join-error-notice__message">{message}</p>
      <button
        className="voice-join-error-notice__dismiss"
        type="button"
        onClick={() => dismiss(null)}
      >
        Dismiss
      </button>
    </div>
  );
};

export default VoiceJoinErrorNotice;
