import React, { useCallback, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { Lock } from 'lucide-react';

/**
 * Computes the on-screen position for the E2EE tooltip relative to its trigger,
 * preferring right-edge alignment and clamping to the viewport. Moved here with
 * the indicator (from MainView) so MainView's cognitive complexity stays below
 * the SonarCloud rule threshold (SC-1).
 */
const computeE2eeTooltipPos = (rect: DOMRect): { top: number; left: number } => {
  const tooltipWidth = 240;
  const padding = 8;
  const top = rect.bottom + padding;

  const leftAligned = rect.right - tooltipWidth;
  if (leftAligned >= padding) {
    return { top, left: leftAligned };
  }
  const maxLeft = globalThis.innerWidth - tooltipWidth - padding;
  return { top, left: Math.min(rect.left, maxLeft) };
};

/**
 * The server-level E2EE lock shown in the channel-panel header, plus its hover
 * tooltip (portaled to <body> to escape transform-based containing blocks).
 * Extracted from MainView (SC-1) so the E2EE state / ref / callbacks / portal no
 * longer inflate MainView's cognitive complexity. The tooltip portals to
 * document.body, so co-locating it with its trigger does not move it in the DOM.
 *
 * Renders one unconditional always-encrypted state: under the E2EE-everywhere
 * structural invariant (#201) every channel is encrypted by construction, so
 * the badge has no per-server data dependency (the misleading per-server
 * e2ee_default opt-out was removed in #1647).
 */
const ServerE2eeIndicator: React.FC = () => {
  const [tooltipPos, setTooltipPos] = useState<{ top: number; left: number } | null>(null);
  const indicatorRef = useRef<HTMLButtonElement>(null);

  const showTooltip = useCallback(() => {
    if (indicatorRef.current) {
      setTooltipPos(computeE2eeTooltipPos(indicatorRef.current.getBoundingClientRect()));
    }
  }, []);
  const hideTooltip = useCallback(() => setTooltipPos(null), []);

  return (
    <>
      <button
        type="button"
        ref={indicatorRef}
        className="server-e2ee-indicator encrypted"
        aria-label="Server end-to-end encryption: enabled"
        onMouseEnter={showTooltip}
        onMouseLeave={hideTooltip}
        onFocus={showTooltip}
        onBlur={hideTooltip}
      >
        <Lock size={12} />
      </button>
      {tooltipPos &&
        createPortal(
          <div
            className="e2ee-tooltip visible encrypted"
            style={{ top: tooltipPos.top, left: tooltipPos.left }}
          >
            <span className="e2ee-tooltip-header">
              <Lock size={11} />
              E2EE Enabled
            </span>
            <span className="e2ee-tooltip-body">
              End-to-end encryption is always on in this server. All channels are encrypted.
            </span>
            <span className="e2ee-tooltip-hint">
              Always check the channel encryption status near the message box to verify E2EE.
            </span>
          </div>,
          document.body
        )}
    </>
  );
};

export default ServerE2eeIndicator;
