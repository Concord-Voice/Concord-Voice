import { useCallback, useEffect, useRef, useState } from 'react';

/**
 * The swap (#3456 §3.3, D-5): a host whose `ConfirmActionModal` confirms a gated
 * action hands over to `DangerousActionStepUpDialog` when the first send is
 * refused for verification, and the step-up dialog stands in its place.
 *
 * `ConfirmActionModal` calls its `onClose` in the same continuation that
 * resolves `onConfirm`, before React renders again. That close is the
 * confirmation giving way, not the action ending, so the host's own end must
 * not run for it: `handOff` flags it synchronously (state would arrive too
 * late) and `confirmClosed` swallows exactly that one close.
 *
 * `endStepUp` closes the dialog and tells the host (`onEnd`) once it has
 * closed, not in the same commit. A host that its parent unmounts on `onEnd`
 * (the channel and server deletions) would otherwise take the dialog with it
 * before the dialog's focus fallback ran, and focus would drop to `<body>`.
 *
 * In the commit between the two, `pending` is null again, so a host that opens
 * its confirmation on `pending === null` would reopen it for that one commit:
 * `ui/Modal` would mount, take focus into a dialog that `onEnd` then removes,
 * and the dialog's fallback would find focus already placed. `ending` is true
 * for exactly that commit; a host that opens its confirmation on
 * `pending === null && !ending` keeps it closed.
 */
export function useStepUpHandoff<T>(onEnd: () => void): {
  /** What was handed over, while the step-up dialog is up. */
  pending: T | null;
  /** The step-up has ended and `onEnd` has not run yet: keep the confirmation closed. */
  ending: boolean;
  /** The confirmation's first send was refused for verification: show the dialog. */
  handOff: (next: T) => void;
  /** The confirmation's `onClose`. */
  confirmClosed: () => void;
  /** The dialog is over: the action succeeded, or was cancelled or abandoned. */
  endStepUp: () => void;
} {
  const [pending, setPending] = useState<T | null>(null);
  const [ending, setEnding] = useState(false);
  const handingOffRef = useRef(false);

  const handOff = useCallback((next: T) => {
    handingOffRef.current = true;
    setPending(next);
  }, []);

  const confirmClosed = () => {
    if (handingOffRef.current) {
      handingOffRef.current = false;
      return;
    }
    onEnd();
  };

  const endStepUp = useCallback(() => {
    handingOffRef.current = false;
    setPending(null);
    setEnding(true);
  }, []);

  useEffect(() => {
    if (!ending) return;
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: closes the one-commit `ending` window once `onEnd` has run, so a host that outlives `onEnd` can open its confirmation again; the `ending` guard stops the setter re-triggering the effect (no render loop)
    setEnding(false);
    onEnd();
  }, [ending, onEnd]);

  return { pending, ending, handOff, confirmClosed, endStepUp };
}
