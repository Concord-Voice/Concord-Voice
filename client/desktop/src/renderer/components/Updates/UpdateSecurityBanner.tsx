import React, { useEffect, useState, type MouseEvent } from 'react';
import { AlertTriangle, ExternalLink } from 'lucide-react';
import {
  useUpdateStatusStore,
  type UpdateCriticalErrorSubtype,
} from '../../stores/updateStatusStore';
import './UpdateSecurityBanner.css';

/**
 * MVP CTA target: the public mirror's Releases page, which hosts the signed
 * installers (#1667 — "private signs, public hosts"). A dedicated
 * concordvoice.com/download page with OS/arch detection is the scoped §11
 * follow-up; until then GitHub Releases ships all assets in one place.
 */
const DOWNLOAD_URL = 'https://github.com/Concord-Voice/Concord-Voice/releases/latest';

// Exhaustive map keyed on UpdateCriticalErrorSubtype so adding a new subtype
// forces a matching copy entry (TS compile error otherwise). Per #719 review.
const COPY_BY_SUBTYPE: Record<UpdateCriticalErrorSubtype, { headline: string; confirm: string }> = {
  'cert-pin-failure': {
    headline:
      "Updates can't be verified — we couldn't confirm the identity of the update server. Your version may be out of date.",
    confirm: 'I understand Concord cannot verify updates and I will reinstall manually.',
  },
  'publisher-failure': {
    headline:
      'Updates blocked — the downloaded installer failed publisher verification. Your version may be out of date.',
    confirm:
      'I understand Concord cannot verify the downloaded update and I will reinstall manually.',
  },
};

/**
 * Route external clicks through the preload bridge's openExternal (hardened
 * by main.ts's setWindowOpenHandler at main.ts:239 and the preload
 * validator). Falls back to default anchor behavior if the bridge is absent —
 * main-process setWindowOpenHandler re-validates in either case. Mirrors
 * SafeLink.tsx's approach without coupling to the Markdown pipeline. #658
 */
function isPromiseLike(value: unknown): value is Promise<unknown> {
  if (value === null || typeof value !== 'object') return false;
  if (!('catch' in value)) return false;
  return typeof value.catch === 'function';
}

function handleCtaClick(e: MouseEvent<HTMLAnchorElement>): void {
  const api = (
    globalThis as unknown as {
      electron?: { openExternal?: (url: string) => Promise<unknown> | void };
    }
  ).electron;
  if (!api || typeof api.openExternal !== 'function') return;
  e.preventDefault();
  const result = api.openExternal(DOWNLOAD_URL);
  if (isPromiseLike(result)) {
    result.catch(() => {
      /* main-process logs failures; renderer is no-op */
    });
  }
}

/**
 * Persistent banner shown when the updater encountered a critical security
 * failure (cert-pin miss or signature verification failure). Dismiss is a
 * two-step consenting action: first click surfaces an explicit acknowledgement
 * prompt, second click dismisses for the current session only. Next launch
 * re-renders if the underlying condition is unresolved. Issue #658.
 */
export const UpdateSecurityBanner: React.FC = () => {
  const criticalError = useUpdateStatusStore((s) => s.criticalError);
  const dismissedForSession = useUpdateStatusStore((s) => s.dismissedForSession);
  const dismissForSession = useUpdateStatusStore((s) => s.dismissForSession);
  const [showConfirm, setShowConfirm] = useState(false);

  // Reset the confirm-prompt local state whenever a new critical error
  // arrives (different subtype OR different message). Without this, a user
  // who clicked "Dismiss" for error A but didn't yet click the consent
  // button would still see the consent UI when error B arrived, and a
  // single click would dismiss B — bypassing the two-step consent gate.
  // Per #719 Copilot review 3.
  const subtype = criticalError?.subtype;
  const message = criticalError?.message;
  useEffect(() => {
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets showConfirm when the error changes so stale dismiss doesn't bypass the two-step consent gate; not a render loop
    setShowConfirm(false);
  }, [subtype, message]);

  if (!criticalError || dismissedForSession) {
    return null;
  }

  const copy = COPY_BY_SUBTYPE[criticalError.subtype] ?? COPY_BY_SUBTYPE['cert-pin-failure'];

  return (
    <div className="update-security-banner" role="alert" aria-live="assertive">
      <AlertTriangle className="update-security-banner__icon" aria-hidden="true" />
      <span className="update-security-banner__message">
        {copy.headline}{' '}
        <a
          href={DOWNLOAD_URL}
          target="_blank"
          rel="noopener noreferrer"
          className="update-security-banner__cta"
          onClick={handleCtaClick}
        >
          Download the latest
          <ExternalLink size={14} aria-hidden="true" />
        </a>
      </span>
      {showConfirm ? (
        <span className="update-security-banner__confirm-row">
          <span className="update-security-banner__confirm-copy">{copy.confirm}</span>
          <button
            type="button"
            onClick={dismissForSession}
            className="update-security-banner__confirm"
          >
            I understand — dismiss for this session
          </button>
        </span>
      ) : (
        <button
          type="button"
          onClick={() => setShowConfirm(true)}
          className="update-security-banner__dismiss"
        >
          Dismiss
        </button>
      )}
    </div>
  );
};
