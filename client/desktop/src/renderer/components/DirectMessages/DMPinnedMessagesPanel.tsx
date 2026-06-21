import React, { useEffect, useState, useCallback } from 'react';
import { X } from 'lucide-react';
import { getPins, unpinMessage } from '../../services/pinService';
import { PinContent, decryptPins, type DecryptedPin } from '../Chat/pinnedMessageUtils';
import '../Chat/PinnedMessagesPanel.css';

export interface DMPinnedMessagesPanelProps {
  conversationId: string;
  isOpen: boolean;
  onClose: () => void;
  onScrollToMessage: (messageId: string) => void;
  canPin?: boolean;
  onUnpin?: () => void;
}

const DMPinnedMessagesPanel: React.FC<DMPinnedMessagesPanelProps> = ({
  conversationId,
  isOpen,
  onClose,
  onScrollToMessage,
  canPin,
  onUnpin,
}) => {
  const [pins, setPins] = useState<DecryptedPin[]>([]);
  const [isLoading, setIsLoading] = useState(false);

  useEffect(() => {
    if (!isOpen) return;
    let cancelled = false;
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: shows loading state when panel opens or conversationId changes; not a render loop
    setIsLoading(true);
    getPins(conversationId)
      .then((rawPins) => decryptPins(conversationId, rawPins))
      .then((decrypted) => {
        if (!cancelled) setPins(decrypted);
      })
      .catch(() => {
        if (!cancelled) setPins([]);
      })
      .finally(() => {
        if (!cancelled) setIsLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [isOpen, conversationId]);

  const handleUnpin = useCallback(
    async (messageId: string) => {
      try {
        await unpinMessage(messageId);
        setPins((prev) => prev.filter((m) => m.id !== messageId));
        onUnpin?.();
      } catch {
        // Silently fail — WS event will correct state
      }
    },
    [onUnpin]
  );

  const handleJump = useCallback(
    (messageId: string) => {
      onScrollToMessage(messageId);
      onClose();
    },
    [onScrollToMessage, onClose]
  );

  if (!isOpen) return null;

  return (
    <div className="pinned-panel-backdrop">
      <button
        type="button"
        className="pinned-panel-backdrop-dismiss"
        onClick={onClose}
        aria-label="Close pinned messages"
      />
      <section className="pinned-panel">
        <div className="pinned-panel-header">
          <h3>Pinned Messages</h3>
          <button className="pinned-panel-close" onClick={onClose} aria-label="Close">
            <X size={18} />
          </button>
        </div>

        <div className="pinned-panel-content">
          {isLoading && <div className="pinned-panel-loading">Loading...</div>}

          {!isLoading && pins.length === 0 && (
            <div className="pinned-panel-empty">
              <p>No pinned messages in this conversation.</p>
              <p className="pinned-panel-empty-hint">
                Pin important messages so they&apos;re easy to find.
              </p>
            </div>
          )}

          {!isLoading &&
            pins.map((msg) => (
              <div key={msg.id} className="pinned-message-card">
                <div className="pinned-message-meta">
                  <span className="pinned-message-author">{msg.display_name || msg.username}</span>
                </div>
                <div className="pinned-message-content">
                  <PinContent message={msg} />
                </div>
                <div className="pinned-message-actions">
                  <button className="pinned-message-jump" onClick={() => handleJump(msg.id)}>
                    Jump
                  </button>
                  {canPin && (
                    <button className="pinned-message-unpin" onClick={() => handleUnpin(msg.id)}>
                      Unpin
                    </button>
                  )}
                </div>
              </div>
            ))}
        </div>
      </section>
    </div>
  );
};

export default DMPinnedMessagesPanel;
