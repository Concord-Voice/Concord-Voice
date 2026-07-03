import React from 'react';
import { KICKSTARTER_URL } from '../subscription/subscriptionCopy';

// Beta Kickstarter promo card (#1304). Links out to the Concord Voice Kickstarter
// campaign. Per electron.md's external-link scheme policy, a user-supplied /
// external URL opened on click MUST route through window.electron.openExternal
// (the explicit click is consent; the IPC handler validates https/http/mailto)
// AND carry target="_blank" rel="noopener noreferrer" so that, if the preload
// bridge is absent (tests / future env), the main-process setWindowOpenHandler
// re-validates and routes the https URL to the OS browser.
//
// Brand: the card uses Concord's own symbol mark (public/branding — self-hosted,
// no CDN). We intentionally do NOT ship the Kickstarter wordmark/logo (that is
// Kickstarter's trademark); the text names the campaign instead.

function openCampaign(e: React.MouseEvent<HTMLAnchorElement>): void {
  const api = (
    globalThis as unknown as {
      electron?: { openExternal?: (url: string) => Promise<unknown> | void };
    }
  ).electron;
  if (api && typeof api.openExternal === 'function') {
    e.preventDefault();
    const result = api.openExternal(KICKSTARTER_URL);
    if (result instanceof Promise) {
      result.catch(() => {
        /* main-process logged the failure; renderer treats as no-op */
      });
    }
  }
  // If the bridge is absent, default anchor activation fires and the main-process
  // setWindowOpenHandler re-validates + routes the https URL to the OS browser.
}

const KickstarterPromoCard: React.FC = () => (
  <aside className="subscription-kickstarter-card">
    <img
      className="subscription-kickstarter-logo"
      src="/branding/Concord-Voice/logos/symbol-transparent-vector.svg"
      alt=""
      aria-hidden="true"
      width={48}
      height={48}
    />
    <div className="subscription-kickstarter-body">
      <h3 className="subscription-kickstarter-title">Back Concord Voice on Kickstarter</h3>
      <p className="subscription-kickstarter-text">
        Concord Voice is community-funded and source-available. Support the campaign to help us ship
        v1.0 — backers get Supersonic codes to redeem right here.
      </p>
      <a
        className="subscription-kickstarter-link"
        href={KICKSTARTER_URL}
        target="_blank"
        rel="noopener noreferrer"
        onClick={openCampaign}
      >
        View the Kickstarter campaign
      </a>
    </div>
  </aside>
);

export default KickstarterPromoCard;
