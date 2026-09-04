// MUST be the first import: pins userData before tokenManager/machineId/updater
// capture app.getPath('userData') at module-load time. See pinUserDataPath.ts.
import './pinUserDataPath';
import { registerOpenExternalHandler } from './ipc/openExternal';
import { registerSaveImageHandler } from './ipc/saveImage';
import { registerSSOIPC } from './ipc/sso';
import { cancelActiveAppleFlow } from './oauth/apple/appleFlow';
import { cancelActiveGoogleFlow } from './oauth/google/googleFlow';
import { registerAttestationIpc } from './ipc/attestation';
import { registerWindowControlsIpc, getCachedClientBehavior } from './ipc/windowControls';
import { initTray, destroyTray, isTrayActive } from './tray';
import { registerVersionInfoIpc } from './ipc/versionInfo';
import { buildBrowserWindowConfig } from './browserWindowConfig';
import { PRODUCTION_API_BASE } from './apiBaseUrl';
import { revealLoadFailure } from './loadFailureVisibility';
import { loadWindowState, attachWindowState } from './windowState';
import { isWayland } from './waylandDetect';
import { deriveCloseAction, deriveMinimizeAction } from '../shared/clientBehavior';
import { SPA_FALLBACK_MESSAGE } from '../shared/spaIpcTypes';
import { isStableDesktopVersion } from '../shared/stableDesktopVersion';
import {
  app,
  autoUpdater as electronAutoUpdater,
  BrowserWindow,
  clipboard,
  desktopCapturer,
  dialog,
  ipcMain,
  nativeImage,
  net,
  powerMonitor,
  protocol,
  screen,
  session,
  shell,
  type IpcMainInvokeEvent,
} from 'electron';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import fs from 'node:fs';
import {
  storeRefreshToken,
  restoreRefreshToken,
  performRefresh,
  performLogout,
  clearTokens,
  clearTokensIfOwner,
  getCapabilities,
  storeE2EEKeys,
  storeE2EEKeysIfOwner,
  restoreE2EEKeys,
  getCredentialCustodyState,
  setProactiveRefreshCallback,
  onSystemResume,
  getCachedAccessToken,
  getApiBaseOrigin,
} from './tokenManager';
import { getMachineId } from './machineId';
import { normalizeSelfHostedUrl, probeSelfHostedServer } from './selfHostedProbe';
import { consumeCeremonyToken } from './selfHostedCeremonyBudget';
import {
  approvalTierForApiBase,
  beginPendingApproval,
  clearPendingApproval,
  commitSelfHostedApproval,
  isValidatedSelfHostedApiBase,
  loadApprovedSelfHostedOrigins,
} from './selfHostedProfile';
import { resolveForDisplay, type ResolveForDisplayResult } from './guardedRequest';
import type { EgressDecision } from './egressPolicy';
import {
  initAutoUpdater,
  stopAutoUpdater,
  setUpdateFeedUrl,
  checkForUpdates,
  forceCheckForUpdates,
  downloadUpdate,
  safeQuitAndInstall,
  getAllowPrerelease,
  setAllowPrerelease,
  getUpdateLogger,
  getUpdateLogPath,
} from './updater';
import { getBuildTag } from './buildInfo';
import {
  checkUpdateSentinel,
  finalizeUpdate,
  finalizeRollback,
  runDeferredCleanup,
  type SentinelResult,
} from './updateSafety';
import { resolveAppProtocolPath } from './appProtocol';
import {
  resolveSpaSource,
  isUnexpectedBundled,
  classifyFallbackReason,
  isTransientRemoteFailure,
  captureSpaHash,
  hashEntryHtml,
  SPA_NO_CACHE_LOAD_OPTIONS,
  type SpaLoadDecision,
} from './spaLoader';
import { CONFIG_TIMEOUT_MS, SPA_RETRY_DELAYS_MS } from './spaTiming';
import { handleCacheProtocolRequest } from './spaCache/cacheProtocol';
import { getLiveDir } from './spaCache/cacheStore';
import { populateCacheFromRemote } from './spaCache/populateCache';
import { resolveCachedSpa } from './spaCache/resolveCachedSpa';
import { SPA_CACHE_HOST, SPA_CACHE_SCHEME } from './spaCache/manifestSchema';
import { handleDidFailLoad, handleSpaRequestSelfHeal } from './spaSelfHealMainFrame';
import { buildRemotePipUrl, isValidPipOpenSender } from './pipUrl';
import { mintPipSessionToken } from './pipSession';
import {
  extractInviteDeepLinkFromArgv,
  normalizeInviteDeepLink,
  type DeepLinkKind,
} from './deepLink';
import {
  getRemoteSpaBaseDir,
  getRemoteSpaBaseUrl,
  getRemoteSpaUrl,
  getSpaHash,
  setRemoteSpaState,
} from './spaState';
import { isPermittedFrameUrl, requireTrustedSender } from './ipc/frameValidation';
import {
  IPC_CONTRACT_VERSION,
  type CredentialOwner,
  type SelfHostedProbeResult,
} from './ipcContract';
import { registerIpcHandlers as registerPermissionHandlers } from './permissionManager';
import { migrateUserData } from './userDataMigration';
import { showSplash, closeSplash, updateSplashError } from './splashWindow';
import { maybePromptMove } from './applicationsFolderGate';
import { cleanupSquirrelResidue } from './squirrelCleanup';

// One-time migration: consolidate any legacy userData tree into the pinned
// "ConcordVoice" dir (#1291). Runs after pinUserDataPath.ts has set the path,
// before any userData read below.
//
// Note: this runs before the single-instance lock (acquired later, in
// app.whenReady). A rare concurrent double-launch could race two migrations,
// but every fs op is guarded + idempotent and archives rather than deletes, and
// promoteLegacy rolls back a failed move — so the worst case is a caught warn,
// never data loss (#1314 review, Gitar). Acquiring the lock this early would
// reorder unrelated startup and isn't worth the risk.
migrateUserData();

// Hydrate the in-memory validated-origin set from the durable approval store (#2354).
// Trust is minted only by the native ceremony below; this replays prior decisions.
loadApprovedSelfHostedOrigins();

// Hardware acceleration preference — must be checked before app.whenReady()
const hwAccelPrefPath = path.join(app.getPath('userData'), 'hw-accel.json');
function readHwAccelPref(): boolean {
  try {
    const data = JSON.parse(fs.readFileSync(hwAccelPrefPath, 'utf-8'));
    return data.enabled !== false;
  } catch {
    return true; // Default: enabled
  }
}

const contentProtectionPrefPath = path.join(app.getPath('userData'), 'content-protection.json');
const contentProtectionPrefTempPath = `${contentProtectionPrefPath}.${process.pid}.tmp`;

type ContentProtectionPreference =
  { enabled: boolean } | { enabled: boolean; previousEnabled: boolean; staged: true };

function isContentProtectionPreference(value: unknown): value is ContentProtectionPreference {
  if (typeof value !== 'object' || value === null) return false;
  const record = value as Record<string, unknown>;
  if (record.enabled === true || record.enabled === false) {
    const keys = Object.keys(record);
    if (keys.length === 1 && keys[0] === 'enabled') return true;
    return (
      record.staged === true &&
      (record.previousEnabled === true || record.previousEnabled === false) &&
      keys.length === 3 &&
      keys.includes('enabled') &&
      keys.includes('previousEnabled') &&
      keys.includes('staged')
    );
  }
  return false;
}

function readContentProtectionPref(): boolean {
  try {
    const data: unknown = JSON.parse(fs.readFileSync(contentProtectionPrefPath, 'utf-8'));
    if (!isContentProtectionPreference(data)) {
      // A file that parses but does not match the shape is the same event as an
      // unreadable one — an enabled control silently going OFF — and it was the
      // one path out of this function that still said nothing. No value is
      // logged: the rejected object is attacker-adjacent user-writable content.
      console.warn('[ContentProtection] preference has an unrecognized shape — treating as OFF');
      return false;
    }
    return 'staged' in data && data.staged ? data.previousEnabled : data.enabled;
  } catch (error) {
    // ENOENT is the opt-in default: the preference was never set, and OFF is
    // correct. Anything else — corrupt JSON, EACCES, a torn read — is a privacy
    // control the user turned ON being silently turned OFF. Same return value,
    // very different event, so only the second one is worth saying out loud.
    //
    // Log the errno CODE, never the message: Node builds fs error messages as
    // `EACCES: permission denied, open '<full path>'`, and this path is under
    // app.getPath('userData') — which on macOS and Windows contains the OS
    // account name. The code carries every bit of the diagnostic value and none
    // of the identity. (#2990 review, Codex P2.)
    const code = (error as NodeJS.ErrnoException | null)?.code;
    if (code !== 'ENOENT') {
      console.warn(
        '[ContentProtection] preference unreadable — treating as OFF:',
        code ?? 'unknown'
      );
    }
    return false;
  }
}

function writeContentProtectionPref(preference: ContentProtectionPreference): boolean {
  try {
    fs.writeFileSync(contentProtectionPrefTempPath, JSON.stringify(preference), 'utf-8');
    fs.renameSync(contentProtectionPrefTempPath, contentProtectionPrefPath);
    return true;
  } catch {
    try {
      fs.unlinkSync(contentProtectionPrefTempPath);
    } catch {
      // The failed temp file never becomes the preference authority.
    }
    return false;
  }
}

// BrowserWindow.setContentProtection is macOS/Windows only; everywhere else the
// call is a silent no-op that neither throws nor protects. Main owns this truth
// so no caller is ever told a protection applied that did not. The renderer also
// hides the toggle off-platform, but a UI gate is not the last line — the remote
// SPA reaches this bridge directly, and an older build of it does not know.
function contentProtectionSupported(): boolean {
  return process.platform === 'darwin' || process.platform === 'win32';
}

// Applying at window creation cannot fail loudly — refusing to open the window
// would be worse than an unprotected one. But the two directions are not
// symmetric: failing to apply `false` leaves a window over-protected and
// visible, while failing to apply `true` leaves it UNPROTECTED while the stored
// preference says otherwise. Only the second is a privacy downgrade.
function applyContentProtectionAtCreation(
  window: BrowserWindow,
  enabled: boolean,
  label: string
): void {
  if (!contentProtectionSupported()) return;
  try {
    window.setContentProtection(enabled);
  } catch (error) {
    if (enabled) {
      console.error(
        `[ContentProtection] ${label} is UNPROTECTED — preference is ON but apply failed:`,
        (error as Error).message
      );
    } else {
      console.warn(
        `[ContentProtection] failed to clear protection on ${label}:`,
        (error as Error).message
      );
    }
  }
}

function getContentProtectionWindows(): BrowserWindow[] {
  const windows = mainWindow ? [mainWindow] : [];
  for (const { window } of pipWindows.values()) windows.push(window);
  return windows.filter((window) => !window.isDestroyed());
}

// Developer Mode preference (#TBD) — gates DevTools access in packaged builds.
// MUST be removed before BETA release per security review.
const devModePrefPath = path.join(app.getPath('userData'), 'developer-mode.json');
function readDeveloperModePref(): boolean {
  try {
    const data = JSON.parse(fs.readFileSync(devModePrefPath, 'utf-8'));
    return data.enabled === true;
  } catch {
    return false;
  }
}
function writeDeveloperModePref(enabled: boolean): void {
  try {
    fs.writeFileSync(devModePrefPath, JSON.stringify({ enabled }), 'utf-8');
  } catch (err) {
    console.error('[DeveloperMode] Failed to persist preference:', (err as Error).message);
  }
}

if (readHwAccelPref()) {
  // Hardware acceleration flags for video encode/decode (IGNIS insight: fastest preset wins)
  app.commandLine.appendSwitch(
    'enable-features',
    'AcceleratedVideoEncoder,AcceleratedVideoDecodeLinuxGL,WebRtcAV1HWEncode'
  );
} else {
  app.disableHardwareAcceleration();
}

// Allow autoplay for all media — Concord is a real-time communication app that
// dynamically creates <audio>/<video> elements for voice/video calls.  The default
// 'document-user-activation-required' policy can silently block these elements.
app.commandLine.appendSwitch('autoplay-policy', 'no-user-gesture-required');

// Handle creating/removing shortcuts on Windows when installing/uninstalling
if (require('electron-squirrel-startup')) {
  app.quit();
}

// app:// scheme registration (#830) — the bundled-fallback renderer loads
// from app://concord/index.html instead of file:// so it has a non-null
// Origin header that the server's CORS allowlist can match. Must run
// BEFORE app.whenReady per Electron API contract. Only registered for
// packaged builds; dev mode uses the existing Vite + file fallback.
if (app.isPackaged) {
  protocol.registerSchemesAsPrivileged([
    {
      scheme: 'app',
      privileges: {
        standard: true,
        secure: true,
        supportFetchAPI: true,
        corsEnabled: true,
      },
    },
    // spa-cache:// scheme (#1870) — serves the signed last-known-good SPA cache
    // from a DEDICATED privileged origin, distinct from app://concord (bundled).
    // Same privileges as app:// (non-null Origin for server CORS, secure context
    // for WebCrypto/ServiceWorker, fetch + CORS). Packaged-only: dev uses Vite.
    {
      scheme: SPA_CACHE_SCHEME,
      privileges: {
        standard: true,
        secure: true,
        supportFetchAPI: true,
        corsEnabled: true,
      },
    },
  ]);
}

let mainWindow: BrowserWindow | null = null;
// Two INDEPENDENT readiness flags, never one shared flag: a pre-22 SPA signals
// only 'invite:renderer-ready', and a shared flag would make main believe the
// friend subscription is live when it is not (#945).
let inviteRendererReady = false;
let friendRendererReady = false;

type PendingDeepLink = { kind: DeepLinkKind; code: string };

const DEEP_LINK_KINDS: readonly DeepLinkKind[] = ['invite', 'friend'];

// Bounded because every argv/open-url deep link arriving before renderer-ready
// appends here, and #945 doubles the write sites. Drop-oldest: a stale queued
// code is worth less than the one the user just clicked.
//
// One queue PER KIND, never one shared array. Both kinds arrive from the same
// untrusted surfaces but wait on independent readiness flags, so on a shared
// queue a burst of friend codes spends the invite allowance: the invite the
// user actually clicked during cold start is evicted and silently never
// delivered. Same reasoning the friend arm already applies at the edge, where
// its WAF rule carries its own ref and counter.
export const PENDING_DEEP_LINK_CAP = 8;
const pendingDeepLinks: Record<DeepLinkKind, string[]> = { invite: [], friend: [] };

// Delivery after renderer-ready is otherwise unbounded: a page the user merely
// visits can fire N concord:// links and get N IPC sends, N forced modal
// re-opens, and N authenticated preview calls — each modal prefilled with an
// attacker-chosen code, one click from sending. emitDeepLink is the single
// choke point every delivery crosses (live and drained, invite and friend), so
// the bound lives here and neither renderer arm has to defend itself. Per kind:
//   - the first delivery goes out immediately — a single click must feel instant;
//   - anything arriving inside the window is HELD in a bounded FIFO and released
//     one per window edge. A different code is deferred, never dropped, so a user
//     who clicks invite A then B then C lands on all three in order. A repeat of
//     the code already showing (or already queued behind) collapses to nothing,
//     because re-sending it would change nothing it displays.
//
// The FIFO replaced a single newest-wins slot (#945, M0): three codes inside one
// window lost the middle one with no log and no trace, and a queue DRAIN of
// eight delivered #1 and #8 while silently discarding six user actions the
// 8-deep queue exists to preserve — while both this comment and spec §6a
// asserted the opposite, and §6a used that invariant as the stated reason a hard
// per-session cap was rejected. Bounded, so it cannot grow without limit; an
// overflow drops the OLDEST and SAYS SO, which is what "never silently" means.
// Short on purpose: long enough to collapse a burst, short enough that
// re-clicking the same link after closing its modal still opens it again.
export const DEEP_LINK_EMIT_WINDOW_MS = 1_000;

type DeepLinkEmitGate = {
  /** Code last sent — what this renderer lifecycle is currently showing. */
  lastCode: string | null;
  /** When it was sent; 0 means nothing sent yet this renderer lifecycle. */
  lastAt: number;
  /**
   * When the code currently in flight was FIRST delivered to any document, on the
   * MONOTONIC clock (`performance.now()`), not wall time. `Date.now()` moves under
   * NTP correction, resume-time synchronisation and manual clock changes: a
   * backward jump kept a dismissed invite eligible past the window, and a forward
   * jump during cold-start recovery made a FRESH invite look expired and
   * reproduced the delivery loss this fence exists to prevent (CODEX P2). The
   * asymmetry decides the trade-off — a monotonic clock may not advance across
   * system SUSPEND on every platform, so a laptop closed mid-window can resume
   * with the code still eligible; that is the F3 direction, which still needs a
   * main-owned swap and an explicit Join click, whereas the wall-clock failure
   * silently discards a live invite. 0
   * means nothing is in flight. Distinct from `lastAt` — which is wall time, used
   * only by the 1-second emit window — and which EVERY delivery refreshes — a carry's replay included. Measuring the recency fence off
   * `lastAt` made it a SLIDING window: each carry reset the clock, so a
   * dismissed code stayed eligible forever as long as main-initiated swaps
   * kept landing inside the window (CODEX P2).
   */
  originAt: number;
  /**
   * The code `resetDeepLinkDelivery` carried and that is awaiting its replay;
   * null otherwise. It is what tells `sendDeepLink` that an incoming code is a
   * REPLAY (clock keeps running) rather than a fresh send (clock restarts) —
   * see there. NOT cleared unconditionally on reset: it survives a reset that
   * carries nothing while its code is still in `pendingDeepLinks`, and is cleared
   * once that code has drained or been evicted. An unconditional clear at reset
   * start is exactly the defect that erased the marker for a still-queued code,
   * which then drained as a fresh delivery and restarted its own window.
   */
  carriedCode: string | null;
  /**
   * The code exempt from `PENDING_DEEP_LINK_CAP` eviction, until it is delivered.
   *
   * SEPARATE from `carriedCode` on purpose (CODEX P2). They started as one field
   * and answer different questions: `carriedCode` means "this queued entry is a
   * REPLAY", which drives the clock preservation and the drain-time expiry;
   * `reservedCode` means "this entry must not be the eviction victim". A
   * deliberate re-click retires the replay marker — it is a fresh delivery, not an
   * echo — but it must KEEP the reservation, or the fresh click becomes the oldest
   * unexempt entry and the next link evicts it. One field could not express both,
   * and collapsing them loses whichever property is not being thought about.
   */
  reservedCode: string | null;
  /** Codes held back by the window, oldest first; one is released per edge. */
  held: string[];
  timer: ReturnType<typeof setTimeout> | null;
};

/** Matches the per-kind pending-queue depth: the gate must not be the narrower bound. */
export const DEEP_LINK_HELD_MAX = 8;

const deepLinkEmitGates: Record<DeepLinkKind, DeepLinkEmitGate> = {
  invite: {
    lastCode: null,
    lastAt: 0,
    originAt: 0,
    carriedCode: null,
    reservedCode: null,
    held: [],
    timer: null,
  },
  friend: {
    lastCode: null,
    lastAt: 0,
    originAt: 0,
    carriedCode: null,
    reservedCode: null,
    held: [],
    timer: null,
  },
};

// A COUNT, not a boolean (#2363). Two main-owned swaps can overlap — a
// `spa:reloadLatest` click can land while a background `scheduleSpaSourceRetry`
// is still awaiting, and `applySpaDecision` itself navigates twice on its
// remote->cache->bundled fallback. A boolean is spent by the FIRST reset inside
// that await, so main's own navigation carried nothing and #2363 reproduced
// silently, fail-open. The count is owned by applySpaDecisionCarryingDeepLink:
// incremented on entry, released in its `finally`.
//
// The EPOCH is what makes the pairing structural rather than absorbed.
// forgetDeliveredDeepLinks zeroes the depth while release closures from
// still-in-flight swaps are outstanding; without the epoch a PRE-logout token
// firing after a LATER swap armed would disarm that swap mid-await (A,B arm ->
// logout -> A releases -> C arms -> B's stale release zeroes C). Fail-closed —
// a repair is lost, nothing leaks — but a `Math.max` clamp there would absorb
// broken pairing instead of preventing it, which is exactly the shape rejected
// when the boolean became a count.
let deepLinkCarryDepth = 0;
let deepLinkCarryEpoch = 0;

/**
 * How recent a DELIVERED code must be for the carry to re-deliver it
 * (RED-TEAM F3). The carry's own justification is "the modal is destroyed
 * before the user could act on it" — a RECENCY condition the original
 * implementation never encoded, so a code the user had already read and
 * DISMISSED was re-presented by every later main-initiated swap, including a
 * "Load latest UI" click minutes afterwards.
 *
 * DERIVED, not chosen (CODEX P1). A hand-picked 15 s was stated to cover the
 * retry schedule "with margin" and did not cover it at all: the delays are
 * SEQUENTIAL and each attempt first awaits resolveSpaSource, so the second
 * attempt's swap lands at 4 s + T + 10 s + T where T is the config timeout —
 * 24 s at worst and 14 s even when both fetches return instantly. The fence
 * then declined the carry on the exact cold-start recovery it exists to
 * repair, silently, presenting as the original bug. Deriving it means editing
 * either input cannot silently re-open that gap.
 *
 * The budget covers the WHOLE chain, not only the delays (CODEX P2, round 6).
 * `scheduleSpaSourceRetry` is first called AFTER the initial `applySpaDecision`
 * resolves, and each attempt schedules its successor only after its own
 * `applySpaDecision` resolves — and every `applySpaDecision` awaits a
 * `captureSpaHash` bounded by the same CONFIG_TIMEOUT_MS. So each attempt costs
 * up to TWO timeouts (one config fetch, one hash capture), plus one hash capture
 * before the chain starts.
 *
 * BEST-EFFORT CEILING, not a proof: `window.loadURL` inside `applySpaDecision`
 * has no deadline of its own, so a pathologically slow document load can still
 * push a swap past this fence. Every step that CAN be bounded is counted here;
 * the one that cannot is named rather than silently assumed to be fast.
 */
export const DEEP_LINK_CARRY_MAX_AGE_MS =
  SPA_RETRY_DELAYS_MS.reduce((total, delay) => total + delay, 0) +
  (2 * SPA_RETRY_DELAYS_MS.length + 1) * CONFIG_TIMEOUT_MS +
  5_000;

/**
 * Arm the carry for the next renderer-lifecycle reset (#2363): the code this
 * renderer has already been SENT is re-queued for its successor document
 * instead of being discarded. Only a navigation MAIN itself initiates may arm
 * it, and only via applySpaDecisionCarryingDeepLink, whose `finally` releases
 * it even when applySpaDecision resolves without navigating at all.
 *
 * Returns its OWN release function — idempotent, and inert once
 * forgetDeliveredDeepLinks has advanced the epoch (see `deepLinkCarryEpoch`).
 * Every caller, including tests, must release.
 */
export function carryDeepLinkAcrossNextLoad(): () => void {
  const epoch = deepLinkCarryEpoch;
  deepLinkCarryDepth += 1;
  let released = false;
  return () => {
    // A token from a superseded epoch has already been accounted for by the
    // forget that advanced it; decrementing again would disarm a later swap.
    if (released || epoch !== deepLinkCarryEpoch) return;
    released = true;
    deepLinkCarryDepth -= 1;
  };
}

// Both readiness flags drop together at every renderer-lifecycle boundary
// (window creation, reload, crash, close), and the emit gate resets with them:
// a fresh renderer displays nothing, so it must be able to receive the same
// code again and must not inherit the previous lifecycle's window.
//
// The timer and the held FIFO are cleared here too (#945, Md3). Leaving them
// live was not exploitable — the boundary is driven by navigation, close or
// crash, never by the external concord:// surface — but it was wrong in two
// visible ways: a stale timer SUPPRESSED the fresh lifecycle's immediate send
// (`gate.timer !== null` short-circuits the fast path), delaying a click by up
// to a second after any reload; and a code the previous renderer had already
// consumed could be re-delivered into the new one, re-opening the modal.
//
// Held codes are re-QUEUED rather than discarded: readiness is already false
// above, so each falls through emitDeepLink into the pending queue and the next
// drain hands it to the new renderer. Snapshot before queueing — queueDeepLink
// re-enters this module's state.
//
// A code that was already SENT is carried too, but ONLY when main itself
// initiated the navigation (#2363). Discarding it is right for an ordinary
// reload — the renderer consumed that code — and wrong for the two source swaps
// main performs on its own initiative (scheduleSpaSourceRetry, spa:reloadLatest),
// where the modal is destroyed before the user could act on it.
function resetDeepLinkDelivery(): void {
  // NO owner fence here, deliberately. It would be redundant: everything this
  // re-queues lands in pendingDeepLinks and meets the fence in
  // drainPendingDeepLinks before it can be delivered — measured by mutation,
  // dropping either one alone leaves the cross-owner test green and dropping both
  // turns it red. One fence per delivery path, not two per path.
  inviteRendererReady = false;
  friendRendererReady = false;
  // Read ONCE for the whole reset, not per kind. NOT cleared here (RED-TEAM
  // F2): the arming belongs to whichever applySpaDecisionCarryingDeepLink call
  // took it out and is released by that call's `finally`. Clearing it on the
  // first reset let an unrelated navigation inside the await spend it, so
  // main's own navigation carried nothing. Leaving it armed for the whole
  // window self-bounds — a second reset finds gate.lastCode === null unless a
  // genuine new send happened in between.
  //
  // "Read once" scopes the ARMING, not the number of codes: the loop below
  // applies this single reading to BOTH gates, so one navigation can re-deliver
  // up to two codes — one invite and one friend, each on its own channel. That
  // is intended (the kinds are independent and never share a queue or a gate);
  // an earlier wording said "one navigation is one carry", which read as a
  // one-code bound and was wrong.
  const carry = deepLinkCarryDepth > 0;
  for (const kind of DEEP_LINK_KINDS) resetDeepLinkGate(kind, carry);
}

/**
 * The per-kind half of `resetDeepLinkDelivery`, lifted out to keep that function
 * under the cognitive-complexity limit. `carry` is read ONCE by the caller and
 * passed in, so both gates see the same reading — see there for why.
 */
function resetDeepLinkGate(kind: DeepLinkKind, carry: boolean): void {
  const gate = deepLinkEmitGates[kind];
  const delivered = gate.lastCode;
  // RED-TEAM F3: only a code the user has NOT had a fair chance to act on.
  // Measured from the FIRST delivery, never from the last (CODEX P2) — see
  // `originAt`.
  const deliveredAge = performance.now() - gate.originAt;
  // TWO fences stop emitDeepLink's same-code collapse (`entry.code ===
  // previous`) from swallowing the carried code, and NEITHER is covered by a
  // test — reordering this block stays green — so keep both:
  //   1. readiness is already false above, so the queueDeepLink calls below
  //      cannot reach the window logic at all; the code lands in the pending
  //      queue and the next drain emits it;
  //   2. the gate is cleared BEFORE anything is re-queued, so a caller that
  //      did reach the window sees `previous === null` and an unbounded
  //      `waited`.
  // A collapse here is silent — no send, no error, no log — and presents
  // exactly as the bug this carry repairs.
  gate.lastCode = null;
  gate.lastAt = 0;
  // NOT cleared unconditionally. A SECOND reset before the renderer signals
  // ready — two main-owned swaps overlapping, which is the case the depth count
  // exists for — sees `lastCode === null` because the FIRST reset cleared it,
  // so it does not carry, and an unconditional clear here erased the marker and
  // the clock for a code still sitting in the pending queue. That code then
  // drained as a "fresh" delivery and restarted its own window, defeating the
  // recency bound exactly as the two earlier versions of it did (CODEX P2).
  // The marker is cleared below only when its code is no longer pending.
  if (gate.timer !== null) {
    clearTimeout(gate.timer);
    gate.timer = null;
  }
  const carried = gate.held;
  gate.held = [];
  // The delivered code predates everything in `held`, so it re-queues first:
  // oldest-first preserves the FIFO ordering documented above.
  const carrying = carry && delivered !== null && deliveredAge <= DEEP_LINK_CARRY_MAX_AGE_MS;
  // A carried code awaiting its replay keeps the clock and the marker alive
  // across resets that carry nothing. Once it is gone from the queue — drained,
  // or evicted at PENDING_DEEP_LINK_CAP — the marker must go too, or a much
  // later genuine delivery of the same code would inherit a long-expired clock.
  const carriedStillPending =
    gate.carriedCode !== null && pendingDeepLinks[kind].includes(gate.carriedCode);
  // Tracked SEPARATELY from carriedStillPending, because the two fields have
  // different lifetimes and tying them was the bug (CODEX P2). queueDeepLink
  // retires `carriedCode` on a re-click while deliberately KEEPING
  // `reservedCode` — the entry is still one queued occurrence and still needs
  // its eviction exemption. An intervening reset then found carriedStillPending
  // false *because carriedCode was null*, and cleared the reservation of a code
  // that was still queued; eight later links could evict the user's own fresh
  // re-click as the oldest unreserved entry. A reservation ends when ITS code
  // leaves the queue, not when some other marker is retired.
  const reservedStillPending =
    gate.reservedCode !== null && pendingDeepLinks[kind].includes(gate.reservedCode);
  if (carrying) {
    gate.carriedCode = delivered;
    gate.reservedCode = delivered;
    queueDeepLink({ kind, code: delivered });
    // RED-TEAM F1: the carried code is the one the user ACTED ON, so it must
    // not be the eviction victim. queueDeepLink drops the OLDEST at
    // PENDING_DEEP_LINK_CAP and the delivered code is re-queued FIRST, which
    // made it the guaranteed victim whenever a hostile page had filled `held`
    // to DEEP_LINK_HELD_MAX — the carry destroying exactly the code it exists
    // to preserve, and handing the user eight attacker-chosen prefilled join
    // modals in its place. Trim the OLDEST HELD codes instead: same
    // drop-oldest policy, applied to the codes the user did not act on.
    const room = PENDING_DEEP_LINK_CAP - pendingDeepLinks[kind].length;
    if (carried.length > room) {
      const dropped = carried.length - room;
      carried.splice(0, dropped);
      // Never silently, and never the code itself — bearer material
      // ([internal]rules/observability.md). Scope: this reports dropped HELD
      // codes only. queueDeepLink's own drop-oldest at PENDING_DEEP_LINK_CAP
      // (below) stays silent, so a full pending queue can still shed one
      // without a line here — reachable only if the renderer never drained it.
      console.warn(
        `Deep-link carry preserved the delivered code for kind=${kind}; dropped ${dropped} older held code(s)`
      );
    }
  } else if (!carriedStillPending) {
    // No carry armed, nothing delivered, or the age fence declining — and no
    // earlier carry still in flight. This code's life ends here, so the next
    // delivery starts a fresh clock instead of inheriting a stale one and
    // being refused on arrival.
    gate.originAt = 0;
    gate.carriedCode = null;
    if (!reservedStillPending) gate.reservedCode = null;
  }
  for (const code of carried) queueDeepLink({ kind, code });
}

// #945 M4 ("a held code does not outlive its session") extended from the
// renderer into main: the carry above can re-deliver a code AFTER the renderer's
// accessToken -> null clear has already run, so main must forget it at the same
// edge. Without this, user A's invite is re-delivered into user B's session by
// the renderer-invokable spa:reloadLatest — see spec
// [internal]specs/2026-08-27-2363-server-invite-repair-design.md §5.2.
// Silent by construction: codes are bearer material, so nothing here logs one.
//
// EVERYTHING still deliverable goes, not just the carry: the delivered code, the
// hold FIFO and its pending flush, and the pending queue. A code sitting in
// gate.held at logout is the same cross-account replay bounded to the <=1s until
// flushDeepLinkGate releases it, and gate.lastAt is a previous session's
// timestamp — so after this the gates are indistinguishable from a fresh
// process. Idempotent by construction, so the logout paths that reach it may
// overlap freely.
//
// Reached from `auth:logout` and `auth:clearTokens` — the two owner-blind
// credential-ending handlers — and nowhere else. NOT from
// `auth:clearTokensIfOwner`, whose callers are all login-side (see there), and
// NOT from a `rememberMe` refresh failure, which runs gracefulReset() WITHOUT
// electron.clearTokens() (apiClient.ts) and so deliberately keeps deliverable
// codes for the remembered session to resume with. The claim is "every LOGOUT
// path forgets", never "every credential wipe forgets".
// The credential owner that was current when main last accepted a deliverable
// code, or null when none has arrived under a credential. `credentialGeneration`
// (which this reads through getCredentialCustodyState) is stable across a
// session's own token refreshes and advances only when a credential is MINTED or
// CLEARED — so it means "which account", not "which token", which is exactly the
// question here.
let deepLinkOwner: CredentialOwner | null = null;

// The main-side half of the teardown fence, and the half that does not care what
// the renderer is. `deeplink:forget` (v25) needs an SPA new enough to call it,
// and spaLoader loads an older one indefinitely by design; this reads the
// credential store every SPA must go through regardless of contract version.
//
// Fires ONLY on a different NON-NULL owner, and both halves of that are
// load-bearing carve-outs documented in [internal]rules/electron.md:
//
//   tag null    — the code was queued before anyone signed in. It belongs to
//                 nobody, and handing it to the account that signs in next IS
//                 the feature (click invite → sign in → join).
//   current null— credentials are gone but no owner has replaced them: an
//                 aborted SSO, a cancelled account-link, an interrupted email
//                 verification. Those are login-side and must NOT forget, or the
//                 retry lands with the invite silently missing.
//
// Called at the three delivery entry points rather than inside emitDeepLink,
// because drainPendingDeepLinks takes a `queue.splice(0)` snapshot first: a
// forget from inside the loop empties the live queue while the snapshot it is
// iterating survives, and every retained code delivers anyway.
function fenceDeepLinksOnOwnerChange(atArrival = false, known?: CredentialOwner | null): void {
  // `known` lets a caller that has ALREADY read custody pass its reading in.
  // auth:restoreSession is the one that must: it re-verifies the owner across an
  // await for its own reasons, and an extra read here would both be redundant and
  // change how many times it observes custody.
  const current = known === undefined ? getCredentialCustodyState().credentialOwner : known;
  if (deepLinkOwner === null || current === deepLinkOwner) return;
  // `current === null` means credentials are gone with no successor. At a DRAIN
  // or FLUSH that is the login-side carve-out above and the queue must survive.
  // At an ARRIVAL it is separable without per-entry ownership, and separating it
  // is required: on a pre-v25 SPA the teardown leaves A's tag in place, a link
  // clicked while logged out cannot clear it (noteDeepLinkOwner refuses to write
  // null, deliberately), so the fresh code joined A's queue under A's tag and the
  // next owner change swept the link the user had just opened (CODEX P2).
  //
  // Arrival is the boundary that makes them distinguishable: everything ALREADY
  // queued belongs to the retained tag, and the arriving code does not.
  if (current === null && !atArrival) return;
  forgetDeliveredDeepLinks();
}

// Claim the codes now in flight for whoever is signed in. Called at OS arrival
// AND at delivery: arrival binds a code that will sit in the queue, delivery
// spends the pre-login carve-out on the account that actually receives it.
// Deliberately does not clear the tag when logged out — a pre-login code must
// stay null-tagged so the carve-out above still applies to it at sign-in, and
// resetting it would extend that exemption to a retained code (see the test).
function noteDeepLinkOwner(known?: CredentialOwner | null): void {
  const current = known === undefined ? getCredentialCustodyState().credentialOwner : known;
  if (current !== null) deepLinkOwner = current;
}

function forgetDeliveredDeepLinks(): void {
  deepLinkOwner = null;
  // Advance BEFORE zeroing: outstanding release tokens are now stale and must
  // no longer decrement (see carryDeepLinkAcrossNextLoad).
  deepLinkCarryEpoch += 1;
  deepLinkCarryDepth = 0;
  for (const kind of DEEP_LINK_KINDS) {
    const gate = deepLinkEmitGates[kind];
    gate.lastCode = null;
    gate.lastAt = 0;
    gate.originAt = 0;
    gate.carriedCode = null;
    gate.reservedCode = null;
    gate.held = [];
    if (gate.timer !== null) {
      clearTimeout(gate.timer);
      gate.timer = null;
    }
    pendingDeepLinks[kind].length = 0;
  }
}

function isRendererReadyFor(kind: DeepLinkKind): boolean {
  return kind === 'friend' ? friendRendererReady : inviteRendererReady;
}

function registerInviteProtocolClient(): void {
  if (!app.isPackaged) return;
  if (typeof app.setAsDefaultProtocolClient !== 'function') return;
  try {
    if (process.platform === 'win32') {
      app.setAsDefaultProtocolClient('concord', process.execPath, []);
    } else {
      app.setAsDefaultProtocolClient('concord');
    }
  } catch {
    console.warn('[DeepLink] protocol registration failed');
  }
}

// Friend codes ride their own channel; 'invite:received' keeps emitting a bare
// { code } forever. spaLoader refuses an SPA NEWER than the shell but loads an
// older one indefinitely, and SPA_IPC_CONTRACT is an operator-set, hot-reloadable
// var — so a contract-22 shell can host a 19-era SPA with no bound. A widened
// { kind, code } payload would reach that SPA, which ignores the unknown field
// and opens the SERVER-join modal with a friend code. On a second channel the
// same SPA simply never subscribes and the deep link no-ops (#945, spec X1).
function sendDeepLink(kind: DeepLinkKind, code: string): void {
  // Bind the code to whoever is RECEIVING it, not only to whoever was signed in
  // when it arrived. A code clicked before sign-in arrives ownerless on purpose
  // — that carve-out is the click-invite-then-sign-in feature — but tagging it
  // only at arrival left it ownerless FOREVER, so the exemption outlived the
  // delivery that spent it and the primary flow produced a code any later
  // account could still be handed (CODEX P2). No-op when nobody is signed in.
  noteDeepLinkOwner();
  const gate = deepLinkEmitGates[kind];
  // A carry REPLAYS a code whose origin clock must keep running; every OTHER
  // delivery starts a fresh one (CODEX P2, twice). The replay is identified by
  // `carriedCode`, which resetDeepLinkDelivery set for exactly the code it
  // carried — not by "same code as last time", which was the first attempt and
  // was wrong in a way that mattered: a user who dismisses an invite and then
  // DELIBERATELY re-opens the same link is making a fresh delivery, and
  // inheriting the old clock meant their new modal was already stale and would
  // not be carried across a swap.
  // Only when THIS code is the reserved one, mirroring the carriedCode check
  // below. Clearing unconditionally meant a different code draining first — the
  // reserved code plus a newer click both queued, readiness flipping — released
  // the reservation before the reserved code was delivered, reopening the
  // eviction window F1 and the round-11 dedupe exist to close (Gitar).
  if (code === gate.reservedCode) gate.reservedCode = null;
  if (code === gate.carriedCode) {
    gate.carriedCode = null;
  } else {
    // Monotonic, deliberately — see `originAt`.
    gate.originAt = performance.now();
  }
  gate.lastCode = code;
  gate.lastAt = Date.now();
  if (kind === 'friend') {
    mainWindow?.webContents.send('deeplink:friend-code', { code });
    return;
  }
  mainWindow?.webContents.send('invite:received', { code });
}

/** Window edge: deliver whatever the window held back, newest only. */
function flushDeepLinkGate(kind: DeepLinkKind): void {
  fenceDeepLinksOnOwnerChange();
  const gate = deepLinkEmitGates[kind];
  gate.timer = null;
  const code = gate.held.shift();
  if (code === undefined) return;
  // Re-arm BEFORE any early return below, so a collapsed repeat or a departed
  // renderer cannot strand the rest of the FIFO unreleased.
  if (gate.held.length > 0) {
    gate.timer = setTimeout(() => flushDeepLinkGate(kind), DEEP_LINK_EMIT_WINDOW_MS);
  }
  if (code === gate.lastCode) return;
  if (!mainWindow || mainWindow.isDestroyed() || !isRendererReadyFor(kind)) {
    // The renderer went away inside the window — hand the code back to its
    // queue instead of dropping it; the next drain delivers it.
    queueDeepLink({ kind, code });
    return;
  }
  sendDeepLink(kind, code);
}

/** True once the entry is accounted for: sent, or held for the window edge. */
function emitDeepLink(entry: PendingDeepLink): boolean {
  if (!mainWindow || mainWindow.isDestroyed()) return false;
  if (!isRendererReadyFor(entry.kind)) return false;

  const gate = deepLinkEmitGates[entry.kind];
  const waited = Date.now() - gate.lastAt;
  if (gate.timer !== null || waited < DEEP_LINK_EMIT_WINDOW_MS) {
    // Collapse only an immediate repeat — of the code showing now, or of the one
    // already queued last. A DIFFERENT code always takes its own slot.
    const previous = gate.held.length > 0 ? gate.held.at(-1) : gate.lastCode;
    if (entry.code !== previous) {
      if (gate.held.length >= DEEP_LINK_HELD_MAX) {
        // Oldest out, and never silently: kind and reason only, never the code —
        // it is bearer material (see [internal]rules/observability.md).
        gate.held.shift();
        console.warn(
          `Deep-link hold buffer full for kind=${entry.kind}; dropped the oldest held code`
        );
      }
      gate.held.push(entry.code);
    }
    gate.timer ??= setTimeout(
      () => flushDeepLinkGate(entry.kind),
      DEEP_LINK_EMIT_WINDOW_MS - waited
    );
    return true;
  }
  sendDeepLink(entry.kind, entry.code);
  return true;
}

function queueDeepLink(entry: PendingDeepLink): void {
  if (emitDeepLink(entry)) return;
  const queue = pendingDeepLinks[entry.kind];
  // Collapse duplicates (CODEX P2). A second copy of a code already waiting adds
  // nothing — it opens the same modal twice — and it multiplies the carried
  // code's reservation below into as many exemptions as there are copies. A page
  // that knows the carried code (its own, if it fired the link that got carried)
  // could then fill every slot with duplicates, making any later LEGITIMATE link
  // the only non-reserved entry and therefore the immediate eviction victim.
  // With duplicates collapsed, at most one entry can ever match, so the
  // reservation is exactly one occurrence by construction.
  if (queue.includes(entry.code)) {
    // ...with ONE exception: a duplicate of the CARRIED code is not a duplicate.
    // The queued copy is a REPLAY awaiting delivery; this is the user deliberately
    // opening the same link again. Dropping it silently and leaving the replay in
    // place means the drain-time expiry can then discard the stale copy and the
    // fresh click produces no modal at all — the two fences cancelling each other
    // (CODEX P2). Retire the replay's identity and restart the clock instead: the
    // single queue entry stays, but it is now a fresh delivery with its own window.
    const gate = deepLinkEmitGates[entry.kind];
    if (entry.code === gate.carriedCode) {
      gate.carriedCode = null;
      gate.originAt = performance.now();
      // `reservedCode` deliberately SURVIVES. Retiring only the replay marker is
      // what makes this a fresh delivery; retiring the reservation too would leave
      // the fresh click as the oldest UNEXEMPT entry, and the next different link
      // would evict it (CODEX P2). Moving it to the FIFO tail does not fix that —
      // at this moment it is usually the queue's only entry, so the move is a
      // no-op and the later links still append after it.
    }
    return;
  }
  queue.push(entry.code);
  // Drop-oldest at the cap, EXCEPT the carried code (RED-TEAM F1, second queue).
  // F1 fixed this for the hold buffer and left the pending queue open: the
  // carried code is re-queued FIRST, so it is the oldest entry and therefore the
  // guaranteed eviction victim once eight links arrive while the successor
  // document is still loading — a burst any page can fire, silently replacing
  // the invite the swap existed to preserve with eight of the attacker's own.
  // The reservation lasts until its first successor delivery, which is exactly
  // when sendDeepLink clears carriedCode.
  const carried = deepLinkEmitGates[entry.kind].reservedCode;
  while (queue.length > PENDING_DEEP_LINK_CAP) {
    const victim = queue.findIndex((code) => code !== carried);
    // Every entry is the carried code (it can only be queued once, so this means
    // the cap itself is 0 or the queue is a single reserved entry): drop the
    // oldest rather than growing without bound.
    queue.splice(victim === -1 ? 0 : victim, 1);
  }
}

// Must FILTER, not blind-re-queue. This runs from three sites, and with per-kind
// readiness flags a blind re-push of an unemittable entry re-enters the queue
// within the same tick — forever.
function drainPendingDeepLinks(): void {
  // Before the splice below, never inside the loop — see fenceDeepLinksOnOwnerChange.
  fenceDeepLinksOnOwnerChange();
  for (const kind of DEEP_LINK_KINDS) {
    const gate = deepLinkEmitGates[kind];
    const queue = pendingDeepLinks[kind];
    for (const code of queue.splice(0)) {
      // Re-check the CARRIED code's age here, not only where it was queued
      // (CODEX P2). Queue residence is unbounded: the successor renderer signals
      // readiness when it is ready, and a stalled initialisation can leave a
      // carried code sitting here long past DEEP_LINK_CARRY_MAX_AGE_MS. Treating
      // the marker as standing proof of freshness let a dismissed invite reappear
      // hours later — the fence passing once and then never being asked again.
      // Only the carried code is re-checked: an ordinary queued code is one the
      // user just clicked and has never been shown.
      if (
        code === gate.carriedCode &&
        performance.now() - gate.originAt > DEEP_LINK_CARRY_MAX_AGE_MS
      ) {
        gate.carriedCode = null;
        gate.originAt = 0;
        continue;
      }
      if (!emitDeepLink({ kind, code })) queue.push(code);
    }
  }
}

// Only result.reason is logged. Invite and friend codes are bearer material and
// never reach a log line.
function handleInviteDeepLink(raw: string | undefined, source: string): void {
  const result = normalizeInviteDeepLink(raw);
  if (result.ok) {
    // The only place a code ENTERS main. queueDeepLink is not it — resetDeepLinkGate
    // and flushDeepLinkGate both re-queue through it, and claiming ownership on a
    // re-queue launders a retained code into whoever is signed in now.
    fenceDeepLinksOnOwnerChange(true);
    noteDeepLinkOwner();
    queueDeepLink({ kind: result.kind, code: result.code });
    return;
  }
  if (result.reason !== 'empty') {
    console.warn('[DeepLink] rejected deep link', 'source', source, 'reason', result.reason);
  }
}

function handleInviteDeepLinksFromArgv(argv: readonly string[] | undefined, source: string): void {
  const result = extractInviteDeepLinkFromArgv(argv);
  if (result.ok) {
    // The SECOND OS entry point, and it needs the same treatment as
    // handleInviteDeepLink: on Windows and Linux a protocol activation against a
    // running app arrives here via `second-instance`, not via `open-url`. Without
    // the fence, a new owner's own click is queued under the PREVIOUS owner's tag
    // and the drain fence then forgets the whole queue — including the link that
    // user just opened (CODEX P2). The startup `process.argv` caller reaches this
    // too and is unaffected: at cold start both the tag and the current owner are
    // null, so each of these is a no-op there.
    fenceDeepLinksOnOwnerChange(true);
    // Redundant TODAY -- sendDeepLink's bind already covers a code that gets
    // delivered, and dropping this line alone leaves the suite green. It stays
    // for symmetry with the other OS entry point: an asymmetry here reads as a
    // deliberate exemption, and it is the only thing that would keep argv bound
    // if sendDeepLink's bind were ever narrowed.
    noteDeepLinkOwner();
    queueDeepLink({ kind: result.kind, code: result.code });
  } else if (result.reason !== 'empty') {
    console.warn('[DeepLink] rejected deep link argv', 'source', source, 'reason', result.reason);
  }
}

// Set true by the before-quit handler so the [X] close intercept can
// distinguish a genuine app quit (⌘Q, Dock→Quit, window:quit/app:quit, the
// updater relaunch) from the user clicking the window close button. Without
// this guard the intercept's preventDefault() cancels Electron's app.quit()
// sequence and the app becomes unquittable — force-quit only (#1383).
let isQuitting = false;
// SPA-load state lives in ./spaState so spaSelfHeal.ts can mutate it on
// fallback/recovery paths. See that module for the lockstep invariant and
// reader/writer enumeration.

/**
 * Probe the Vite dev server, then load it. Falls back to the bundled
 * renderer on any failure (timeout, ECONNREFUSED, etc.). Extracted from
 * createWindow to keep cognitive complexity in check.
 */
async function loadDevRendererWithFallback(
  window: BrowserWindow,
  bundledPath: string
): Promise<void> {
  const devServerUrl = 'http://localhost:3001';
  const http = await import('node:http');
  const httpGet = (http.default?.get ?? http.get) as typeof import('node:http').get;
  try {
    await new Promise<void>((resolve, reject) => {
      const req = httpGet(devServerUrl, { timeout: 1000 }, (res) => {
        res.resume();
        resolve();
      });
      req.on('error', reject);
      req.on('timeout', () => {
        req.destroy();
        reject(new Error('timeout'));
      });
    });
    await window.loadURL(devServerUrl);
  } catch {
    await window.loadFile(bundledPath);
  }
}

/** Effective SPA load outcome: remote, the signed LKG cache (#1870), or bundled. */
type SpaLoadOutcome = 'remote' | 'cache' | 'bundled';

/**
 * Load the terminal bundled `app://concord` renderer. #830: app:// gives the
 * renderer a non-null Origin for server CORS. Bundled is the terminal load
 * layer, so a failure here leaves a blank window with no further fallback —
 * surface it to the splash error overlay. Always clears remote-SPA state first.
 */
async function loadBundledFallback(window: BrowserWindow): Promise<'bundled'> {
  // Clear state so PiP and origin-only consumers see a consistent "not in
  // remote-SPA mode" signal even on re-entry.
  setRemoteSpaState(null);
  try {
    await window.loadURL('app://concord/index.html');
    // Covers genuine bundled mode AND the remote→bundled fallback, so the
    // effective mode is always reflected accurately. Best-effort: never throws.
    await captureSpaHash('bundled');
  } catch (err) {
    console.error('[SpaLoader] bundled app:// loadURL failed:', (err as Error).message);
    revealLoadFailure(window, 'Could not load application — please reinstall');
  }
  return 'bundled';
}

/**
 * Attempt to serve the signed last-known-good cache (#1870). Returns 'cache'
 * iff a valid live cache verified AND the cache URL loaded; otherwise null so
 * the caller falls through to the bundled path. The cache is a LOCAL privileged
 * origin (spa-cache://concord), NOT a remote SPA — so we clear remote-SPA state
 * and treat it like bundled for origin/attestation purposes (captureSpaHash
 * fetches the cache's index.html via the protocol handler, which serves it).
 */
async function tryCacheLoad(window: BrowserWindow): Promise<'cache' | null> {
  const cached = resolveCachedSpa();
  if (!cached) {
    return null;
  }
  try {
    setRemoteSpaState(null);
    await window.loadURL(cached.url);
    await captureSpaHash('bundled');
    console.debug('[SpaLoader] served signed last-known-good cache');
    return 'cache';
  } catch (err) {
    console.warn('[SpaLoader] cache load failed, falling back to bundled:', (err as Error).message);
    return null;
  }
}

/**
 * Apply a resolved SPA decision to a live window. Sets the remote-SPA state
 * BEFORE loadURL — load-bearing so the will-navigate gate, PiP, openExternal,
 * SSO, and versionInfo origin consumers key on the ACTUAL loaded origin — then
 * navigates and best-effort-captures the entry HTML hash.
 *
 * Fallback order on a remote failure:
 *   1. signed LKG cache (#1870) — ONLY when the remote `loadURL` threw, or the
 *      decision is bundled for a TRANSIENT remote reason (config fetch failed /
 *      5xx). NOT for expected-bundled reasons (no apiBase / no spaUrl / contract
 *      zero) or an IPC-mismatch — those must not be masked by a stale cache.
 *   2. bundled app://concord (terminal).
 *
 * On a successful remote load, fires a best-effort cache refresh (fire-and-
 * forget; never awaited, never throws into the load path).
 *
 * Shared by the launch path (loadPackagedRenderer) and the runtime
 * `spa:reloadLatest` handler so the two can never drift. Returns the effective
 * mode actually loaded.
 */
async function applySpaDecision(
  window: BrowserWindow,
  decision: SpaLoadDecision
): Promise<SpaLoadOutcome> {
  if (decision.mode === 'remote' && decision.url) {
    try {
      setRemoteSpaState(decision.url);
      await window.loadURL(decision.url, SPA_NO_CACHE_LOAD_OPTIONS);
      // Capture the entry HTML hash for client attestation (#677). Best-effort:
      // captureSpaHash never throws, so this cannot break the load path.
      await captureSpaHash('remote', decision.url);
      // #1870: best-effort refresh the signed LKG cache from the live origin.
      // Fire-and-forget — must NOT delay the visible load; swallow rejections.
      try {
        const origin = new URL(decision.url).origin + '/';
        void populateCacheFromRemote(origin).catch(() => {});
      } catch {
        // Malformed decision.url — already loaded fine; just skip the refresh.
      }
      return 'remote';
    } catch {
      console.warn('[SpaLoader] Failed to load remote SPA, trying cache then bundled');
      setRemoteSpaState(null);
      // The remote loadURL threw — a genuine remote failure, so the cache may
      // bridge the gap (#1870).
      const cacheMode = await tryCacheLoad(window);
      if (cacheMode) {
        return cacheMode;
      }
      return loadBundledFallback(window);
    }
  }

  // Bundled decision. Consult the signed cache ONLY for a transient remote
  // failure (config fetch failed / 5xx) — never for expected-bundled reasons or
  // an IPC-mismatch (those must not be masked by a stale cache).
  if (isTransientRemoteFailure(decision.reason)) {
    const cacheMode = await tryCacheLoad(window);
    if (cacheMode) {
      return cacheMode;
    }
  }

  return loadBundledFallback(window);
}

// ponytail: a wrapper rather than an exported arm/disarm pair — a caller that
// armed the flag and then returned early would re-deliver a stale code on the
// next unrelated load. The `finally` makes that unreachable at every PRODUCTION
// call site; carryDeepLinkAcrossNextLoad is exported for the tests, so an
// importer can still write the unbalanced form and must release itself. Covers
// the paths where applySpaDecision resolves without navigating at all. Fail-closed
// by construction: forgetting to disarm is not expressible at a call site.
async function applySpaDecisionCarryingDeepLink(
  window: BrowserWindow,
  decision: SpaLoadDecision
): Promise<SpaLoadOutcome> {
  const releaseCarry = carryDeepLinkAcrossNextLoad();
  try {
    return await applySpaDecision(window, decision);
  } finally {
    releaseCarry();
  }
}

// #1742 follow-up: the SPA-source decision is made once at launch with a 5s
// config-fetch timeout (spaLoader CONFIG_TIMEOUT_MS) and no retry. A cold-start
// network (DNS/TLS/CF edge not yet warm) loses that race and strands the client
// on the bundled SPA for the whole session. When the bundled fallback was
// UNEXPECTED (config fetch failed — not a logged-out / no-spaUrl / contract
// case), retry resolveSpaSource a few seconds later, on a now-warm network, and
// switch to the remote SPA if it resolves. Bounded; stops on success or once
// the delays are exhausted. The manual "Load latest UI" button (spa:reloadLatest)
// remains the fallback. Mirrors the WebSocket onlineFallbackTimer pattern (#1768).
// (SPA_RETRY_DELAYS_MS is declared beside DEEP_LINK_CARRY_MAX_AGE_MS, which is
// derived from it.)

function scheduleSpaSourceRetry(window: BrowserWindow, attempt = 0): void {
  if (attempt >= SPA_RETRY_DELAYS_MS.length) return;
  setTimeout(() => {
    void (async () => {
      // Abort if the window is gone, or if we are already in remote mode (a
      // prior retry or a manual reload succeeded — getRemoteSpaUrl is non-null
      // only when the remote SPA is loaded).
      if (window.isDestroyed() || getRemoteSpaUrl() !== null) return;
      const decision = await resolveSpaSource();
      if (decision.mode === 'remote' && decision.url) {
        console.debug('[SpaLoader/retry] remote SPA reachable on warm network — switching');
        // Use the EFFECTIVE mode: resolveSpaSource can return remote (config
        // host reachable) while the remote SPA host itself is still down, in
        // which case applySpaDecision falls back to bundled. Only stop retrying
        // when we actually reached remote; otherwise keep retrying.
        // Main initiated this swap, so a code already delivered to the outgoing
        // document is carried into its successor (#2363).
        const mode = await applySpaDecisionCarryingDeepLink(window, decision);
        if (mode === 'remote') return;
      }
      scheduleSpaSourceRetry(window, attempt + 1);
    })();
  }, SPA_RETRY_DELAYS_MS[attempt]);
}

/**
 * Load the packaged renderer: try the remote SPA first (Tier 3), fall back
 * to the bundled file on any failure. Extracted to keep createWindow simple.
 */
async function loadPackagedRenderer(window: BrowserWindow, _bundledPath: string): Promise<void> {
  const decision = await resolveSpaSource();
  console.debug(`[SpaLoader] ${decision.mode}: ${decision.reason}`);
  await applySpaDecision(window, decision);

  // #830 Option C: surface a non-blocking diagnostic event if the bundled
  // fallback fired for an unexpected reason (config fetch failed, network
  // issue, spaUrl rejected, etc.). Expected fallbacks (first launch, no
  // spaUrl, contract zero) do NOT trigger it.
  // Delay 2000ms to ensure renderer listeners are registered.
  //
  // The payload carries a CLASS, not just prose (#2401). Six distinct reasons
  // reach this branch and only one of them ("config fetch failed" / 5xx) means
  // the servers were unreachable — the rest fire against a perfectly reachable
  // server that sent something this client refused, or that this shell is too
  // old for. Sending one hardcoded "Could not reach Concord servers" for all
  // six was wrong on five of them, and left the renderer unable to tell which
  // it had received — so it could not know whether proof of reachability
  // falsified the claim. The class decides both the copy and whether the
  // banner may be retracted; see SpaFallbackKind in shared/spaIpcTypes.ts.
  if (decision.mode === 'bundled' && isUnexpectedBundled(decision.reason)) {
    const kind = classifyFallbackReason(decision.reason);
    setTimeout(() => {
      // Guard against window destroyed during the 2s delay (rapid quit, crash).
      if (window.isDestroyed()) return;
      window.webContents.send('app:configFetchFailed', {
        reason: SPA_FALLBACK_MESSAGE[kind],
        kind,
      });
    }, 2000);
    // Self-heal the cold-start race: retry on a warm network and switch to
    // remote if it becomes reachable (#1742 follow-up launch-time retry).
    scheduleSpaSourceRetry(window);
  }
}

const createWindow = async (): Promise<void> => {
  // Build the per-platform BrowserWindow config (#806). Pure factory so
  // each platform gets the correct titleBarStyle / titleBarOverlay shape:
  //   - darwin → hiddenInset (preserves traffic-light controls)
  //   - win32 / linux → hidden + titleBarOverlay (native min/max/close)
  const baseConfig = buildBrowserWindowConfig({
    platform: process.platform,
    isWayland: isWayland(),
    preloadPath: path.join(__dirname, '../preload/preload.js'),
    isPackaged: app.isPackaged,
  });

  // Restore the user's last window placement (#806). loadWindowState returns
  // the saved bounds if window-state.json exists and the validator accepts
  // them; otherwise defaults to centered (x/y undefined → OS default placement).
  const savedState = loadWindowState();
  const contentProtectionEnabled = readContentProtectionPref();

  mainWindow = new BrowserWindow({
    ...baseConfig,
    x: savedState.x,
    y: savedState.y,
    width: savedState.width,
    height: savedState.height,
  });
  applyContentProtectionAtCreation(mainWindow, contentProtectionEnabled, 'main window');
  resetDeepLinkDelivery();

  if (savedState.isMaximized) {
    mainWindow.maximize();
  }

  mainWindow.webContents.on('did-start-loading', () => {
    resetDeepLinkDelivery();
  });

  // Wire resize/move/maximize/unmaximize/close listeners that persist
  // bounds to window-state.json with 500ms debounce (#806).
  attachWindowState(mainWindow);

  // Register the window-control + version-info IPC surfaces (#806). The
  // factory closures defer the window lookup so the late-bind window
  // reference is always current (handles the brief teardown window).
  registerWindowControlsIpc(() => mainWindow);
  registerVersionInfoIpc(() => mainWindow);

  const bundledPath = path.join(__dirname, '../renderer/index.html');

  // Register before loading any URL — ready-to-show fires during loadURL
  // and the listener must be attached first to avoid a race condition where
  // the event fires before the handler is registered (window never shows).
  mainWindow.once('ready-to-show', () => {
    closeSplash();
    mainWindow?.show();
  });

  if (app.isPackaged) {
    await loadPackagedRenderer(mainWindow, bundledPath);
  } else {
    await loadDevRendererWithFallback(mainWindow, bundledPath);
    // Always open DevTools in dev mode regardless of load path
    if (process.env.DEVTOOLS !== '0') {
      mainWindow.webContents.openDevTools();
    }
  }

  // Open DevTools at startup if Developer Mode preference is enabled.
  // Applies to packaged builds (bundled or remote SPA) and to dev fallback.
  if (readDeveloperModePref() && !mainWindow.webContents.isDevToolsOpened()) {
    mainWindow.webContents.openDevTools({ mode: 'detach' });
  }

  // Cmd/Ctrl+Opt+I toggles DevTools when Developer Mode is enabled.
  // (TEMPORARY — remove before BETA along with Developer Mode feature.)
  mainWindow.webContents.on('before-input-event', (_event, input) => {
    if (input.type !== 'keyDown') return;
    const isToggleCombo =
      input.key.toLowerCase() === 'i' && input.alt && (input.meta || input.control);
    if (!isToggleCombo) return;
    if (!readDeveloperModePref()) return;
    if (mainWindow?.webContents.isDevToolsOpened()) {
      mainWindow.webContents.closeDevTools();
    } else {
      mainWindow?.webContents.openDevTools({ mode: 'detach' });
    }
  });

  // Open external links in browser. https-only after #754 tightening —
  // see [internal]specs/2026-04-26-754-externalize-blocked-nav-design.md
  // and [internal]rules/electron.md "External-link scheme policy" for the
  // threat-model rationale (passive nav is held to a stricter scheme set
  // than the user-clicked Markdown-link IPC path).
  mainWindow.webContents.setWindowOpenHandler(({ url }) => {
    try {
      const parsed = new URL(url);
      if (parsed.protocol === 'https:') {
        // Fire-and-forget — main process can't surface a result here. .catch
        // suppresses unhandled-rejection if the OS denies (sandbox, no handler).
        // Symmetric with the IPC handler at src/main/ipc/openExternal.ts.
        shell.openExternal(url).catch(() => {});
      }
      // http: intentionally rejected — Electron app externalization is
      // https-only. Markdown-rendered chat links keep http: via
      // the open-external IPC handler (different consent model).
    } catch {
      // Invalid URL — ignore (no externalization, deny in-app open).
    }
    return { action: 'deny' };
  });

  // Client Behavior [X] close intercept (#806). When the user clicks the
  // native close button, route based on the cached Client Behavior:
  //   - 'tray'    -> hide() (stays running, accessed from system tray, #1099)
  //   - 'toolbar' -> minimize() (stays running in the OS taskbar)
  //   - 'quit'    -> app.quit() (graceful full-app shutdown)
  // event.preventDefault() suppresses the default close -> destroy path so the
  // hide/minimize fall-throughs can take over. The 'quit' branch also calls
  // preventDefault so app.quit can run its before-quit hooks without racing
  // a window-destroy mid-shutdown.
  //
  // isQuitting guard (#1383): once a genuine quit is underway, before-quit has
  // already fired and set isQuitting = true. We MUST let this close proceed
  // (no preventDefault) or we veto Electron's own app.quit() sequence and the
  // app becomes unquittable. This also resolves the 'quit' branch's
  // re-entrancy: app.quit() -> before-quit sets the flag -> re-fired close ->
  // early return here -> window destroys -> quit completes.
  //
  // Trayless fallback (#1099): if action='tray' but the tray failed to
  // initialize (sandboxed env, missing StatusNotifier host, etc.),
  // mainWindow.hide() would strand an invisible, unrecoverable resident
  // process on Windows/Linux (macOS still has Dock activate). Quit instead so
  // a trayless session degrades to a recoverable state. window-all-closed's
  // !isTrayActive() branch alone is not enough: preventDefault()+hide() means
  // the window never actually closes, so window-all-closed never fires.
  mainWindow.on('close', (event) => {
    if (!mainWindow) return;
    if (isQuitting) return;
    const action = deriveCloseAction(getCachedClientBehavior());

    if (action === 'tray') {
      event.preventDefault();
      if (isTrayActive()) {
        mainWindow.hide();
      } else {
        app.quit();
      }
    } else if (action === 'toolbar') {
      event.preventDefault();
      mainWindow.minimize();
    } else {
      // action === 'quit'
      event.preventDefault();
      app.quit();
    }
  });

  // Client Behavior [-] minimize intercept (#806). When the user clicks the
  // native minimize button AND their Client Behavior config routes minimize
  // to the system tray, redirect the post-fact minimize to hide(). Brief
  // flicker is accepted per spec §3.2 trade-off; minimize is a post-fact
  // event, not interceptible-before-the-OS-acts the way close is.
  //
  // Trayless fallback (#1099): if tray init failed, calling restore()+hide()
  // would strand the window invisibly with no taskbar entry. Skip the
  // redirect so the native minimize stands and the window stays recoverable
  // from the taskbar.
  mainWindow.on('minimize', () => {
    if (!mainWindow) return;
    const action = deriveMinimizeAction(getCachedClientBehavior());
    if (action === 'tray' && isTrayActive()) {
      mainWindow.restore();
      mainWindow.hide();
    }
    // else: leave native minimize -> toolbar as-is
  });

  mainWindow.on('closed', () => {
    // #974/#975: a window-less SSO flow (apple or google) has no UI to deliver
    // its result to — tear both down so the loopback listener and 5-minute
    // deadline don't outlive the renderer (teardown trigger (b) in each flow's
    // documented lifecycle).
    cancelActiveAppleFlow();
    cancelActiveGoogleFlow();
    mainWindow = null;
    resetDeepLinkDelivery();
  });

  // Log renderer crashes to diagnose voice join segfaults
  mainWindow.webContents.on('render-process-gone', (_event, details) => {
    resetDeepLinkDelivery();
    console.error('[MAIN] Renderer process gone:', details.reason, 'exitCode:', details.exitCode);
  });
};

// Track rollback result so we can notify the renderer after window creation
let rollbackResult: SentinelResult | null = null;

// App lifecycle handlers
app.whenReady().then(async () => {
  if (maybePromptMove()) return;

  registerInviteProtocolClient();

  // app:// protocol handler (#830) — serves the bundled SPA from the asar
  // bundle root. The pure resolver in appProtocol.ts validates the URL,
  // rejects path-traversal, and returns the absolute file path. Here we
  // wrap that with net.fetch which handles asar bundle paths transparently.
  if (app.isPackaged) {
    const bundleRoot = path.resolve(__dirname, '../renderer');
    protocol.handle('app', async (request) => {
      const result = resolveAppProtocolPath(request.url, bundleRoot);
      if (!result.ok || !result.absolutePath) {
        return new Response(null, { status: result.status });
      }
      // pathToFileURL is the canonical cross-platform-safe path→file://
      // transformation. String concat (`'file://' + absolutePath`) works
      // on macOS/Linux but produces malformed URLs on Windows where
      // path.resolve emits drive-letter paths like C:\app\...
      return net.fetch(pathToFileURL(result.absolutePath).href);
    });

    // spa-cache:// protocol handler (#1870) — serves the VERIFIED last-known-good
    // SPA cache from the live cache dir. The pure resolver in cacheProtocol.ts
    // mirrors appProtocol.ts's path-traversal rejection; getLiveDir resolves the
    // pinned userData path lazily at request time.
    protocol.handle(SPA_CACHE_SCHEME, (request) => handleCacheProtocolRequest(request, getLiveDir));
  }

  registerOpenExternalHandler(getRemoteSpaBaseUrl);
  // #1729 — native Save-As for decrypted image attachments. Lazy window provider
  // (mirrors registerPermissionHandlers) so the dialog parents to the live window.
  registerSaveImageHandler(() => mainWindow, getRemoteSpaBaseUrl);
  registerSSOIPC(getRemoteSpaBaseUrl);
  registerAttestationIpc(getRemoteSpaBaseUrl);
  // Permission request handler: explicitly allow app-required permissions, deny risky ones.
  // Notifications are allowed (JIT-managed by permissionManager #197).
  session.defaultSession.setPermissionRequestHandler((_webContents, permission, callback) => {
    const denied = ['geolocation'];
    if (denied.includes(permission)) {
      callback(false);
      return;
    }
    callback(true);
  });

  // Permission check handler: Chromium queries this synchronously to verify grants.
  // Default to true so internal checks (e.g. WebAuthn/FIDO2) aren't silently blocked.
  session.defaultSession.setPermissionCheckHandler((_webContents, permission) => {
    const denied = ['geolocation'];
    return !denied.includes(permission);
  });

  // Device permission handler: allow camera, microphone, speaker, and HID (security keys)
  session.defaultSession.setDevicePermissionHandler((details) => {
    return ['camera', 'microphone', 'speaker', 'hid'].includes(details.deviceType ?? '');
  });

  // NO TLS certificate pinning on api.concordvoice.chat — deliberate, do not
  // re-add (#658 reverted). Cloudflare serves that host from a MANAGED edge
  // certificate that it rotates on its own schedule with a NEW keypair. The pin
  // design assumed Concord uploaded its own origin keypair via Cloudflare's
  // Custom SSL Certificates API; that endpoint is Business/Enterprise-only and
  // this account is on Pro, so the upload never ran even once. Every rotation
  // therefore locked the ENTIRE installed base out of the API with
  // net::ERR_FAILED, with no server-side remedy possible because the rejection
  // happens client-side before the request leaves the machine. That is five
  // fleet outages between 2026-04 and 2026-08, and zero detections.
  //
  // API TLS now uses Chromium's default validation (public CA + Certificate
  // Transparency) — the same anchor the browser SPA at spa.concordvoice.chat
  // and the deliberately-unpinned update feed (#2020) already rely on.
  // Mis-issuance is caught out-of-band by CT monitoring, which alerts instead
  // of bricking the fleet. Only ever pin a keypair Concord controls end to end.
  // See docs/policies/api-tls-trust-model.md.

  // HID device selection: auto-select for WebAuthn hardware security keys
  session.defaultSession.on('select-hid-device', (event, details, callback) => {
    event.preventDefault();
    if (details.deviceList && details.deviceList.length > 0) {
      callback(details.deviceList[0].deviceId);
    } else {
      callback('');
    }
  });

  // OS permission management (#197) — JIT prompting replaces proactive startup requests.
  // Camera/mic/screen/notifications are requested when the feature is first used.
  registerPermissionHandlers(() => mainWindow, getRemoteSpaBaseUrl);

  // KLIPY media proxy auth injection (#626): <img>/<video> src attributes
  // send plain GETs without Authorization headers. This interceptor injects
  // the cached JWT so the authenticated media proxy returns 200 instead of 401.
  // Origin-restricted: only injects when the request targets the known API base.
  session.defaultSession.webRequest.onBeforeSendHeaders(
    { urls: ['*://*/api/v1/klipy/media*'] },
    (details, callback) => {
      const token = getCachedAccessToken();
      const apiOrigin = getApiBaseOrigin();
      if (token && apiOrigin) {
        try {
          const requestOrigin = new URL(details.url).origin;
          if (requestOrigin === apiOrigin) {
            details.requestHeaders['Authorization'] = `Bearer ${token}`;
            const clientVersion = app.getVersion();
            if (isStableDesktopVersion(clientVersion)) {
              details.requestHeaders['X-Concord-Client-Version'] = clientVersion;
            }
          }
        } catch {
          // Malformed URL — skip injection
        }
      }
      callback({ requestHeaders: details.requestHeaders });
    }
  );

  // ─── Update safety: startup validation (#384) ─────────────────────
  // Must run before createWindow() so we can stash rollback info,
  // but after initAutoUpdater() so the logger is available.
  initAutoUpdater(
    () => mainWindow,
    () => {
      isQuitting = true;
    }
  );

  const updateLogger = getUpdateLogger();
  if (updateLogger) {
    // Check for deferred cleanup from a previous incomplete finalization
    runDeferredCleanup(updateLogger).catch(() => {});

    const sentinelResult = checkUpdateSentinel(updateLogger);
    if (sentinelResult.type === 'success') {
      updateLogger.info(
        `Update validated: v${sentinelResult.fromVersion} → v${sentinelResult.toVersion}`
      );
      finalizeUpdate(updateLogger, sentinelResult).catch((err) => {
        updateLogger.error(`Post-update cleanup failed: ${(err as Error).message}`);
      });
    } else if (sentinelResult.type === 'rollback') {
      updateLogger.warn(
        `Update to v${sentinelResult.toVersion} failed, rolled back to v${sentinelResult.fromVersion}`
      );
      finalizeRollback(updateLogger, sentinelResult).catch((err) => {
        updateLogger.error(`Rollback cleanup failed: ${(err as Error).message}`);
      });
      rollbackResult = sentinelResult;
    }
  }

  // Show branded splash while the main window loads (#387)
  if (app.isPackaged) {
    const iconPath = path.join(app.getAppPath(), 'build', 'icon.png');
    const icon = nativeImage.createFromPath(iconPath);
    showSplash(icon.isEmpty() ? undefined : icon.toDataURL());

    // If a rollback was detected, show error state on the splash while main window loads
    if (rollbackResult) {
      updateSplashError(
        `Update to v${rollbackResult.toVersion} failed — rolled back to v${rollbackResult.fromVersion}`
      );
    }
  }

  void createWindow().then(() => {
    drainPendingDeepLinks();
    setTimeout(drainPendingDeepLinks, 1000);

    // Remove orphaned Squirrel.Windows residue left by the NSIS migration (#2402).
    //
    // Runs from the INSTALLED APP on a later launch, never from the installer. An
    // installer executing from %LOCALAPPDATA%\ConcordVoice\pending\ cannot delete
    // %LOCALAPPDATA%\ConcordVoice\ — it would be deleting its own locked image,
    // which IS the bug this migration fixes (Squirrel died exactly that way with
    // UnauthorizedAccessException).
    //
    // Deliberately AFTER the window exists and deferred a further tick: the sweep is
    // SYNCHRONOUS recursive rmSync over app-* directories that can total hundreds of
    // MB, so running it during whenReady would block the main-process event loop
    // before first paint. It is allowlist-scoped, never throws, and never touches
    // ConcordVoice\app (the running install) or ConcordVoice\pending (the updater
    // cache). No-op on non-win32.
    //
    // app.isPackaged-gated: without it, a developer running `npm start` on a real
    // Windows machine would have their genuine %LOCALAPPDATA%\ConcordVoice profile
    // swept — Update.exe, packages\, SquirrelTemp\, the Start Menu folder and the
    // uninstall registry key deleted as a side effect of local development. Matches
    // the packaged-gating of the other startup side effects in this file.
    //
    // Reuses the logger initAutoUpdater() already created rather than calling
    // createUpdateLogger() again: each construction re-runs mkdirSync plus a full
    // readdirSync + pruneOldLogs sweep of the log directory (updateLogger.ts), so a
    // second one doubles that filesystem work on every launch.
    if (app.isPackaged) {
      const squirrelLogger = getUpdateLogger();
      if (squirrelLogger) {
        setTimeout(() => cleanupSquirrelResidue(squirrelLogger), 0);
      }
    }
  });

  // System tray (#1099): init after the first window exists so the activate
  // handler has a window to reveal. Init failure is non-fatal — the app runs
  // trayless and window-all-closed keeps its quit (see handler below).
  initTray({ getMainWindow: () => mainWindow, createWindow });

  // Notify renderer of rollback after window is created (#384)
  if (rollbackResult) {
    // Capture to local const so TypeScript narrows through the setTimeout closure.
    const rollback = rollbackResult;
    // Delay slightly to ensure renderer IPC listeners are registered
    setTimeout(() => {
      mainWindow?.webContents.send('update:rollback', {
        fromVersion: rollback.fromVersion,
        toVersion: rollback.toVersion,
        message: `Update to v${rollback.toVersion} failed. You are still on v${rollback.fromVersion}.`,
      });
    }, 2000);
  }

  // Proactive token refresh (#254): notify renderer when main process
  // refreshes the token (timer or sleep/wake), so authStore stays current.
  setProactiveRefreshCallback((accessToken, sessionId, previousSessionId) => {
    mainWindow?.webContents.send('auth:token-refreshed', {
      accessToken,
      sessionId,
      previousSessionId,
    });
  });

  // Refresh token immediately on system wake — main process timers may have
  // drifted past the token's expiry window during sleep (#248).
  powerMonitor.on('resume', () => {
    onSystemResume();
  });

  app.on('activate', () => {
    // On macOS, re-create window when dock icon is clicked
    if (BrowserWindow.getAllWindows().length === 0) {
      createWindow();
    }
  });
});

// Quit when all windows are closed — except on macOS (platform convention)
// and except when the tray is active: the tray is the persistent affordance
// to reopen or quit (#1099). If tray init FAILED, isTrayActive() is false and
// the pre-#1099 quit-on-all-closed behavior is preserved, so a trayless
// session can never strand an invisible resident process.
app.on('window-all-closed', () => {
  if (process.platform !== 'darwin' && !isTrayActive()) {
    app.quit();
  }
});

// autoUpdater.quitAndInstall() closes windows before the normal before-quit
// event, so release the close-to-tray veto at the update-specific hook too (#1897).
electronAutoUpdater.on('before-quit-for-update', () => {
  isQuitting = true;
  destroyPipWindowsForQuit();
});

// Clean up scheduled update checks on quit; flush update log (#383)
app.on('before-quit', () => {
  // Release the [X] close intercept's veto so the window can actually close and
  // the quit can complete — without this, app.quit() deadlocks (#1383).
  isQuitting = true;
  destroyPipWindowsForQuit();
  // Release the OS tray resource so no orphaned icon outlives the app (#1099).
  destroyTray();
  const ul = getUpdateLogger();
  ul?.info('Application quitting');
  ul?.flush();
  stopAutoUpdater();
});

// Crash-safe logging: flush update log before unhandled exceptions terminate the process (#383).
// Registering this listener disables Node's default crash behavior, so we must exit explicitly.
process.on('uncaughtException', (error) => {
  const ul = getUpdateLogger();
  ul?.error(`Uncaught exception: ${error.message}\n${error.stack ?? ''}`);
  ul?.flush();
  setImmediate(() => app.exit(1));
});

// IPC Handlers
ipcMain.handle('app:getVersion', () => {
  return app.getVersion();
});

ipcMain.handle('app:getPlatform', () => {
  return process.platform;
});

ipcMain.handle('app:getIpcContract', () => {
  return IPC_CONTRACT_VERSION;
});

// Forensic build-tag observability (#920 §5.13, #939). Returns the CI
// build tag baked into the packaged app (via forge extraResource
// buildtag.json) or 'unknown' for local dev builds. Read-only,
// information-only — knowing the tag does not unlock any capability.
ipcMain.handle('app:getBuildTag', () => {
  return getBuildTag();
});

// Screen capture sources for voice/video screen share picker (#44)
ipcMain.handle('media:getDesktopSources', async (event) => {
  if (!requireTrustedSender(event, getRemoteSpaBaseUrl())) return;
  const sources = await desktopCapturer.getSources({
    types: ['window', 'screen'],
    thumbnailSize: { width: 320, height: 180 },
  });
  return sources.map((s) => ({
    id: s.id,
    name: s.name,
    thumbnail: s.thumbnail.toDataURL(),
    appIcon: s.appIcon?.toDataURL() || null,
  }));
});

// Clipboard write (navigator.clipboard.writeText is blocked in Electron)
ipcMain.handle('clipboard:writeText', (event, text: string) => {
  if (!requireTrustedSender(event, getRemoteSpaBaseUrl())) return;
  clipboard.writeText(text);
});

// GPU info
const GPU_VENDORS: Record<number, string> = {
  0x106b: 'Apple',
  0x8086: 'Intel',
  0x10de: 'NVIDIA',
  0x1002: 'AMD',
  0x1022: 'AMD',
  0x5143: 'Qualcomm',
  0x13b5: 'ARM',
};

const VIDEO_CODEC_PROFILE_MIMES = new Map<number, string>([
  ...[0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10].map((profile) => [profile, 'video/H264'] as const),
  [11, 'video/VP8'],
  ...[12, 13, 14, 15].map((profile) => [profile, 'video/VP9'] as const),
  // Chromium 148 VideoCodecProfile: 19-22/27-28/37-38 are Dolby Vision, 23 is Theora.
  ...[16, 17, 18, 29, 30, 31, 32, 33, 34, 35, 36].map(
    (profile) => [profile, 'video/HEVC'] as const
  ),
  ...[24, 25, 26].map((profile) => [profile, 'video/AV1'] as const),
]);

function extractEncodeProfiles(info: unknown): string[] {
  if (!isRecord(info)) return [];
  const profiles = info.videoEncodeAcceleratorSupportedProfiles;
  if (profiles === undefined) return [];
  if (!Array.isArray(profiles)) {
    console.warn('[gpu:getInfo] unexpected videoEncodeAcceleratorSupportedProfiles shape');
    return [];
  }

  const mimes = new Set<string>();
  for (const entry of profiles) {
    const profile = isRecord(entry) ? entry.profile : entry;
    if (typeof profile !== 'number') continue;
    const mime = VIDEO_CODEC_PROFILE_MIMES.get(profile);
    if (mime) mimes.add(mime);
  }
  return [...mimes];
}

ipcMain.handle('gpu:getInfo', async () => {
  try {
    // Read-only GPU metadata: no filesystem/network/keychain/OS mutation, bounded
    // shape parsing below, so this fits [internal]rules/electron.md's low-stakes IPC exception.
    const info = await app.getGPUInfo('complete');
    const gpu = (
      info as {
        gpuDevice?: Array<{
          vendorId: number;
          deviceId: number;
          driverVendor?: string;
          driverDescription?: string;
        }>;
      }
    ).gpuDevice?.[0];
    if (gpu) {
      // driverVendor on macOS often contains the hex vendorId as a string — skip it
      const driverName =
        gpu.driverVendor && !/^0x[0-9a-f]+$/i.test(gpu.driverVendor) ? gpu.driverVendor : '';
      const vendor =
        driverName || GPU_VENDORS[gpu.vendorId] || `Unknown (0x${gpu.vendorId.toString(16)})`;
      const device =
        gpu.driverDescription || (gpu.deviceId ? `Device 0x${gpu.deviceId.toString(16)}` : '');
      return { vendor, device, encodeProfiles: extractEncodeProfiles(info) };
    }
    return null;
  } catch {
    return null;
  }
});

// Display info for resolution/refresh rate awareness
ipcMain.handle('screen:getDisplayInfo', () => {
  const displays = screen.getAllDisplays();
  const primary = screen.getPrimaryDisplay();
  return displays.map((d) => ({
    width: d.size.width * d.scaleFactor,
    height: d.size.height * d.scaleFactor,
    refreshRate: d.displayFrequency,
    scaleFactor: d.scaleFactor,
    isPrimary: d.id === primary.id,
    colorDepth: d.colorDepth, // 24 = SDR, 30/48 = HDR
    colorSpace: d.colorSpace, // "srgb" = SDR, "p3"/"rec2020" = wide gamut
  }));
});

// Hardware acceleration preference
ipcMain.handle('app:getHardwareAcceleration', () => readHwAccelPref());

ipcMain.handle('app:setHardwareAcceleration', (event, enabled: boolean) => {
  if (!requireTrustedSender(event, getRemoteSpaBaseUrl())) return;
  fs.writeFileSync(hwAccelPrefPath, JSON.stringify({ enabled }), 'utf-8');
});

ipcMain.handle('app:getContentProtection', async (event) => {
  if (!requireTrustedSender(event, getRemoteSpaBaseUrl())) return false;
  // Off-platform the effective protection is OFF whatever the file says, so
  // report the truth rather than a preference that cannot take effect.
  if (!contentProtectionSupported()) return false;
  return readContentProtectionPref();
});

ipcMain.handle('app:setContentProtection', async (event, enabled: unknown): Promise<boolean> => {
  if (!requireTrustedSender(event, getRemoteSpaBaseUrl()) || typeof enabled !== 'boolean') {
    return false;
  }
  // Reporting success for a no-op is the one outcome worse than refusing: it
  // tells the caller a screen-capture protection is in force when nothing is.
  if (!contentProtectionSupported()) return false;

  const previous = readContentProtectionPref();
  if (!writeContentProtectionPref({ enabled, previousEnabled: previous, staged: true })) {
    return false;
  }

  const updated: BrowserWindow[] = [];
  try {
    for (const window of getContentProtectionWindows()) {
      window.setContentProtection(enabled);
      updated.push(window);
    }
    if (!writeContentProtectionPref({ enabled })) throw new Error('content protection preference');
    return true;
  } catch {
    for (const window of updated) {
      try {
        window.setContentProtection(previous);
      } catch {
        // Preserve rollback attempts for every previously updated window.
      }
    }
    return false;
  }
});

// Relaunch (for hardware acceleration toggle)
ipcMain.handle('app:relaunch', (event) => {
  if (!requireTrustedSender(event, getRemoteSpaBaseUrl())) return;
  app.relaunch();
  app.quit();
});

// Hot SPA reload — main process re-runs the spaLoader safety chain and points
// the live window at the freshly-resolved remote SPA URL (or bundled on
// fallback). Powers the Settings ▸ About "Load latest UI" button: lets a client
// stranded on the bundled SPA (the cold-start config-fetch race) escape to
// remote WITHOUT an app restart. SECURITY: the renderer only TRIGGERS this; the
// URL is derived entirely in main from resolveSpaSource (getPersistedApiBase +
// the authenticated /api/v1/client/config fetch). NO URL is accepted from the
// renderer, so a compromised renderer cannot choose the origin. Reuses
// applySpaDecision so launch and runtime cannot drift.
ipcMain.handle('spa:reloadLatest', async (event) => {
  // Privileged: this reaches a top-frame navigation. Reject an untrusted sender
  // frame FIRST — before any window/packaged state check — so a compromised
  // frame is refused regardless of state. isPermittedFrameUrl accepts both
  // states this feature spans: app://concord (the stranded bundled state) and
  // the active remote origin.
  if (!isPermittedFrameUrl(event.senderFrame?.url ?? '', getRemoteSpaBaseUrl())) {
    console.warn('[SpaLoader/reload] rejected spa:reloadLatest from untrusted frame');
    return { mode: 'bundled', changed: false, rejected: true };
  }
  if (!mainWindow || !app.isPackaged) return { mode: 'bundled', changed: false };
  const before = getSpaHash();
  const decision = await resolveSpaSource();
  console.debug(`[SpaLoader/reload] ${decision.mode}: ${decision.reason}`);
  // Main initiated this swap (the renderer only TRIGGERED it), so a code already
  // delivered to the outgoing document is carried into its successor (#2363).
  const outcome = await applySpaDecisionCarryingDeepLink(mainWindow, decision);
  // Collapse the internal 'cache' outcome to 'bundled' for the renderer-facing
  // IPC contract (#1870): the signed LKG cache is a local fallback origin, not a
  // remote SPA, and no renderer consumer distinguishes it — checkForUpdate's
  // currentMode already reports 'bundled' when serving from cache (remote-SPA
  // state is null). Keeps the preload `mode: 'remote' | 'bundled'` type honest
  // without leaking the internal selection detail to the renderer.
  const mode: 'remote' | 'bundled' = outcome === 'remote' ? 'remote' : 'bundled';
  return { mode, changed: getSpaHash() !== before };
});

// SPA (UI) update check — the SECOND update axis, distinct from the
// electron-updater desktop-binary axis (update:*). Reports whether the renderer
// is on the bundled fallback vs remote, and whether NEWER remote-SPA bytes are
// live (a SHA-256 diff of the served index.html — /client/config exposes no SPA
// build id and the SPA URL is constant post-#976, so there is no version number
// to show, only "newer bytes available"). Read-only and best-effort (never
// throws); the network target is server-derived, not renderer-supplied.
ipcMain.handle('spa:checkForUpdate', async (event) => {
  // Read-only, but validate the sender frame for parity with spa:reloadLatest
  // and defense-in-depth — closes the (minor) redundant-self-fetch surface a
  // compromised/sandboxed frame could otherwise trigger.
  if (!isPermittedFrameUrl(event.senderFrame?.url ?? '', getRemoteSpaBaseUrl())) {
    return {
      currentMode: 'bundled',
      remoteAvailable: false,
      newerBytesAvailable: null,
      reason: 'rejected',
    };
  }
  if (!app.isPackaged) {
    return {
      currentMode: 'remote',
      remoteAvailable: false,
      newerBytesAvailable: null,
      reason: 'dev mode',
    };
  }
  const currentMode = getRemoteSpaUrl() === null ? 'bundled' : 'remote';
  const decision = await resolveSpaSource();
  const remoteAvailable = decision.mode === 'remote' && !!decision.url;
  let newerBytesAvailable: boolean | null = null;
  if (remoteAvailable && decision.url) {
    const available = await hashEntryHtml(decision.url);
    // null (re-fetch failed) → unknown; the UI degrades to an unconditional offer.
    newerBytesAvailable = available === null ? null : available !== getSpaHash();
  }
  return { currentMode, remoteAvailable, newerBytesAvailable, reason: decision.reason };
});

// Clean app exit (preserves persisted auth/remember-me state)
ipcMain.handle('app:quit', () => {
  app.quit();
});

// ─── Desktop Notification Helpers (#175) ────────────────────────────

ipcMain.handle('app:setBadgeCount', (_event, count: number) => {
  app.setBadgeCount(count);
});

ipcMain.handle('app:flashFrame', (_event, flash: boolean) => {
  mainWindow?.flashFrame(flash);
});

ipcMain.handle('app:focusWindow', () => {
  if (mainWindow) {
    if (mainWindow.isMinimized()) mainWindow.restore();
    mainWindow.focus();
  }
});

// ─── Auto-Update IPC ──────────────────────────────────────────────────

ipcMain.handle('update:check', (event) => {
  if (!requireTrustedSender(event, getRemoteSpaBaseUrl())) return;
  return checkForUpdates();
});
ipcMain.handle('update:download', (event) => {
  if (!requireTrustedSender(event, getRemoteSpaBaseUrl())) return;
  return downloadUpdate();
});
ipcMain.handle('update:install', (event) => {
  if (!requireTrustedSender(event, getRemoteSpaBaseUrl())) return;
  return safeQuitAndInstall();
});
ipcMain.handle('update:getAllowPrerelease', () => getAllowPrerelease());
ipcMain.handle('update:setAllowPrerelease', (event, enabled: boolean) => {
  if (!requireTrustedSender(event, getRemoteSpaBaseUrl())) return;
  return setAllowPrerelease(enabled);
});
ipcMain.handle('update:getLogPath', () => getUpdateLogPath());
ipcMain.handle(
  'updater:force-check',
  (event, reason: 'attestation_required' | 'user_triggered') => {
    if (!requireTrustedSender(event, getRemoteSpaBaseUrl())) return;
    return forceCheckForUpdates(reason);
  }
);

// Developer Mode (gates DevTools in packaged builds — REMOVE BEFORE BETA)
ipcMain.handle('app:getDeveloperMode', () => readDeveloperModePref());
ipcMain.handle('app:setDeveloperMode', (event, enabled: boolean) => {
  if (!requireTrustedSender(event, getRemoteSpaBaseUrl())) return;
  writeDeveloperModePref(enabled);
  if (!mainWindow) return;
  if (enabled) {
    if (!mainWindow.webContents.isDevToolsOpened()) {
      mainWindow.webContents.openDevTools({ mode: 'detach' });
    }
  } else if (mainWindow.webContents.isDevToolsOpened()) {
    mainWindow.webContents.closeDevTools();
  }
});

// System info for About page (#155 Tier 4)
ipcMain.handle('app:getSystemInfo', () => ({
  platform: process.platform,
  arch: process.arch,
  electronVersion: process.versions.electron,
  chromiumVersion: process.versions.chrome,
  nodeVersion: process.versions.node,
}));

// ─── Secure Auth Token Management (safeStorage) ──────────────────────

export interface ApprovalDialogParams {
  host: string;
  address: string;
  decision: EgressDecision;
}

function addressClassSuffix(decision: EgressDecision): string {
  if (decision.tier === 'tier2') {
    if (decision.reason === 'loopback') return 'on this device';
    if (decision.reason === 'cgnat') return "on your provider's network";
    return 'on your network'; // private / ula
  }
  return 'on the internet'; // public (tier1 never reaches the ceremony)
}

/** Exported for unit test. Copy is verbatim per spec §7.2; punycode is never decoded. */
export function buildApprovalDialogCopy(p: ApprovalDialogParams): {
  message: string;
  detail: string;
} {
  const suffix = addressClassSuffix(p.decision);
  const message = `Trust ${p.host}?`;
  const detail =
    `Host:         ${p.host}\n` +
    `Resolves to:  ${p.address}, ${suffix}\n\n` +
    `If you trust it, Concord Voice will store your sign-in on this device and ` +
    `use ${p.host} to sign you in from now on.\n\n` +
    `Concord Voice will remember this choice on this device.`;
  return { message, detail };
}

const defaultShowMessageBox = (w: unknown, o: unknown): Promise<{ response: number }> =>
  w
    ? dialog.showMessageBox(w as BrowserWindow, o as Electron.MessageBoxOptions)
    : dialog.showMessageBox(o as Electron.MessageBoxOptions);

/** Exported for unit test. showMessageBox is injected (the applicationsFolderGate pattern). */
export async function requestSelfHostedApproval(
  win: unknown,
  p: ApprovalDialogParams,
  {
    showMessageBox = defaultShowMessageBox,
  }: { showMessageBox?: (w: unknown, o: unknown) => Promise<{ response: number }> } = {}
): Promise<boolean> {
  const { message, detail } = buildApprovalDialogCopy(p);
  const { response } = await showMessageBox(win, {
    type: 'warning',
    title: 'Concord Voice',
    message,
    detail,
    buttons: ['Cancel', 'Trust This Server'],
    defaultId: 0,
    cancelId: 0,
    noLink: true,
  });
  return response === 1;
}

// One probe at a time per webContents (the set is keyed on event.sender.id, so the
// bound is PER-FRAME, not process-global). This is a REJECTING single-flight, not the
// promise-sharing kind used by restoreSessionPromise / spaSelfHeal: two concurrent
// probes of DIFFERENT urls must never share one result.
//
// It gates the whole handler because resolveForDisplay's dns.lookup is a
// libuv-threadpool job that cannot be cancelled. The default pool is 4, so N
// concurrent probes of unresolvable names queue N getaddrinfo jobs against those
// slots and head-of-line-block every async fs operation in the main process —
// credential IO and the approvals-file read included — with zero dialogs shown.
// The threadpool is process-global while this bound is not, so the protection rests
// on there being one auth window and the renderer having no way to mint another.
// A per-request timeout (resolveForDisplay's race) does not free the slot; only
// not starting the lookup does. ServerInput already disables Connect while busy,
// so the refused concurrency has no legitimate caller.
const probesInFlight = new Set<number>();

ipcMain.handle('selfHosted:probeServer', async (event, url: unknown) => {
  if (!isTrustedAuthSender(event)) {
    console.warn('[selfHosted:probeServer] rejected — sender frame validation failed');
    return {
      status: 'error',
      code: 'rejected',
      message: 'Self-hosted server probing is not available from this frame.',
    };
  }
  // Set synchronously, before the handler's first await, so a burst dispatched in
  // one tick cannot all observe an empty set.
  // Fail closed rather than collapsing an unidentifiable sender into a shared bucket.
  // `?? -1` would put every such caller on one key, so unrelated frames would refuse
  // each other's probes as 'busy'. Electron populates `sender` for every ipcMain.handle
  // today; this guards the refactor or harness where it does not. `rejected`, not
  // `busy` — 'busy' is a transient retryable state and this is not transient.
  const senderId = event.sender?.id;
  if (senderId === undefined) {
    console.warn('[selfHosted:probeServer] rejected — the invoke event carried no sender id');
    return {
      status: 'error',
      code: 'rejected',
      message: 'Self-hosted server probing is not available from this frame.',
    };
  }
  if (probesInFlight.has(senderId)) {
    console.warn('[selfHosted:probeServer] refused — a probe is already in flight');
    return { status: 'error', code: 'busy', message: '' };
  }
  probesInFlight.add(senderId);
  try {
    return await handleProbeSelfHostedServer(url);
  } finally {
    probesInFlight.delete(senderId);
  }
});

async function handleProbeSelfHostedServer(url: unknown): Promise<SelfHostedProbeResult> {
  if (typeof url !== 'string') {
    return {
      status: 'error',
      code: 'invalid_url',
      message: 'Enter a self-hosted server URL.',
    };
  }

  const normalized = normalizeSelfHostedUrl(url);
  if (!normalized.ok) {
    return { status: 'error', code: normalized.code, message: normalized.message };
  }

  const parsed = new URL(normalized.apiBase);
  const resolved: ResolveForDisplayResult = await resolveForDisplay(parsed.hostname);
  if (!resolved.ok && resolved.kind === 'tier1') {
    // Reason token only; the renderer owns the user-facing copy.
    return { status: 'error', code: 'address_not_allowed', message: resolved.reason };
  }
  if (!resolved.ok) {
    return { status: 'error', code: 'unreachable', message: 'Could not reach the server.' };
  }

  // Consent is scoped to the address class the ceremony displayed. An origin approved
  // against a public address has NOT authorized a tier-2 dial under that name, so a
  // server that has genuinely moved onto a LAN re-runs the ceremony once — showing the
  // private address this time — instead of stranding. An origin already approved at
  // tier 2, or one still resolving public, never re-prompts.
  const approvalTier = approvalTierForApiBase(normalized.apiBase);
  const needsApproval =
    approvalTier === null || (resolved.decision.tier === 'tier2' && approvalTier !== 'tier2');

  if (needsApproval) {
    // Rations the DIALOG, not the probe: an approved origin never reaches this
    // branch, so re-probing it stays unthrottled. The token is taken before the
    // modal opens because a declined ceremony cost the user the same interruption
    // an approved one did.
    if (!consumeCeremonyToken()) {
      console.warn('[selfHosted:probeServer] refused — approval prompt budget exhausted');
      return { status: 'error', code: 'too_many_prompts', message: '' };
    }
    // `mainWindow` is read HERE, at ceremony time — not at handler registration,
    // when the auth window does not yet exist.
    const approved = await requestSelfHostedApproval(mainWindow, {
      host: parsed.hostname,
      // Every resolved address, not just the representative: the user consents to the
      // whole set the name answers with, so the whole set is what the dialog states.
      address: resolved.addresses.join(', '),
      decision: resolved.decision,
    });
    if (!approved) {
      return { status: 'error', code: 'approval_declined', message: 'Connection cancelled.' };
    }
    // Consent is not proof. The grant is PROVISIONAL until the probe shows this origin
    // answers like a Concord server: beginPendingApproval authorizes only the dial the
    // probe itself needs, and the durable record is written after. Committing first meant
    // any post-consent failure — non-Concord server, TLS, ECONNREFUSED, HTTP 500 — still
    // left a permanent grant gating auth:storeRefreshToken and the SSO exchange.
    beginPendingApproval(normalized.apiBase, resolved.address);
    let result: SelfHostedProbeResult;
    try {
      result = await probeSelfHostedServer(normalized.apiBase);
    } finally {
      clearPendingApproval();
    }
    if (result?.status !== 'ok') return result;
    if (!commitSelfHostedApproval(normalized.apiBase, resolved.address)) {
      return {
        status: 'error',
        code: 'approval_not_saved',
        message: "Concord couldn't save your choice.",
      };
    }
    return result;
  }

  return probeSelfHostedServer(normalized.apiBase);
}

ipcMain.handle('auth:storeRefreshToken', (event, data: unknown) => {
  if (!isTrustedAuthSender(event)) {
    return rejectUntrustedAuthSender('auth:storeRefreshToken');
  }
  if (!isStoreRefreshTokenPayload(data)) {
    return rejectInvalidAuthPayload('auth:storeRefreshToken');
  }

  const credentialOwner = storeRefreshToken(data);
  // Main learns of a sign-in HERE and, for a cold start, nowhere else. App.tsx
  // signals invite-readiness from a mount effect before anyone has signed in, so
  // a startup code is DELIVERED while the owner is still null and the renderer's
  // later replay into the modal is local and sends no IPC back. Without this the
  // tag stayed null for the life of the process and the pre-login exemption never
  // expired (CODEX P2). Fence first: if a PREVIOUS owner's codes are still
  // retained they must go rather than be adopted by whoever just signed in.
  fenceDeepLinksOnOwnerChange();
  noteDeepLinkOwner();
  // Clear any cached restore result so subsequent restoreSession calls
  // (e.g. after HMR or renderer reload) use the fresh token, not a stale
  // 'refresh_failed' from a previous attempt.
  restoreSessionPromise = null;

  // Activate auto-updater now that we have an API base
  // (first-launch users won't have this until login)
  if (data.apiBase) {
    setUpdateFeedUrl(data.apiBase);
  }
  return credentialOwner;
});

// Deduplicate restoreSession: React Strict Mode can fire the renderer's
// useEffect twice, causing two IPC calls.  Without dedup each call would
// trigger a separate token rotation, wasting rotations and widening the
// window for race conditions.
let restoreSessionPromise: Promise<{
  status: string;
  accessToken?: string;
  sessionId?: string;
  e2eeKeys?: unknown;
  rememberMe?: boolean;
  credentialOwner?: CredentialOwner;
  pendingE2EEUnlock?: boolean;
}> | null = null;

function isTrustedAuthSender(event: Electron.IpcMainInvokeEvent): boolean {
  return isPermittedFrameUrl(event.senderFrame?.url ?? '', getRemoteSpaBaseUrl());
}

function rejectUntrustedAuthSender(channel: string): { status: 'rejected' } {
  console.warn(`[${channel}] rejected — sender frame validation failed`);
  return { status: 'rejected' };
}

function rejectInvalidAuthPayload(channel: string): { status: 'rejected' } {
  console.warn(`[${channel}] rejected — invalid payload`);
  return { status: 'rejected' };
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isNonEmptyString(value: unknown): value is string {
  return typeof value === 'string' && value.length > 0;
}

function isValidApiBase(value: unknown): value is string {
  if (!isNonEmptyString(value)) return false;
  try {
    const parsed = new URL(value);
    const wellFormed =
      (parsed.protocol === 'https:' || parsed.protocol === 'http:') &&
      parsed.username === '' &&
      parsed.password === '' &&
      parsed.origin !== 'null';
    if (!wellFormed) return false;
    // Defense-in-depth (#1872 review): a packaged client only ever talks to the
    // single SaaS control-plane the updater already TLS-pins, so pin the
    // renderer-supplied apiBase to that origin too. This denies a
    // compromised-but-correctly-framed renderer the ability to steer the update
    // feed / persisted API origin at an attacker host — the one path
    // code-signing doesn't cover on Linux (no publisher verification). Dev/LAN
    // builds (isPackaged=false) keep the host-agnostic check.
    if (app.isPackaged) {
      return (
        parsed.origin === new URL(PRODUCTION_API_BASE).origin ||
        isValidatedSelfHostedApiBase(parsed.origin)
      );
    }
    return true;
  } catch {
    return false;
  }
}

function isStoreRefreshTokenPayload(data: unknown): data is {
  refreshToken: string;
  rememberMe: boolean;
  apiBase: string;
  accessToken?: string;
} {
  return (
    isRecord(data) &&
    isNonEmptyString(data.refreshToken) &&
    typeof data.rememberMe === 'boolean' &&
    isValidApiBase(data.apiBase) &&
    (data.accessToken === undefined || typeof data.accessToken === 'string')
  );
}

function isStoreE2EEKeysPayload(data: unknown): data is {
  wrappingKeyBase64: string;
  preferencesKeyBase64: string;
  wrappedPrivateKeyBase64: string;
} {
  return (
    isRecord(data) &&
    isNonEmptyString(data.wrappingKeyBase64) &&
    isNonEmptyString(data.preferencesKeyBase64) &&
    isNonEmptyString(data.wrappedPrivateKeyBase64)
  );
}

function isLogoutPayload(data: unknown): data is { accessToken?: string } | undefined {
  if (data === undefined) return true;
  if (!isRecord(data)) return false;
  return data.accessToken === undefined || typeof data.accessToken === 'string';
}

function isCredentialOwner(value: unknown): value is CredentialOwner {
  return typeof value === 'number' && Number.isSafeInteger(value) && value > 0;
}

ipcMain.handle('auth:restoreSession', (event) => {
  if (!isTrustedAuthSender(event)) {
    return rejectUntrustedAuthSender('auth:restoreSession');
  }

  if (restoreSessionPromise) return restoreSessionPromise;

  restoreSessionPromise = (async () => {
    const restored = restoreRefreshToken();
    if (restored.status !== 'ok') {
      return { status: restored.status };
    }
    const restoreOwner = getCredentialCustodyState().credentialOwner;
    if (restoreOwner === null) {
      return { status: 'refresh_failed' };
    }
    // The other point at which main learns who is signed in — a remembered
    // session restored at launch, where a startup deep link is likeliest of all.
    // Same order as the login path: fence a previous owner's retained codes, then
    // claim what remains.
    //
    // The FENCE half is unproven here and says so rather than pretending: at cold
    // start the tag is null, so it is a no-op, and every path that would leave a
    // previous owner's tag live before a restore also forgets. Dropping it alone
    // leaves the suite green. It stays because the two auth points must behave
    // identically — an asymmetry between them is a trap, and this is the ordering
    // whose inverse is a cross-account leak on the login path (covered there).
    fenceDeepLinksOnOwnerChange(false, restoreOwner);
    noteDeepLinkOwner(restoreOwner);
    // Token restored from disk or main-process memory — refresh it to get a
    // fresh access token.
    const refreshResult = await performRefresh();
    if (refreshResult.status === 'ok' && refreshResult.accessToken) {
      // A fresh login may have replaced the restored credential after refresh
      // resolved but before this continuation resumed. Verify the owner before
      // reading keys so an old access token can never be paired with a
      // successor's E2EE custody.
      const ownerCheck = getCredentialCustodyState();
      if (ownerCheck.credentialOwner !== restoreOwner) {
        return { status: 'refresh_failed' };
      }
      // Also restore E2EE keys if available. No await follows the owner check,
      // so main-process state cannot interleave before this result is built.
      const e2eeKeys = restoreE2EEKeys();
      const custody = getCredentialCustodyState();
      if (custody.credentialOwner !== restoreOwner) {
        return { status: 'refresh_failed' };
      }
      return {
        status: 'restored',
        accessToken: refreshResult.accessToken,
        sessionId: refreshResult.sessionId,
        e2eeKeys,
        rememberMe: restored.rememberMe,
        credentialOwner: custody.credentialOwner,
        pendingE2EEUnlock: custody.pendingE2EEUnlock,
      };
    }
    return { status: 'refresh_failed' };
  })().finally(() => {
    restoreSessionPromise = null;
  });

  return restoreSessionPromise;
});

ipcMain.handle('auth:storeE2EEKeys', (event, data: unknown) => {
  if (!isTrustedAuthSender(event)) {
    return rejectUntrustedAuthSender('auth:storeE2EEKeys');
  }
  if (!isStoreE2EEKeysPayload(data)) {
    return rejectInvalidAuthPayload('auth:storeE2EEKeys');
  }

  // Surface persistence success/failure to the renderer (#1288) — a genuine
  // keychain/disk write failure returns false rather than being swallowed.
  return storeE2EEKeys(data);
});

ipcMain.handle('auth:storeE2EEKeysIfOwner', (event, data: unknown, owner: unknown) => {
  if (!isTrustedAuthSender(event)) {
    return rejectUntrustedAuthSender('auth:storeE2EEKeysIfOwner');
  }
  if (!isStoreE2EEKeysPayload(data) || !isCredentialOwner(owner)) {
    return rejectInvalidAuthPayload('auth:storeE2EEKeysIfOwner');
  }

  return storeE2EEKeysIfOwner(data, owner);
});

ipcMain.handle('auth:refreshToken', async (event) => {
  if (!isTrustedAuthSender(event)) {
    return rejectUntrustedAuthSender('auth:refreshToken');
  }

  return performRefresh();
});

ipcMain.handle('auth:logout', async (event, data?: { accessToken?: string }) => {
  if (!isTrustedAuthSender(event)) {
    return rejectUntrustedAuthSender('auth:logout');
  }
  if (!isLogoutPayload(data)) {
    return rejectInvalidAuthPayload('auth:logout');
  }

  // The renderer's real logout path is userStore.logout() -> electron.logout(),
  // which lands HERE and never reaches the auth:clearTokens handler. It happens
  // to reach it later, through nuclearReset()'s optional-chained
  // `globalThis.electron?.clearTokens?.()` (resetService.ts) — a renderer-side
  // call ordering that no main-process test can observe and that a refactor
  // could drop silently. The §5.2 fence therefore holds at THIS edge on its own.
  //
  // BEFORE the await, not after: performLogout clears the credentials on its
  // first line and only then issues an unbounded network POST, so anything after
  // the await leaves the clear open for the width of that request.
  forgetDeliveredDeepLinks();
  await performLogout(data?.accessToken);
  return undefined;
});

ipcMain.handle('auth:clearTokens', (event, opts: unknown) => {
  if (!isTrustedAuthSender(event)) {
    return rejectUntrustedAuthSender('auth:clearTokens');
  }

  // Renderer-supplied, so read strictly: only the exact literal `true` opts OUT,
  // and anything else — absent, malformed, truthy-but-not-true — forgets.
  //
  // The default direction is load-bearing (CODEX P2, round 12). An OLDER SPA on
  // this shell — spaLoader loads one indefinitely — predates this argument and
  // sends nothing, so whatever "no argument" means is what those SPAs get. Keep
  // by default would leave user A's delivered invite alive through a forced
  // logout and replay it into user B's session; forget by default costs such an
  // SPA an invite at SSO start, which the user recovers by clicking the link
  // again. Lose an invite, never cross an account.
  const keepDeepLinks =
    typeof opts === 'object' &&
    opts !== null &&
    (opts as { keepDeepLinks?: unknown }).keepDeepLinks === true;

  // Deliberately does NOT forget deep links (CODEX P2, round 8). This handler
  // has three renderer callers and NOT ONE of them is unambiguously a logout:
  //   - useSSOFlow.begin() calls it immediately BEFORE starting SSO, so
  //     forgetting here destroys a `concord://invite/CODE` queued while the app
  //     was closed — the click-invite-then-sign-in flow, which is the PRIMARY
  //     path this whole change exists to repair;
  //   - App.tsx's clearOwnerlessRestoredCredential rejects a restored credential
  //     at cold start, after which the user logs in and should still get it;
  //   - resetService.gracefulReset() reaches it on a rememberMe refresh failure,
  //     which deliberately keeps codes for the resuming session.
  // Real logout has its own handler, `auth:logout`, which forgets — so nothing
  // is lost by narrowing this one. The claim is "every LOGOUT path forgets", and
  // this is the second handler to have been wrongly read as one.
  //
  // `keepDeepLinks` is what the login-side callers pass: useSSOFlow before
  // starting SSO, App.tsx rejecting an ownerless restored credential, and
  // gracefulReset's shared clear (whose remembered-session path resumes). The one
  // caller that does NOT pass it is `nuclearReset()` — a refresh failure with
  // `rememberMe === false`, documented in resetService as "ends the authenticated
  // lifecycle", which reaches this handler through gracefulReset and never calls
  // `auth:logout`.
  clearTokens();
  if (!keepDeepLinks) forgetDeliveredDeepLinks();
  return undefined;
});

// FORGET-ONLY, and that is the whole reason it exists (#2363). Every other edge
// that forgets also destroys credentials, and the one teardown that must forget
// WITHOUT destroying them is a `rememberMe` refresh failure: the session has ended
// and the user must re-authenticate, but the disk tokens stay so the next launch
// can retry. `gracefulReset` had no way to say "the session ended" without also
// saying "delete the credentials", so main kept `gate.lastCode` alive and a swap
// inside the carry window replayed user A's invite into user B's renderer.
//
// The renderer-side `deep-link-session-ended` fence is necessary and NOT
// sufficient: it clears the copy App holds, and main holds its own.
//
// Sender-fenced like the auth channels it sits beside. Forgetting is the fail-safe
// direction — the worst a hostile caller achieves is losing its own pending invite
// — but a renderer-reachable channel that mutates main's delivery state gets the
// same guard as its neighbours regardless, rather than an argument for why it does
// not need one.
ipcMain.handle('deeplink:forget', (event) => {
  if (!isTrustedAuthSender(event)) {
    return rejectUntrustedAuthSender('deeplink:forget');
  }

  forgetDeliveredDeepLinks();
  return undefined;
});

ipcMain.handle('auth:clearTokensIfOwner', (event, owner: unknown) => {
  if (!isTrustedAuthSender(event)) {
    return rejectUntrustedAuthSender('auth:clearTokensIfOwner');
  }
  if (!isCredentialOwner(owner)) {
    return rejectInvalidAuthPayload('auth:clearTokensIfOwner');
  }

  // Deliberately does NOT forget deep links, despite ending credentials.
  // Every caller is LOGIN-side, not logout: aborted SSO (useSSOFlow), an
  // interrupted email verification, a cancelled account-link or passphrase
  // setup, an aborted login continuation (Login.tsx), and the eager-unlock
  // re-persist cleanup. Forgetting here is exactly the asymmetry the deep-link
  // rules warn against, and the loss is concrete: a `concord://invite/CODE`
  // opened while the app was closed sits in `pendingDeepLinks` before any
  // window exists — main holds the ONLY copy — so an SSO sign-in that aborts
  // would erase it and the retry would land with the invite silently gone.
  // That is #2363's own symptom on a new path.
  return clearTokensIfOwner(owner);
});

ipcMain.handle('auth:getCapabilities', (event) => {
  if (!isTrustedAuthSender(event)) {
    console.warn('[auth:getCapabilities] rejected — sender frame validation failed');
    // Fail closed with this handler's own shape (not the {status:'rejected'}
    // reject shape) so callers always get a valid, safe default.
    return { persistAvailable: false };
  }
  return getCapabilities();
});

ipcMain.handle('auth:getMachineId', (event, apiBase?: unknown) => {
  if (!isTrustedAuthSender(event)) {
    console.warn('[auth:getMachineId] rejected — sender frame validation failed');
    return '';
  }

  if (apiBase === undefined) return getMachineId();
  if (!isValidApiBase(apiBase)) return '';
  return getMachineId(apiBase);
});

// ─── PiP (Picture-in-Picture) Window Management ──────────────────────
type PipWindowEntry = {
  window: BrowserWindow;
  ownerFrameTreeNodeId: number;
  remoteSpaUrl: string | null;
  /**
   * #3104 D6 — the per-window capability that authenticates this PiP on the
   * renderer-side `concord-pip` RPC channel. Minted here because the main
   * process is the only out-of-band path between two renderers, and disclosed
   * only by `pip:opened` (to the main window) and `pip:session` (to this PiP's
   * own main frame). Never log it: the private reply channel is NAMED from it.
   */
  sessionToken: string;
};
const pipWindows = new Map<string, PipWindowEntry>();

/** Force-close PiP children during full-app shutdown so renderer unload guards cannot veto quit. */
function destroyPipWindowsForQuit(): void {
  for (const { window: pip } of pipWindows.values()) {
    if (!pip.isDestroyed()) pip.destroy();
  }
}

/**
 * Resolve the PiP `id` names together with WHICH of the two permitted
 * relationships the sender holds. `pip:close` / `pip:setAlwaysOnTop` accept
 * either; `pip:session` accepts only the PiP's own main frame, because the
 * token it returns is a working capability rather than a window mutation.
 */
function resolvePipSender(
  event: IpcMainInvokeEvent,
  id: unknown
): { entry: PipWindowEntry; isPipMainFrame: boolean } | null {
  if (typeof id !== 'string' || id.length === 0) return null;
  const senderFrame = event.senderFrame;
  if (!senderFrame) return null;
  const entry = pipWindows.get(id);
  if (!entry || entry.window.isDestroyed()) return null;
  const isPipMainFrame =
    event.sender === entry.window.webContents &&
    senderFrame.frameTreeNodeId === entry.window.webContents.mainFrame.frameTreeNodeId;
  if (
    !isValidPipOpenSender(
      senderFrame.url,
      app.isPackaged,
      isPipMainFrame ? entry.remoteSpaUrl : getRemoteSpaUrl()
    )
  ) {
    return null;
  }
  const isOwnerFrame = entry.ownerFrameTreeNodeId === senderFrame.frameTreeNodeId;
  if (!isOwnerFrame && !isPipMainFrame) return null;
  return { entry, isPipMainFrame };
}

function getOwnedPipWindow(event: IpcMainInvokeEvent, id: string): BrowserWindow | null {
  return resolvePipSender(event, id)?.entry.window ?? null;
}

ipcMain.handle(
  'pip:open',
  (event, opts: { id: string; width?: number; height?: number; title?: string }) => {
    // Defense-in-depth: only accept pip:open from the active SPA, the dev
    // server, or the bundled renderer. Without validation any frame loaded
    // in any window could spawn PiP windows.
    const senderFrame = event.senderFrame;
    const remoteUrl = getRemoteSpaUrl();
    if (!senderFrame || !isValidPipOpenSender(senderFrame.url, app.isPackaged, remoteUrl)) {
      console.warn('[pip:open] rejected — sender frame validation failed');
      return;
    }

    const existing = pipWindows.get(opts.id);
    if (existing) {
      if (existing.ownerFrameTreeNodeId === senderFrame.frameTreeNodeId) {
        existing.window.focus();
      }
      return;
    }

    const contentProtectionEnabled = readContentProtectionPref();
    const pip = new BrowserWindow({
      width: opts.width || 320,
      height: opts.height || 240,
      minWidth: 160,
      minHeight: 120,
      frame: false,
      // #806: opt out of the OS-drawn drop shadow so the lightweight chrome
      // visually reads as "floating glass" rather than "compact app window".
      // On macOS this drops the standard window shadow; on Wayland and X11
      // the compositor decides regardless. Safe on Windows (no-op).
      hasShadow: false,
      alwaysOnTop: true,
      skipTaskbar: true,
      resizable: true,
      title: opts.title || 'Concord Voice PiP',
      webPreferences: {
        preload: path.join(__dirname, '../preload/preload.js'),
        nodeIntegration: false,
        contextIsolation: true,
        sandbox: true,
      },
      backgroundColor: '#000',
    });
    applyContentProtectionAtCreation(pip, contentProtectionEnabled, 'PiP window');

    // Load PiP route (hash-based routing)
    if (!app.isPackaged) {
      pip.loadURL(`http://localhost:3001#/pip/${opts.id}`);
    } else if (remoteUrl) {
      // Tier 3: PiP windows load from the remote SPA, including the
      // `/spa/<sha>/` path component. Origin-only would fall through to
      // nginx's catch-all and redirect to the marketing site (#802).
      pip.loadURL(buildRemotePipUrl(remoteUrl, opts.id));
    } else if (
      (mainWindow?.webContents.getURL() ?? '').startsWith(
        `${SPA_CACHE_SCHEME}://${SPA_CACHE_HOST}/`
      )
    ) {
      // #1870: the main window is serving from the signed last-known-good cache
      // (setRemoteSpaState(null) means remoteUrl is null, but the cache origin is
      // the active shell). Load the PiP child from the SAME cache origin so it
      // renders the cached shell, not the possibly-older bundled asset. The
      // will-navigate gate + spa-cache protocol handler permit and
      // integrity-verify it. Derived from the live main-window URL (stateless) so
      // it cannot drift from the actual effective mode.
      pip.loadURL(`${SPA_CACHE_SCHEME}://${SPA_CACHE_HOST}/index.html#/pip/${opts.id}`);
    } else {
      // #830: PiP bundled mode loads via app:// for consistent origin.
      pip.loadURL(`app://concord/index.html#/pip/${opts.id}`);
    }

    const sessionToken = mintPipSessionToken();
    pipWindows.set(opts.id, {
      window: pip,
      ownerFrameTreeNodeId: senderFrame.frameTreeNodeId,
      remoteSpaUrl: remoteUrl,
      sessionToken,
    });

    pip.on('closed', () => {
      pipWindows.delete(opts.id);
      mainWindow?.webContents.send('pip:closed', { id: opts.id });
    });

    // #3104 D6 — deliberately a PUSH, symmetric with 'pip:closed' above. The
    // main renderer's signaling proxy must already know the capability when the
    // PiP's own renderer finishes booting and issues its first RPC; a pull would
    // race that boot. The PiP window gets its copy by pulling 'pip:session'.
    mainWindow?.webContents.send('pip:opened', { id: opts.id, token: sessionToken });
  }
);

ipcMain.handle('pip:close', (event, opts: { id: string }) => {
  getOwnedPipWindow(event, opts.id)?.close();
});

ipcMain.handle('pip:setAlwaysOnTop', (event, opts: { id: string; flag: boolean }) => {
  getOwnedPipWindow(event, opts.id)?.setAlwaysOnTop(opts.flag);
});

/**
 * #3104 D6 — disclose one PiP's session capability to that PiP alone.
 *
 * This is the whole trust boundary for the renderer-side `concord-pip` RPC
 * channel: `BroadcastChannel` reaches every same-origin document, so the proxy
 * in the main renderer can only distinguish a real PiP by a secret the PiP
 * could not have obtained on its own. `resolvePipSender` proves the caller is
 * this window's main frame (same `webContents`, same `frameTreeNodeId` as its
 * `mainFrame`, and a permitted frame URL), which no other document can forge.
 *
 * The OPENER frame is refused even though it passes the ownership check used by
 * `pip:close`: it already received the token on the `pip:opened` push, so a
 * second disclosure path would widen the surface for nothing.
 *
 * Fails closed to `null` on every other input. The renderer treats `null` as
 * "this window gets no PiP voice", never as "proceed unauthenticated".
 *
 * The token is never logged — the private reply channel is NAMED from it, so
 * printing it is equivalent to publishing it.
 */
ipcMain.handle('pip:session', (event, opts: unknown) => {
  const resolved = resolvePipSender(event, (opts as { id?: unknown } | null | undefined)?.id);
  if (!resolved?.isPipMainFrame) return null;
  return { token: resolved.entry.sessionToken };
});

ipcMain.handle('spa:requestSelfHeal', async (event, payload: unknown) => {
  // Thin adapter: unpack Electron's event-args, delegate to the pure-data
  // handler body. The handler itself does sender-frame validation,
  // payload validation, and dispatches to attemptSelfHeal. Extracted for
  // unit-testability per #753 reconciliation finding TA3.
  await handleSpaRequestSelfHeal({
    senderFrameUrl: event.senderFrame?.url ?? '',
    payload,
    remoteSpaBaseUrl: getRemoteSpaBaseUrl(),
    remoteSpaBaseDir: getRemoteSpaBaseDir(),
  });
});

ipcMain.on('invite:renderer-ready', (event) => {
  if (!mainWindow || event.sender !== mainWindow.webContents) return;
  inviteRendererReady = true;
  drainPendingDeepLinks();
});

// #945: the friend arm's own readiness signal. Deliberately separate from
// 'invite:renderer-ready' so an SPA that only knows the invite channel can never
// arm the friend one — see emitDeepLink above.
ipcMain.on('deeplink:renderer-ready', (event) => {
  if (!mainWindow || event.sender !== mainWindow.webContents) return;
  friendRendererReady = true;
  drainPendingDeepLinks();
});

// Prevent multiple instances
const gotTheLock = app.requestSingleInstanceLock();

if (gotTheLock) {
  handleInviteDeepLinksFromArgv(process.argv, 'argv');

  app.on('open-url', (event, url) => {
    event.preventDefault();
    handleInviteDeepLink(url, 'open-url');
  });

  app.on('second-instance', (_event, argv: string[]) => {
    handleInviteDeepLinksFromArgv(argv, 'second-instance');
    // Someone tried to run a second instance, focus our window
    if (mainWindow) {
      if (mainWindow.isMinimized()) mainWindow.restore();
      mainWindow.focus();
    }
  });
} else {
  app.quit();
}

/**
 * Packaged-mode same-origin navigation allowlist for the `will-navigate` gate.
 * Returns true when `parsedUrl` is a permitted in-window navigation target:
 *   - the active remote SPA origin (origin-equality), OR
 *   - the bundled `app://concord` origin, OR
 *   - the signed last-known-good cache `spa-cache://concord` origin (#1870).
 *
 * For the two non-special schemes, compare protocol + host (WHATWG `URL.origin`
 * is the literal "null" for non-special schemes — same gotcha as
 * pipUrl.ts:isValidPipOpenSender). A remote/compromised page cannot reach
 * `app://` or `spa-cache://` here: no branch matches a remote-origin frame, and
 * Chromium scheme-gating blocks remote→privileged navigation independently.
 *
 * Extracted from the will-navigate handler so that handler stays within the
 * cognitive-complexity budget (S3776) as origins are added.
 */
function isPermittedPackagedNavTarget(parsedUrl: URL, spaOrigin: string | null): boolean {
  if (spaOrigin && parsedUrl.origin === spaOrigin) {
    return true;
  }
  if (parsedUrl.protocol === 'app:' && parsedUrl.host === 'concord') {
    return true;
  }
  return parsedUrl.protocol === `${SPA_CACHE_SCHEME}:` && parsedUrl.host === SPA_CACHE_HOST;
}

// Security: validate navigation. After #754: extends with externalization
// safety net — bare <a href="https://..."> clicks (or programmatic
// navigation) route to the OS browser via shell.openExternal instead of
// silently failing. https-only, symmetric with setWindowOpenHandler.
// See [internal]specs/2026-04-26-754-externalize-blocked-nav-design.md
// and [internal]rules/electron.md "External-link scheme policy".
app.on('web-contents-created', (_, contents) => {
  contents.on('will-navigate', (event, navigationUrl) => {
    let parsedUrl: URL;
    try {
      parsedUrl = new URL(navigationUrl);
    } catch {
      // Malformed URL — fail closed. Do not navigate, do not externalize.
      event.preventDefault();
      return;
    }

    if (app.isPackaged) {
      // Packaged release: allow same-origin SPA navigation within the active
      // remote SPA origin, the bundled `app://concord` origin, or the signed
      // last-known-good cache `spa-cache://concord` origin (#1870). This permits
      // in-window navigations these origins legitimately perform — chunk reloads,
      // programmatic location.assign, and full-page reloads from the error-
      // boundary "Reload" / resetService.softRestart() crash-recovery paths
      // (load-bearing for the cache, which is exactly the degraded-network state
      // where recovery matters). See isPermittedPackagedNavTarget for the
      // protocol+host comparison rationale and why a remote page cannot reach the
      // privileged schemes here.
      if (isPermittedPackagedNavTarget(parsedUrl, getRemoteSpaBaseUrl())) {
        return;
      }
      // Block in-window navigation to anything else.
      event.preventDefault();
      // Drift safety net: route https: navigations to OS browser so a
      // future bare-<a> link or SPA-driven redirect doesn't silently fail.
      // .catch suppresses unhandled-rejection (OS deny) — symmetric with
      // setWindowOpenHandler and the IPC handler.
      if (parsedUrl.protocol === 'https:') {
        shell.openExternal(navigationUrl).catch(() => {});
      }
      // Other schemes (javascript:, data:, file:, http:, vbscript:, ...)
      // are silently dropped after preventDefault — fail-closed posture.
    } else if (parsedUrl.hostname !== 'localhost') {
      // Dev mode: allow any localhost port (HMR), block everything else.
      event.preventDefault();
      if (parsedUrl.protocol === 'https:') {
        shell.openExternal(navigationUrl).catch(() => {});
      }
    }
  });

  // SPA self-heal main-process detection (#753, ADR-0001) — runs outside
  // the renderer bundle, so it survives renderer-side corruption that
  // would silence the renderer-side listener in spaSelfHealClient.ts.
  // Thin adapter delegating to handleDidFailLoad (extracted for unit
  // testability per #753 reconciliation finding TA3).
  contents.on(
    'did-fail-load',
    (_event, errorCode, _errorDescription, validatedURL, isMainFrame) => {
      void handleDidFailLoad({
        errorCode,
        validatedURL,
        isMainFrame,
        remoteSpaBaseUrl: getRemoteSpaBaseUrl(),
        remoteSpaBaseDir: getRemoteSpaBaseDir(),
      });
      if (isMainFrame && validatedURL === 'app://concord/index.html' && errorCode !== -3) {
        revealLoadFailure(mainWindow, 'Could not load application — please reinstall');
      }
    }
  );
});
