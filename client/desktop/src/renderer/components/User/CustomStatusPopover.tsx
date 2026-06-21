import React, { useState, useRef, useEffect, useCallback } from 'react';
import { apiFetch } from '../../services/apiClient';
import { useRichPresenceStore } from '../../stores/richPresenceStore';
import EmojiPicker from '../EmojiPicker/EmojiPicker';
import './CustomStatusPopover.css';

const MAX_LEN = 140;

interface CustomStatusPopoverProps {
  /** Called to dismiss the popover (after save/clear or cancel). */
  onClose: () => void;
}

/**
 * Set / clear custom-text status popover (#1233 B5).
 *
 * A text input (max 140 chars) with a live remaining-char counter, an optional
 * emoji via the shared EmojiPicker, and Save / Clear actions. On Save it PATCHes
 * /users/me/presence-settings with { custom_text, custom_text_emoji } and mirrors
 * the result into useRichPresenceStore.self. An empty text + Save clears the
 * status (the server treats empty strings as a clear). Text is rendered as plain
 * text everywhere (React auto-escapes) — no dangerouslySetInnerHTML.
 */
const CustomStatusPopover: React.FC<CustomStatusPopoverProps> = ({ onClose }) => {
  const self = useRichPresenceStore((s) => s.self);
  const [text, setText] = useState(self.customText ?? '');
  const [emoji, setEmoji] = useState(self.customTextEmoji ?? '');
  const [showEmojiPicker, setShowEmojiPicker] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [emojiAnchor, setEmojiAnchor] = useState<{ x: number; y: number } | null>(null);

  const emojiBtnRef = useRef<HTMLButtonElement>(null);

  const remaining = MAX_LEN - text.length;
  const overLimit = text.length > MAX_LEN;
  const trimmed = text.trim();

  // PATCH the presence-settings endpoint and mirror the applied values into the
  // store. Empty text clears both fields (server interprets '' as a clear).
  const handleSave = useCallback(async () => {
    if (overLimit || saving) return;
    setSaving(true);
    setError(null);
    const isClear = trimmed.length === 0;
    const customText = isClear ? '' : text;
    const customTextEmoji = isClear ? '' : emoji;
    try {
      const res = await apiFetch('/api/v1/users/me/presence-settings', {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ custom_text: customText, custom_text_emoji: customTextEmoji }),
      });
      if (!res.ok) {
        const data = await res.json().catch(() => ({}));
        throw new Error(data.error || 'Failed to update status');
      }
      useRichPresenceStore.getState().setSelfPresence({
        customText: isClear ? undefined : customText,
        customTextEmoji: isClear || !customTextEmoji ? undefined : customTextEmoji,
      });
      onClose();
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to update status');
    } finally {
      setSaving(false);
    }
  }, [overLimit, saving, trimmed.length, text, emoji, onClose]);

  // Clear: send empty strings and wipe the store's self custom text.
  const handleClear = useCallback(async () => {
    if (saving) return;
    setSaving(true);
    setError(null);
    try {
      const res = await apiFetch('/api/v1/users/me/presence-settings', {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ custom_text: '', custom_text_emoji: '' }),
      });
      if (!res.ok) {
        const data = await res.json().catch(() => ({}));
        throw new Error(data.error || 'Failed to clear status');
      }
      useRichPresenceStore
        .getState()
        .setSelfPresence({ customText: undefined, customTextEmoji: undefined });
      onClose();
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to clear status');
    } finally {
      setSaving(false);
    }
  }, [saving, onClose]);

  // Close on Escape (only when the emoji picker isn't capturing it).
  useEffect(() => {
    const handleEscape = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !showEmojiPicker) onClose();
    };
    document.addEventListener('keydown', handleEscape);
    return () => document.removeEventListener('keydown', handleEscape);
  }, [onClose, showEmojiPicker]);

  const openEmojiPicker = () => {
    const rect = emojiBtnRef.current?.getBoundingClientRect();
    if (rect) setEmojiAnchor({ x: rect.left, y: rect.bottom + 4 });
    setShowEmojiPicker(true);
  };

  return (
    // Native <dialog> (implicit role="dialog") rendered as a NON-modal anchored
    // popover via the `open` attribute — NOT showModal() (no ::backdrop, no focus
    // trap, no top-layer). Keeps the existing absolute positioning; S6819 prefers
    // the native element over a role attribute on a <div>.
    <dialog className="custom-status-popover" open aria-label="Set custom status">
      <div className="custom-status-popover-title">Set a custom status</div>

      <div className="custom-status-popover-row">
        <button
          type="button"
          ref={emojiBtnRef}
          className="custom-status-emoji-btn"
          onClick={openEmojiPicker}
          aria-label="Choose emoji"
          title="Choose emoji"
        >
          {emoji || <span className="custom-status-emoji-placeholder">🙂</span>}
        </button>

        <input
          type="text"
          className="custom-status-input"
          value={text}
          maxLength={MAX_LEN}
          placeholder="What's happening?"
          aria-label="Custom status text"
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') handleSave();
          }}
          autoFocus
        />
      </div>

      <div className="custom-status-popover-meta">
        {emoji && (
          <button type="button" className="custom-status-emoji-remove" onClick={() => setEmoji('')}>
            Remove emoji
          </button>
        )}
        <span className={`custom-status-counter ${overLimit ? 'over-limit' : ''}`}>
          {remaining}
        </span>
      </div>

      {error && <div className="custom-status-error">{error}</div>}

      <div className="custom-status-popover-actions">
        <button
          type="button"
          className="custom-status-clear-btn"
          onClick={handleClear}
          disabled={saving}
        >
          Clear
        </button>
        <button
          type="button"
          className="custom-status-save-btn"
          onClick={handleSave}
          disabled={saving || overLimit}
        >
          {saving ? 'Saving...' : 'Save'}
        </button>
      </div>

      {showEmojiPicker && (
        <EmojiPicker
          mode="popover"
          position={emojiAnchor ?? { x: 0, y: 0 }}
          onSelect={(selected) => {
            setEmoji(selected);
            setShowEmojiPicker(false);
          }}
          onClose={() => setShowEmojiPicker(false)}
        />
      )}
    </dialog>
  );
};

export default CustomStatusPopover;
