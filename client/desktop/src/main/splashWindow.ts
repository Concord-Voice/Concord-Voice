import fs from 'node:fs';
import path from 'node:path';
import { app, BrowserWindow, screen } from 'electron';

// ─── Brand constants ─────────────────────────────────────────────────────

const SPLASH_WIDTH = 300;
const SPLASH_HEIGHT = SPLASH_WIDTH;
const BRAND_BG = '#0d0821';
const BRAND_ACCENT = '#fa709a';

// ─── Position persistence ─────────────────────────────────────────────────

const POSITION_FILE = 'splash-position.json';

function loadSavedPosition(): { x: number; y: number } | null {
  try {
    const data = JSON.parse(
      fs.readFileSync(path.join(app.getPath('userData'), POSITION_FILE), 'utf-8')
    ) as { x: number; y: number };
    // Validate the saved position is within at least one connected display
    const displays = screen.getAllDisplays();
    const onScreen = displays.some(
      (d) =>
        data.x >= d.bounds.x &&
        data.y >= d.bounds.y &&
        data.x + SPLASH_WIDTH <= d.bounds.x + d.bounds.width &&
        data.y + SPLASH_HEIGHT <= d.bounds.y + d.bounds.height
    );
    return onScreen ? { x: data.x, y: data.y } : null;
  } catch {
    return null;
  }
}

function savePosition(): void {
  if (!splashWindow || splashWindow.isDestroyed()) return;
  const bounds = splashWindow.getBounds();
  try {
    fs.writeFileSync(
      path.join(app.getPath('userData'), POSITION_FILE),
      JSON.stringify({ x: bounds.x, y: bounds.y })
    );
  } catch {
    // Best-effort — non-fatal if userData is not writable
  }
}

// ─── Splash HTML (self-contained, no external dependencies) ──────────────

const SPLASH_HTML = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body {
    background: ${BRAND_BG};
    color: #fff;
    font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    height: 100vh;
    overflow: hidden;
    -webkit-app-region: drag;
    user-select: none;
  }
  .logo {
    width: 120px;
    height: 120px;
    border-radius: 24px;
  }
  /* State 1: Pulse — startup/idle */
  #state-pulse .logo {
    animation: pulse 2s ease-in-out infinite;
  }
  @keyframes pulse {
    0%, 100% {
      transform: scale(1);
      opacity: 1;
      filter: drop-shadow(0 0 0px rgba(250, 112, 154, 0));
    }
    50% {
      transform: scale(1.05);
      opacity: 0.85;
      filter: drop-shadow(0 0 12px rgba(250, 112, 154, 0.7));
    }
  }
  /* Error modifier — applied via JS when rollback/error is detected */
  .logo-error {
    animation: error-shake 0.5s ease forwards !important;
    filter: hue-rotate(300deg) saturate(2.5) brightness(0.8) !important;
  }
  @keyframes error-shake {
    0%, 100% { transform: translateX(0); }
    20%, 60% { transform: translateX(-5px); }
    40%, 80% { transform: translateX(5px); }
  }
  /* State 2: Fill progress — download/install */
  #state-fill { display: none; }
  .logo-fill-wrapper {
    position: relative;
    width: 120px;
    height: 120px;
  }
  .logo-base {
    position: absolute;
    top: 0; left: 0;
    filter: grayscale(100%) brightness(0.35);
  }
  .logo-fill {
    position: absolute;
    top: 0; left: 0;
    clip-path: inset(100% 0 0 0);
    transition: clip-path 0.4s ease;
  }
  .status {
    margin-top: 24px;
    font-size: 13px;
    font-weight: 500;
    color: rgba(255, 255, 255, 0.7);
    text-align: center;
    transition: opacity 0.3s ease;
  }
  .status-error { color: #ff6b6b; font-weight: 600; }
  .brand {
    margin-top: 12px;
    font-size: 16px;
    font-weight: 600;
    background: linear-gradient(135deg, ${BRAND_ACCENT}, #ffe13f);
    -webkit-background-clip: text;
    -webkit-text-fill-color: transparent;
  }
</style>
</head>
<body>
  <!-- State 1: Pulse (startup/idle) — visible by default -->
  <div id="state-pulse">
    <img class="logo" id="logo-pulse" src="APP_ICON_DATA_URL" alt="Concord Voice" />
  </div>
  <!-- State 2: Fill progress (downloading) — hidden until download starts -->
  <div id="state-fill">
    <div class="logo-fill-wrapper">
      <img class="logo logo-base" src="APP_ICON_DATA_URL" alt="" />
      <img class="logo logo-fill" id="logo-fill" src="APP_ICON_DATA_URL" alt="" />
    </div>
  </div>
  <div class="brand">Concord Voice</div>
  <div class="status" id="status">Preparing for launch...</div>
</body>
</html>`;

// ─── Module state ─────────────────────────────────────────────────────────

let splashWindow: BrowserWindow | null = null;

// ─── Public API ───────────────────────────────────────────────────────────

/**
 * Show the branded splash window at the last saved position, or centered on
 * the primary display. The splash is frameless, movable, and always on top.
 */
export function showSplash(iconDataUrl?: string): void {
  if (splashWindow && !splashWindow.isDestroyed()) return;

  // Prefer saved position; fall back to centering on primary display
  const savedPos = loadSavedPosition();
  let x: number;
  let y: number;
  if (savedPos) {
    x = savedPos.x;
    y = savedPos.y;
  } else {
    const primaryDisplay = screen.getPrimaryDisplay();
    const { width: screenW, height: screenH } = primaryDisplay.workAreaSize;
    x = Math.round((screenW - SPLASH_WIDTH) / 2);
    y = Math.round((screenH - SPLASH_HEIGHT) / 2);
  }

  splashWindow = new BrowserWindow({
    width: SPLASH_WIDTH,
    height: SPLASH_HEIGHT,
    x,
    y,
    frame: false,
    resizable: false,
    movable: true,
    alwaysOnTop: true,
    skipTaskbar: true,
    backgroundColor: BRAND_BG,
    show: false,
    webPreferences: {
      nodeIntegration: false,
      contextIsolation: true,
      sandbox: true,
    },
  });

  // Replace all icon placeholders with the actual data URL (or empty for dev/tests)
  const html = SPLASH_HTML.replaceAll('APP_ICON_DATA_URL', iconDataUrl ?? '');

  splashWindow.loadURL(`data:text/html;charset=utf-8,${encodeURIComponent(html)}`);
  splashWindow.once('ready-to-show', () => {
    splashWindow?.show();
  });
}

/**
 * Update the status text displayed on the splash window.
 * No-op if the splash window doesn't exist or has been destroyed.
 */
export function updateSplashStatus(text: string): void {
  if (!splashWindow || splashWindow.isDestroyed()) return;

  splashWindow.webContents
    .executeJavaScript(`document.getElementById('status').textContent = ${JSON.stringify(text)};`)
    .catch(() => {
      // Swallow errors — splash may have been destroyed between the check and execution
    });
}

/**
 * Switch the splash to fill-progress mode and update the fill level (0–100).
 * The icon transitions from desaturated (base) to full-color bottom-to-top.
 * No-op if the splash window doesn't exist or has been destroyed.
 */
export function showSplashProgress(percent: number): void {
  if (!splashWindow || splashWindow.isDestroyed()) return;

  const clamped = Math.min(100, Math.max(0, percent));
  splashWindow.webContents
    .executeJavaScript(
      `(function(){
        document.getElementById('state-pulse').style.display='none';
        document.getElementById('state-fill').style.display='block';
        document.getElementById('logo-fill').style.clipPath=
          'inset(${100 - clamped}% 0 0 0)';
      })();`
    )
    .catch(() => {
      // Swallow errors — splash may have been destroyed between the check and execution
    });
}

/**
 * Switch the splash to error state: red-tinted logo with shake animation and
 * red status text. Used when an update fails or a rollback is detected.
 * No-op if the splash window doesn't exist or has been destroyed.
 */
export function updateSplashError(message: string): void {
  if (!splashWindow || splashWindow.isDestroyed()) return;

  splashWindow.webContents
    .executeJavaScript(
      `(function(){
        // Switch back to pulse state so logo-error is always on the visible element
        document.getElementById('state-fill').style.display='none';
        document.getElementById('state-pulse').style.display='block';
        const logo = document.getElementById('logo-pulse');
        if (logo) logo.classList.add('logo-error');
        const status = document.getElementById('status');
        if (status) {
          status.textContent = ${JSON.stringify(message)};
          status.classList.add('status-error');
        }
      })();`
    )
    .catch(() => {
      // Swallow errors — splash may have been destroyed between the check and execution
    });
}

/**
 * Close and destroy the splash window, saving its current screen position first.
 * Safe to call multiple times (idempotent).
 */
export function closeSplash(): void {
  if (!splashWindow) return;
  savePosition();
  if (!splashWindow.isDestroyed()) {
    splashWindow.destroy();
  }
  splashWindow = null;
}
