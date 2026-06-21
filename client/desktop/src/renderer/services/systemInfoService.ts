/**
 * System diagnostics collector for bug reports (#158).
 *
 * Bundles the small, well-bounded set of environmental signals that helps
 * triage a bug: app version, OS, GPU vendor/renderer, primary display
 * geometry, current WebSocket recovery phase, and the first 8 chars of the
 * machine ID. Everything else is intentionally OUT of scope — message
 * content, account details, friend lists, IPs, and full machine ID never
 * leave this process via bug reports.
 *
 * Contract notes:
 * - The collector is best-effort. Each field is wrapped so a failure in one
 *   probe does not poison the whole snapshot.
 * - `collect()` is async because the app version + machine ID come over IPC.
 * - Field shapes are intentionally narrow strings/numbers — no nested objects
 *   beyond two levels — so the server-side PII scrub can run a simple
 *   recursive string visit.
 */

import { useConnectionStore } from '../stores/connectionStore';

export interface GpuInfo {
  vendor: string;
  renderer: string;
}

export interface DisplayInfo {
  width: number;
  height: number;
  refreshRate?: number;
  scaleFactor: number;
}

export interface SystemInfo {
  appVersion: string;
  platform: string;
  userAgent: string;
  machineIdPrefix: string;
  gpu?: GpuInfo;
  display?: DisplayInfo;
  /** Current WebSocket recovery phase (stable, grace_period, preflight, …). */
  connectionPhase: string;
}

/**
 * Probe app version via the preload IPC. Falls back to 'unknown' if the IPC
 * is absent (e.g., browser dev mode without the preload bridge).
 */
async function probeAppVersion(): Promise<string> {
  try {
    const v = await globalThis.electron?.getVersion?.();
    if (typeof v === 'string' && v.length > 0) return v;
  } catch {
    // fall through
  }
  return 'unknown';
}

/**
 * First 8 chars of the machine ID — sufficient for dedup / follow-up,
 * insufficient for cross-instance correlation. Returns 'unknown' if the
 * preload IPC is missing or the ID hasn't been generated yet.
 */
async function probeMachineIdPrefix(): Promise<string> {
  try {
    const id = await globalThis.electron?.getMachineId?.();
    if (typeof id === 'string' && id.length >= 8) return id.slice(0, 8);
  } catch {
    // fall through
  }
  return 'unknown';
}

/**
 * GPU vendor + renderer via the WebGL `WEBGL_debug_renderer_info` extension.
 * Returns `undefined` if WebGL is unavailable or the extension is not
 * exposed (Chromium >= 88 hides it from cross-origin contexts; the desktop
 * client runs in a first-party context so the extension is available).
 */
function probeGpu(): GpuInfo | undefined {
  try {
    const canvas = document.createElement('canvas');
    const gl = canvas.getContext('webgl') ?? canvas.getContext('experimental-webgl');
    if (!gl) return undefined;
    const ext = (gl as WebGLRenderingContext).getExtension('WEBGL_debug_renderer_info');
    if (!ext) return undefined;
    const debugExt = ext as {
      UNMASKED_VENDOR_WEBGL: number;
      UNMASKED_RENDERER_WEBGL: number;
    };
    const vendor = String(
      (gl as WebGLRenderingContext).getParameter(debugExt.UNMASKED_VENDOR_WEBGL) ?? 'unknown'
    );
    const renderer = String(
      (gl as WebGLRenderingContext).getParameter(debugExt.UNMASKED_RENDERER_WEBGL) ?? 'unknown'
    );
    return { vendor, renderer };
  } catch {
    return undefined;
  }
}

/**
 * Primary display info from window/screen APIs. `refreshRate` is only
 * available when the underlying platform exposes it (modern Chromium does
 * via `screen.refreshRate`, but the property is non-standard, so we feature
 * test rather than declare).
 */
function probeDisplay(): DisplayInfo | undefined {
  try {
    const s = globalThis.screen;
    if (!s) return undefined;
    const scaleFactor = globalThis.devicePixelRatio || 1;
    const refreshRate = (s as Screen & { refreshRate?: number }).refreshRate;
    return {
      width: s.width,
      height: s.height,
      refreshRate: typeof refreshRate === 'number' ? refreshRate : undefined,
      scaleFactor,
    };
  } catch {
    return undefined;
  }
}

/**
 * Collect everything in parallel. Returns a fully-populated `SystemInfo`
 * even if some probes failed — failed probes degrade to `undefined`/`unknown`
 * rather than throwing.
 */
export async function collect(): Promise<SystemInfo> {
  const [appVersion, machineIdPrefix] = await Promise.all([
    probeAppVersion(),
    probeMachineIdPrefix(),
  ]);
  return {
    appVersion,
    platform: globalThis.navigator?.platform ?? 'unknown',
    userAgent: globalThis.navigator?.userAgent ?? 'unknown',
    machineIdPrefix,
    gpu: probeGpu(),
    display: probeDisplay(),
    connectionPhase: useConnectionStore.getState().phase,
  };
}
