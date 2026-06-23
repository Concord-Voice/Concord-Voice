import React, { useEffect, useState } from 'react';
import './Titlebar.css';

// Compose the version text from two independent state slices. Returns empty
// string for both `null` (loading state) AND empty-string (degenerate upstream
// case where `app.getVersion()` returned ''). The truthiness gate at the
// rendering site (`{versionText && <span>...}`) then suppresses the version
// span entirely — no broken `'v'` or `'v-<hash>'` content ever reaches the DOM.
// Defense-in-depth note: the strict-null check that previously lived here
// (Gitar #1153 finding) would let `formatVersionText('', null)` return `'v'`,
// the exact broken state this PR aims to prevent. Truthiness check forecloses it.
function formatVersionText(appVersion: string | null, spaHash: string | null): string {
  if (!appVersion) return '';
  if (spaHash) return `v${appVersion}-${spaHash}`;
  return `v${appVersion}`;
}

export const Titlebar: React.FC = () => {
  // Split state to eliminate the synthesis race: pre-fix code stored a
  // composite VersionInfo and synthesized { appVersion: '' } when onChange
  // fired before get() resolved, briefly rendering 'v' or 'v-<hash>'. The
  // two slices update independently and the formatter gates on appVersion.
  const [appVersion, setAppVersion] = useState<string | null>(null);
  const [spaHash, setSpaHash] = useState<string | null>(null);
  const platform =
    typeof navigator === 'undefined'
      ? ''
      : ((navigator as Navigator & { userAgentData?: { platform?: string } }).userAgentData
          ?.platform ??
        navigator.platform ??
        '');
  const isMac = platform.toLowerCase().includes('mac');

  useEffect(() => {
    // Defensive `?.` chains: in unit-test environments and during the brief
    // preload-bridge initialization window, `window.electron.version` may not
    // be installed yet. Skipping the fetch + subscription cleanly lets the
    // component render the brand text without crashing.
    const versionApi = globalThis.electron?.version;
    if (!versionApi) {
      return;
    }

    let mounted = true;
    void (async () => {
      try {
        const v = await versionApi.get();
        if (mounted) {
          setAppVersion(v.appVersion);
          setSpaHash(v.spaHash);
        }
      } catch (err) {
        console.error('[Titlebar] failed to fetch version:', (err as Error).message);
      }
    })();

    // onChange writes only spaHash. If it fires before get() resolves,
    // appVersion stays null and formatVersionText returns ''; no broken
    // 'v' or 'v-<hash>' state is ever rendered.
    const unsubscribe = versionApi.onChange((data) => {
      setSpaHash(data.spaHash);
    });

    return () => {
      mounted = false;
      unsubscribe();
    };
  }, []);

  const versionText = formatVersionText(appVersion, spaHash);

  return (
    <div className={`titlebar ${isMac ? 'titlebar--mac' : ''}`}>
      <div className="titlebar-center">
        <span className="titlebar-title">Concord Voice</span>
        {versionText && <span className="titlebar-version">{versionText}</span>}
      </div>
    </div>
  );
};
