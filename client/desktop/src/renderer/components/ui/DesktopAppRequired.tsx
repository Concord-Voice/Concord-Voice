import React from 'react';
import { Download } from 'lucide-react';
import './ForceUpdateOverlay.css';

const DOWNLOAD_URL = 'https://concordvoice.com/download';

/** A production build without the Electron preload bridge is a plain browser. */
export function isUnsupportedBrowser(isProd: boolean = import.meta.env.PROD): boolean {
  return isProd && globalThis.electron === undefined;
}

/**
 * Shown instead of the app when a production SPA build loads in a plain browser.
 * The desktop client is the only shipping client: without the preload bridge the
 * renderer cannot declare a client version or attest, so the server refuses every
 * authenticated request and the user would see an "Update Required" overlay whose
 * updater cannot run. Dev builds (Vite + Playwright e2e) are unaffected.
 */
const DesktopAppRequired: React.FC = () => (
  <main className="desktop-app-required">
    <div className="force-update-card">
      <h1 className="force-update-title">Get the Concord desktop app</h1>
      <p className="force-update-message">
        Concord runs in the desktop app. It can&apos;t be used from a web browser.
      </p>
      <div className="force-update-actions">
        <a className="force-update-btn primary" href={DOWNLOAD_URL}>
          <Download size={16} />
          Download Concord
        </a>
      </div>
    </div>
  </main>
);

export default DesktopAppRequired;
