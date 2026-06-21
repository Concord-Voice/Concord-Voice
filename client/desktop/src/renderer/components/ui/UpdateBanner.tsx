import { useEffect, useState, useCallback } from 'react';
import { AlertTriangle, Download, RefreshCw, X } from 'lucide-react';
import './UpdateBanner.css';

type BannerState = 'hidden' | 'available' | 'downloading' | 'downloaded' | 'rollback';

interface DownloadProgress {
  percent: number;
  transferred: number;
  total: number;
  bytesPerSecond: number;
}

const DISMISSED_KEY = 'concord:update-banner-dismissed';
/** Dismiss expires after 7 days — re-shows the banner for the same version */
const DISMISS_TTL_MS = 7 * 24 * 60 * 60 * 1000;
/** Defense-in-depth cap on version string length (semver max realistically under 64) */
const MAX_VERSION_LEN = 64;
/** Semver-like shape: X.Y.Z optionally followed by `-pre` and/or `+build` metadata */
const SEMVER_RE = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$/;

/**
 * Boundary validator for version strings flowing in from the auto-updater IPC
 * and out to localStorage. Rejects empty, oversized, or non-semver-shaped
 * values so a compromised update channel cannot poison persisted state.
 */
function isValidVersion(v: unknown): v is string {
  return typeof v === 'string' && v.length > 0 && v.length <= MAX_VERSION_LEN && SEMVER_RE.test(v);
}

function getDismissed(): { version: string; at: number } | null {
  try {
    const raw = localStorage.getItem(DISMISSED_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw);
    if (parsed && isValidVersion(parsed.version) && typeof parsed.at === 'number') {
      return { version: parsed.version, at: parsed.at };
    }
    // Stored value didn't pass the validator — discard rather than trust
    localStorage.removeItem(DISMISSED_KEY);
  } catch {
    // Corrupted — remove to stop repeated parse failures
    localStorage.removeItem(DISMISSED_KEY);
  }
  // Migrate legacy format (plain version string)
  const legacy = localStorage.getItem('concord:update-banner-dismissed-version');
  if (legacy) {
    localStorage.removeItem('concord:update-banner-dismissed-version');
    return null;
  }
  return null;
}

const UpdateBanner: React.FC = () => {
  const [state, setState] = useState<BannerState>('hidden');
  const [version, setVersion] = useState('');
  const [rollbackMessage, setRollbackMessage] = useState('');
  const [progress, setProgress] = useState<DownloadProgress | null>(null);
  const [dismissedVersion, setDismissedVersion] = useState(() => {
    const dismissed = getDismissed();
    if (!dismissed) return '';
    // Expired — clear and re-show
    if (Date.now() - dismissed.at > DISMISS_TTL_MS) {
      localStorage.removeItem(DISMISSED_KEY);
      return '';
    }
    return dismissed.version;
  });

  useEffect(() => {
    if (!globalThis.electron?.onUpdateAvailable) return;

    const cleanups = [
      globalThis.electron.onUpdateAvailable((info) => {
        setVersion(info.version);
        setState('available');
      }),
      globalThis.electron.onUpdateDownloadProgress((prog) => {
        setState('downloading');
        setProgress(prog);
      }),
      globalThis.electron.onUpdateDownloaded((info) => {
        setVersion(info.version);
        setState('downloaded');
        setProgress(null);
      }),
      globalThis.electron.onUpdateNotAvailable(() => {
        // No action — stays hidden or dismissed
      }),
      globalThis.electron.onUpdateError(() => {
        // Reset to available so user can retry; main process logs details
        setState((prev) => (prev === 'downloading' ? 'available' : prev));
        setProgress(null);
      }),
      ...(globalThis.electron.onUpdateRollback
        ? [
            globalThis.electron.onUpdateRollback((data) => {
              setRollbackMessage(data.message);
              setState('rollback');
            }),
          ]
        : []),
    ];

    return () => {
      for (const fn of cleanups) fn();
    };
  }, []);

  const handleDownload = useCallback(async () => {
    setState('downloading');
    setProgress({ percent: 0, transferred: 0, total: 0, bytesPerSecond: 0 });
    try {
      await globalThis.electron?.downloadUpdate();
    } catch {
      // Reset to available so user can retry; onUpdateError also fires
      setState('available');
      setProgress(null);
    }
  }, []);

  const handleInstall = useCallback(() => {
    globalThis.electron?.installUpdate();
  }, []);

  const handleCheckAndRetry = useCallback(async () => {
    setState('hidden');
    setRollbackMessage('');
    try {
      await globalThis.electron?.checkForUpdates();
    } catch {
      // Restore rollback state so the user gets feedback instead of a silent hidden banner
      setState('rollback');
      setRollbackMessage('Failed to check for updates. Please try again later.');
    }
  }, []);

  const handleDismiss = useCallback(() => {
    // Treat the persisted-dismiss as a security boundary: only write shape-validated versions.
    // A compromised update channel could otherwise inject arbitrary payloads through `version`.
    if (isValidVersion(version)) {
      localStorage.setItem(DISMISSED_KEY, JSON.stringify({ version, at: Date.now() }));
      setDismissedVersion(version);
      return;
    }
    // Validator rejected the version. Don't write to localStorage, but still
    // hide the banner for the current session so the dismiss button isn't a
    // silent no-op (a confusing UX where clicking X does nothing visible).
    // The banner will re-appear on next session because nothing was persisted.
    console.warn('[UpdateBanner] dismiss skipped persistence: version failed shape validation');
    setDismissedVersion(version);
  }, [version]);

  if (state === 'hidden') return null;
  if (state !== 'rollback' && dismissedVersion && dismissedVersion === version) return null;

  return (
    <div className={`update-banner update-banner--${state}`}>
      <div className="update-banner__icon">
        {state === 'rollback' && <AlertTriangle size={16} />}
        {state === 'downloaded' && <RefreshCw size={16} />}
        {(state === 'available' || state === 'downloading') && <Download size={16} />}
      </div>

      <div className="update-banner__text">
        {state === 'available' && `Update available: v${version}`}
        {state === 'downloading' &&
          'Downloading update\u2026 ' + (progress ? progress.percent.toFixed(0) + '%' : '')}
        {state === 'downloaded' && `v${version} ready to install`}
        {state === 'rollback' && rollbackMessage}
      </div>

      <div className="update-banner__actions">
        {state === 'available' && (
          <button className="update-banner__btn primary" onClick={handleDownload}>
            Download
          </button>
        )}
        {state === 'downloaded' && (
          <button className="update-banner__btn primary" onClick={handleInstall}>
            Restart Now
          </button>
        )}
        {state === 'rollback' && (
          <button className="update-banner__btn primary" onClick={handleCheckAndRetry}>
            Retry
          </button>
        )}
        {state !== 'rollback' && (
          <button
            className="update-banner__btn dismiss"
            onClick={handleDismiss}
            aria-label="Dismiss"
          >
            <X size={14} />
          </button>
        )}
      </div>

      {state === 'downloading' && progress && (
        <div className="update-banner__progress" style={{ width: `${progress.percent}%` }} />
      )}
    </div>
  );
};

export default UpdateBanner;
