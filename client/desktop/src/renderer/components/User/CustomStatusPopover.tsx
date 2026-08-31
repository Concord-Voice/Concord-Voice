import React, { useState, useRef, useEffect, useLayoutEffect, useCallback } from 'react';
import { apiFetch } from '../../services/system/apiClient';
import {
  CUSTOM_STATUS_RECONCILIATION_ERROR,
  isDefinitePresenceSettingsPatchRejection,
  parsePresenceSettingsResponse,
  presenceSettingsResponseError,
  type CustomStatusSubmissionResult,
  type PresenceSettings,
  useRichPresenceStore,
} from '../../stores/ui/richPresenceStore';
import EmojiPicker from '../EmojiPicker/EmojiPicker';
import './CustomStatusPopover.css';

const MAX_LEN = 140;
// Native maxLength counts UTF-16 units; preserve one all-astral over-limit value.
const MAX_INPUT_CODE_UNITS = (MAX_LEN + 1) * 2;

const updatePopoverStateIfMounted = (
  mountedRef: React.RefObject<boolean>,
  update: () => void
): void => {
  if (mountedRef.current) update();
};

const finishAmbiguousReconciliation = (
  result: CustomStatusSubmissionResult,
  confirmedSettings: PresenceSettings | null,
  customText: string,
  customTextEmoji: string,
  mountedRef: React.RefObject<boolean>,
  onClose: () => void,
  setError: (error: string | null) => void
): void => {
  if (!mountedRef.current) return;
  if (
    result.contextCurrent &&
    result.activityCurrent &&
    result.customCurrent &&
    confirmedSettings !== null &&
    (confirmedSettings.customText ?? '') === customText &&
    (confirmedSettings.customTextEmoji ?? '') === customTextEmoji
  ) {
    setError(null);
    onClose();
  }
};

interface CustomStatusPopoverProps {
  /** Called to dismiss the popover (after save/clear or cancel). */
  onClose: () => void;
}

/**
 * Set / clear custom-text status popover (#1233 B5).
 *
 * A text input (max 140 Unicode code points) with a live remaining-code-point
 * counter, an optional emoji via the shared EmojiPicker, and Save / Clear
 * actions. On Save it PATCHes
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
  const [retrying, setRetrying] = useState(false);
  const customStatusSaving = useRichPresenceStore((s) => s.customStatusSaving);
  const confirmedPresenceSettings = useRichPresenceStore((s) => s.confirmedPresenceSettings);
  const presenceSettingsError = useRichPresenceStore((s) => s.presenceSettingsError);
  const presenceSettingsLoading = useRichPresenceStore((s) => s.presenceSettingsLoading);
  const presenceSettingsSaving = useRichPresenceStore((s) => s.presenceSettingsSaving);
  const [error, setError] = useState<string | null>(null);
  const [emojiAnchor, setEmojiAnchor] = useState<{ x: number; y: number } | null>(null);

  const emojiBtnRef = useRef<HTMLButtonElement>(null);
  const mountedRef = useRef(false);
  const textDirtyRef = useRef(false);
  const emojiDirtyRef = useRef(false);
  const refreshAfterCustomStatusSaveRef = useRef(customStatusSaving);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  useLayoutEffect(() => {
    if (!refreshAfterCustomStatusSaveRef.current || customStatusSaving) return;

    if (!textDirtyRef.current) setText(self.customText ?? '');
    if (!emojiDirtyRef.current) setEmoji(self.customTextEmoji ?? '');
    refreshAfterCustomStatusSaveRef.current = false;
  }, [customStatusSaving, self.customText, self.customTextEmoji]);

  const textLength = [...text].length;
  const remaining = MAX_LEN - textLength;
  const overLimit = textLength > MAX_LEN;
  const counterMagnitude = Math.abs(remaining);
  const counterUnit = counterMagnitude === 1 ? 'point' : 'points';
  const counterText =
    remaining >= 0
      ? `${remaining} code ${counterUnit} remaining`
      : `${counterMagnitude} code ${counterUnit} over limit`;
  const trimmed = text.trim();
  const needsReconciliation = confirmedPresenceSettings === null && presenceSettingsError !== null;
  const writeDisabled =
    saving || customStatusSaving || presenceSettingsLoading || needsReconciliation;
  const displayedError = error ?? (needsReconciliation ? presenceSettingsError : null);

  const submit = useCallback(
    async (customText: string, customTextEmoji: string, fallback: string) => {
      if (writeDisabled) return;
      const store = useRichPresenceStore.getState();
      const ticket = store.captureCustomStatusSubmission();
      if (ticket === null) return;
      setSaving(true);
      setError(null);
      let reconcileOnFailure = true;

      try {
        const response = await apiFetch('/api/v1/users/me/presence-settings', {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ custom_text: customText, custom_text_emoji: customTextEmoji }),
        });
        if (!response.ok) {
          const error = await presenceSettingsResponseError(response, fallback);
          reconcileOnFailure = !isDefinitePresenceSettingsPatchRejection(response);
          throw error;
        }

        const raw: unknown = await response.json();
        const settings = parsePresenceSettingsResponse(raw);
        if (settings === null) {
          throw new Error('Received invalid Rich Presence settings');
        }
        const result = useRichPresenceStore.getState().applyCustomStatusSettings(settings, ticket);
        if (!result.contextCurrent || !mountedRef.current) return;
        if (result.activityCurrent && result.customCurrent) onClose();
        else setError(CUSTOM_STATUS_RECONCILIATION_ERROR);
      } catch (error) {
        const current = useRichPresenceStore.getState().getCustomStatusSubmissionResult(ticket);
        if (!current.contextCurrent) return;
        updatePopoverStateIfMounted(mountedRef, () =>
          setError(error instanceof Error && error.message ? error.message : fallback)
        );
        if (reconcileOnFailure) {
          await useRichPresenceStore.getState().reconcileCustomStatusAmbiguousOutcome(ticket);
          const reconciled = useRichPresenceStore.getState();
          finishAmbiguousReconciliation(
            reconciled.getCustomStatusSubmissionResult(ticket),
            reconciled.confirmedPresenceSettings,
            customText,
            customTextEmoji,
            mountedRef,
            onClose,
            setError
          );
        }
      } finally {
        useRichPresenceStore.getState().releaseCustomStatusSubmission(ticket);
        updatePopoverStateIfMounted(mountedRef, () => setSaving(false));
      }
    },
    [onClose, writeDisabled]
  );

  // PATCH the presence-settings endpoint and mirror the applied values into the
  // store. Empty text clears both fields (server interprets '' as a clear).
  const handleSave = useCallback(async () => {
    if (overLimit || writeDisabled) return;
    const isClear = trimmed.length === 0;
    await submit(isClear ? '' : text, isClear ? '' : emoji, 'Failed to update status');
  }, [emoji, overLimit, submit, text, trimmed.length, writeDisabled]);

  // Clear: send empty strings and wipe the store's self custom text.
  const handleClear = useCallback(async () => {
    if (writeDisabled) return;
    await submit('', '', 'Failed to clear status');
  }, [submit, writeDisabled]);

  const handleRetry = useCallback(async () => {
    if (
      presenceSettingsLoading ||
      presenceSettingsSaving ||
      retrying ||
      useRichPresenceStore.getState().presenceSettingsLoading ||
      useRichPresenceStore.getState().presenceSettingsSaving
    ) {
      return;
    }
    setRetrying(true);
    setError(null);
    try {
      await useRichPresenceStore.getState().hydratePresenceSettings();
    } finally {
      updatePopoverStateIfMounted(mountedRef, () => setRetrying(false));
    }
  }, [presenceSettingsLoading, presenceSettingsSaving, retrying]);

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
          maxLength={MAX_INPUT_CODE_UNITS}
          aria-invalid={overLimit}
          aria-describedby="custom-status-counter"
          placeholder="What's happening?"
          aria-label="Custom status text"
          onChange={(e) => {
            textDirtyRef.current = true;
            setText(e.target.value);
          }}
          onKeyDown={(e) => {
            if (e.key === 'Enter') handleSave();
          }}
          autoFocus
        />
      </div>

      <div className="custom-status-popover-meta">
        {emoji && (
          <button
            type="button"
            className="custom-status-emoji-remove"
            onClick={() => {
              emojiDirtyRef.current = true;
              setEmoji('');
            }}
          >
            Remove emoji
          </button>
        )}
        <span
          id="custom-status-counter"
          className={`custom-status-counter ${overLimit ? 'over-limit' : ''}`}
          aria-live="polite"
          aria-atomic="true"
        >
          {counterText}
        </span>
      </div>

      {displayedError && (
        <div className="custom-status-error" role="alert">
          {displayedError}
        </div>
      )}

      <div className="custom-status-popover-actions">
        <button
          type="button"
          className="custom-status-clear-btn"
          onClick={handleClear}
          disabled={writeDisabled}
        >
          Clear
        </button>
        <button
          type="button"
          className="custom-status-save-btn"
          onClick={handleSave}
          disabled={writeDisabled || overLimit}
        >
          {saving ? 'Saving...' : 'Save'}
        </button>
        {(needsReconciliation || retrying) && (
          <button
            type="button"
            className="custom-status-save-btn"
            onClick={handleRetry}
            disabled={
              saving ||
              customStatusSaving ||
              presenceSettingsLoading ||
              presenceSettingsSaving ||
              retrying
            }
          >
            {retrying ? 'Retrying...' : 'Try again'}
          </button>
        )}
      </div>

      {showEmojiPicker && (
        <EmojiPicker
          mode="popover"
          position={emojiAnchor ?? { x: 0, y: 0 }}
          onSelect={(selected) => {
            emojiDirtyRef.current = true;
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
