import { API_BASE, WS_BASE } from '../config';

let runtimeApiBase = API_BASE;
let runtimeWsBase = WS_BASE;
let runtimeServerSelectionEpoch = 0;

export interface RuntimeServerSelection {
  readonly apiBase: string;
  readonly epoch: number;
}

function normalizeHttpBase(base: string): string {
  const parsed = new URL(base);
  if (parsed.protocol !== 'https:' && parsed.protocol !== 'http:') {
    throw new Error('Runtime server base must use HTTP or HTTPS.');
  }
  return parsed.origin;
}

function deriveWsBase(apiBase: string): string {
  const parsed = new URL(apiBase);
  parsed.protocol = parsed.protocol === 'https:' ? 'wss:' : 'ws:';
  return parsed.origin;
}

export function getApiBase(): string {
  return runtimeApiBase;
}

export function getWsBase(): string {
  return runtimeWsBase;
}

export function captureRuntimeServerSelection(): RuntimeServerSelection {
  return { apiBase: runtimeApiBase, epoch: runtimeServerSelectionEpoch };
}

export function runtimeServerSelectionIsCurrent(selection: RuntimeServerSelection): boolean {
  return selection.apiBase === runtimeApiBase && selection.epoch === runtimeServerSelectionEpoch;
}

export function apiUrl(path: string): string {
  const normalizedPath = path.startsWith('/') ? path : `/${path}`;
  return `${runtimeApiBase}${normalizedPath}`;
}

export function mediaUrl(url?: string | null): string | undefined {
  if (!url) return undefined;
  if (url.startsWith('/')) return apiUrl(url);
  if (url.startsWith('data:') || url.startsWith('blob:') || /^https?:\/\//i.test(url)) {
    return url;
  }
  return undefined;
}

export function setRuntimeServerBase(base: string): void {
  const apiBase = normalizeHttpBase(base);
  const wsBase = deriveWsBase(apiBase);
  runtimeApiBase = apiBase;
  runtimeWsBase = wsBase;
  runtimeServerSelectionEpoch += 1;
}

export function resetRuntimeServerBase(): void {
  runtimeApiBase = API_BASE;
  runtimeWsBase = WS_BASE;
  runtimeServerSelectionEpoch += 1;
}
