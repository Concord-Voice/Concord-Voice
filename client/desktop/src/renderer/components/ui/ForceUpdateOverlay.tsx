import React, { useCallback, useEffect, useRef, useState } from 'react';
import { AlertTriangle, Download, RefreshCw, Loader, ExternalLink } from 'lucide-react';
import { useClientConfigStore } from '../../stores/clientConfigStore';
import './ForceUpdateOverlay.css';

type Phase = 'checking' | 'downloading' | 'downloaded' | 'error';

const DOWNLOAD_URL = 'https://concordvoice.com/download';
/** After this many failed attempts, show a "Continue Anyway" escape hatch.
 *  This is intentional: the overlay is a UX gate, not a hard security boundary.
 *  Hard enforcement is done server-side by rejecting API calls from outdated clients.
 *  The escape prevents permanent lockout on network failure or delayed releases. */
const ESCAPE_AFTER_FAILURES = 2;

/** Compare two semver strings. Returns negative if a < b, 0 if equal, positive if a > b.
 *  Strips prerelease/build metadata (e.g. "0.2.0-beta.1" → "0.2.0") before comparing. */
function compareSemver(a: string, b: string): number {
  const strip = (v: string) =>
    v
      .replace(/[-+].*$/, '')
      .split('.')
      .map(Number);
  const pa = strip(a);
  const pb = strip(b);
  for (let i = 0; i < 3; i++) {
    const diff = (pa[i] || 0) - (pb[i] || 0);
    if (diff !== 0) return diff;
  }
  return 0;
}

const ForceUpdateOverlay: React.FC = () => {
  const minVersion = useClientConfigStore((s) => s.minVersion);
  const lastFetchedAt = useClientConfigStore((s) => s.lastFetchedAt);
  const [appVersion, setAppVersion] = useState<string | null>(null);
  const [phase, setPhase] = useState<Phase | null>(null);
  const [progress, setProgress] = useState(0);
  const [errorMsg, setErrorMsg] = useState('');
  const failureCountRef = useRef(0);
  const [dismissed, setDismissed] = useState(false);

  // Get current app version on mount
  useEffect(() => {
    globalThis.electron?.getVersion?.().then(setAppVersion);
  }, []);

  // Brief grace period on mount so the overlay doesn't flash during initial render.
  // Config fetch has a 2s startup delay, so this just prevents sub-second flicker
  // if cached store state happens to have a stale minVersion.
  const [ready, setReady] = useState(false);
  useEffect(() => {
    const t = setTimeout(() => setReady(true), 1000);
    return () => clearTimeout(t);
  }, []);

  const updateRequired = !!(
    ready &&
    appVersion &&
    minVersion &&
    lastFetchedAt &&
    compareSemver(appVersion, minVersion) < 0
  );

  // Subscribe to update IPC events when overlay is active
  useEffect(() => {
    if (!updateRequired) return;

    const cleanups: (() => void)[] = [];

    if (globalThis.electron?.onUpdateAvailable) {
      cleanups.push(
        globalThis.electron.onUpdateAvailable(() => {
          // Auto-download — this is a mandatory update
          setPhase('downloading');
          setProgress(0);
          globalThis.electron?.downloadUpdate();
        })
      );
    }
    if (globalThis.electron?.onUpdateNotAvailable) {
      cleanups.push(
        globalThis.electron.onUpdateNotAvailable(() => {
          failureCountRef.current++;
          setPhase('error');
          setErrorMsg(
            'No update is available yet. The release may still be building. Please try again shortly.'
          );
        })
      );
    }
    if (globalThis.electron?.onUpdateDownloadProgress) {
      cleanups.push(
        globalThis.electron.onUpdateDownloadProgress((p) => {
          setPhase('downloading');
          setProgress(Math.round(p.percent));
        })
      );
    }
    if (globalThis.electron?.onUpdateDownloaded) {
      cleanups.push(globalThis.electron.onUpdateDownloaded(() => setPhase('downloaded')));
    }
    if (globalThis.electron?.onUpdateError) {
      cleanups.push(
        globalThis.electron.onUpdateError((err) => {
          failureCountRef.current++;
          setPhase('error');
          setErrorMsg(err.message || 'An unexpected error occurred.');
        })
      );
    }

    return () => {
      for (const fn of cleanups) fn();
    };
  }, [updateRequired]);

  const handleUpdate = useCallback(async () => {
    setPhase('checking');
    setErrorMsg('');
    try {
      await globalThis.electron?.checkForUpdates();
    } catch {
      failureCountRef.current++;
      setPhase('error');
      setErrorMsg('Failed to check for updates.');
    }
  }, []);

  const handleInstall = useCallback(() => {
    globalThis.electron?.installUpdate();
  }, []);

  if (!updateRequired || dismissed) return null;

  const showEscape = phase === 'error' && failureCountRef.current >= ESCAPE_AFTER_FAILURES;

  return (
    <div className="force-update-overlay">
      <div className="force-update-card">
        <div className="force-update-icon">
          <AlertTriangle size={48} />
        </div>
        <h2 className="force-update-title">Update Required</h2>
        <p className="force-update-message">
          Your version (<strong>v{appVersion}</strong>) is below the minimum required version ({' '}
          <strong>v{minVersion}</strong>). Please update to continue using Concord.
        </p>

        {phase === 'checking' && (
          <div className="force-update-status">
            <Loader size={18} className="force-update-spinner" />
            <span>Checking for updates&hellip;</span>
          </div>
        )}

        {phase === 'downloading' && (
          <div className="force-update-status">
            <div className="force-update-progress-bar">
              <div className="force-update-progress-fill" style={{ width: `${progress}%` }} />
            </div>
            <span className="force-update-progress-text">Downloading&hellip; {progress}%</span>
          </div>
        )}

        {phase === 'downloaded' && (
          <div className="force-update-status success">
            <span>Update downloaded and ready to install.</span>
          </div>
        )}

        {phase === 'error' && (
          <div className="force-update-status error">
            <span>{errorMsg}</span>
          </div>
        )}

        <div className="force-update-actions">
          {(!phase || phase === 'error') && (
            <button className="force-update-btn primary" onClick={handleUpdate}>
              <Download size={16} />
              {phase === 'error' ? 'Retry' : 'Update Now'}
            </button>
          )}
          {phase === 'downloaded' && (
            <button className="force-update-btn primary" onClick={handleInstall}>
              <RefreshCw size={16} />
              Install & Restart
            </button>
          )}
        </div>

        {phase === 'error' && (
          <a
            className="force-update-manual-link"
            href={DOWNLOAD_URL}
            target="_blank"
            rel="noopener noreferrer"
          >
            <ExternalLink size={13} />
            Download manually from concordvoice.com
          </a>
        )}

        {showEscape && (
          <button
            className="force-update-btn secondary force-update-escape"
            onClick={() => setDismissed(true)}
          >
            Continue Anyway
          </button>
        )}
      </div>
    </div>
  );
};

export default ForceUpdateOverlay;
