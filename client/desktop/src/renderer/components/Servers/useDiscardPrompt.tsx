import React, { useCallback, useEffect, useRef, useState } from 'react';
import ConfirmActionModal from '../ui/ConfirmActionModal';

/**
 * The discard guard `openVerificationSetup` takes (#3456 §3.6a, D-4): asked before anything
 * closes, because leaving Server Settings for App Settings unmounts the page and its edits.
 *
 * `confirmDiscard` resolves true at once when `isDirty()` says nothing would be lost; otherwise
 * it asks, and resolves with the answer. `prompt` is the question, to render once in the page.
 */
export function useDiscardPrompt(isDirty: () => boolean): {
  confirmDiscard: () => Promise<boolean>;
  prompt: React.ReactNode;
} {
  const [open, setOpen] = useState(false);
  const resolveRef = useRef<((discard: boolean) => void) | null>(null);

  // First answer wins: `ConfirmActionModal` closes itself after a confirmation too, and that
  // close must not read as a refusal.
  const settle = useCallback((discard: boolean) => {
    resolveRef.current?.(discard);
    resolveRef.current = null;
    setOpen(false);
  }, []);

  // A page that unmounts with the question up keeps whatever called it from waiting forever.
  useEffect(() => () => settle(false), [settle]);

  const confirmDiscard = () => {
    if (!isDirty()) return Promise.resolve(true);
    return new Promise<boolean>((resolve) => {
      resolveRef.current?.(false);
      resolveRef.current = resolve;
      setOpen(true);
    });
  };

  const prompt = (
    <ConfirmActionModal
      isOpen={open}
      onClose={() => settle(false)}
      title="Discard unsaved changes?"
      message="Setting up verification leaves Server Settings. Your unsaved changes here will be lost."
      confirmLabel="Discard Changes"
      loadingLabel="Discarding..."
      onConfirm={async () => settle(true)}
    />
  );

  return { confirmDiscard, prompt };
}
