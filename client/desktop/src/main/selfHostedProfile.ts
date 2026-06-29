import { app } from 'electron';
import { createHash } from 'node:crypto';
import path from 'node:path';

const SAAS_API_ORIGIN = 'https://api.concordvoice.chat';

const validatedSelfHostedOrigins = new Set<string>();

export interface ProfilePaths {
  tokenFile: string;
  metaFile: string;
  e2eeFile: string;
  machineIdFile: string;
}

function originForApiBase(apiBase: string): string {
  return new URL(apiBase).origin;
}

function hashOrigin(origin: string): string {
  return createHash('sha256').update(origin).digest('hex');
}

function userDataRoot(): string {
  return app.getPath('userData');
}

export function profileIdForApiBase(apiBase: string): string {
  const origin = originForApiBase(apiBase);
  if (origin === SAAS_API_ORIGIN) return 'saas';
  return `selfhost-${hashOrigin(origin).slice(0, 16)}`;
}

export function profilePathsForApiBase(apiBase: string): ProfilePaths {
  const origin = originForApiBase(apiBase);
  const root = userDataRoot();
  const dir = origin === SAAS_API_ORIGIN ? root : path.join(root, 'profiles', hashOrigin(origin));

  return {
    tokenFile: path.join(dir, 'secure-token.dat'),
    metaFile: path.join(dir, 'token-meta.json'),
    e2eeFile: path.join(dir, 'secure-e2ee.dat'),
    machineIdFile: path.join(dir, 'machine-id.json'),
  };
}

export function rememberValidatedSelfHostedApiBase(apiBase: string): void {
  const origin = originForApiBase(apiBase);
  if (origin !== SAAS_API_ORIGIN) {
    validatedSelfHostedOrigins.add(origin);
  }
}

export function isValidatedSelfHostedApiBase(apiBase: string): boolean {
  const origin = originForApiBase(apiBase);
  return origin !== SAAS_API_ORIGIN && validatedSelfHostedOrigins.has(origin);
}

export function _resetSelfHostedProfileForTesting(): void {
  validatedSelfHostedOrigins.clear();
}
