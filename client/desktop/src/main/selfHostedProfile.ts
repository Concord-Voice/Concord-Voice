import { app } from 'electron';
import { createHash } from 'node:crypto';
import path from 'node:path';
import { appendApprovalRecord, findApprovalRecord, readApprovalsFile } from './selfHostedApprovals';
import { classifyAddress } from './egressPolicy';

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

/**
 * The consented class for one address, or null when the address is not approvable at
 * all. Fail CLOSED: only an explicit `public` or `tier2` verdict yields a tier. The
 * prior `=== 'public' ? 'public' : 'tier2'` folded `tier1` and `invalid` into the MORE
 * permissive label, inside the one function that mints dial permission.
 */
function approvableTierForAddress(address: string): 'tier2' | 'public' | null {
  const decision = classifyAddress(address.replace(/^\[|\]$/g, ''));
  if (decision.tier !== 'public' && decision.tier !== 'tier2') return null;
  return decision.tier;
}

/**
 * Provisional dial permission for the duration of ONE ceremony's own probe.
 *
 * The user has consented, but nothing has yet proven this origin is a Concord server.
 * Committing before the probe left EVERY post-consent failure — non-Concord server,
 * TLS failure, ECONNREFUSED, HTTP 500, oversized body — holding a permanent grant that
 * gates auth:storeRefreshToken and the SSO exchange, with no in-app revocation. So the
 * probe's dial runs on this slot and the durable record is written only on success.
 *
 * It grants DIAL permission only, never origin trust: isValidatedSelfHostedApiBase
 * still reads the durable set, so a pending ceremony cannot admit a token store.
 */
let pendingCeremony: { origin: string; tier: 'tier2' | 'public' } | null = null;

export function beginPendingApproval(apiBase: string, address: string): void {
  const tier = approvableTierForAddress(address);
  if (tier === null) return; // unapprovable address grants nothing; the dial fails closed
  pendingCeremony = { origin: originForApiBase(apiBase), tier };
}

export function clearPendingApproval(): void {
  pendingCeremony = null;
}

/** THE ONLY minting authority for validatedSelfHostedOrigins. Writes file + set. */
export function commitSelfHostedApproval(apiBase: string, address: string): boolean {
  const origin = originForApiBase(apiBase);
  if (origin === SAAS_API_ORIGIN) return true; // SaaS is never a self-hosted grant; no-op success
  const tierAtApproval = approvableTierForAddress(address);
  if (tierAtApproval === null) return false; // never approvable; mint nothing
  const saved = appendApprovalRecord({
    origin,
    approvedAt: new Date().toISOString(),
    lastSeenAddress: address, // display-only; NEVER an input to isDialPermitted
    tierAtApproval,
  });
  if (!saved) return false; // do NOT optimistically update the set on a failed write
  validatedSelfHostedOrigins.add(origin);
  return true;
}

/** Populate the hot-path set once at startup from the durable store. */
export function loadApprovedSelfHostedOrigins(): void {
  for (const record of readApprovalsFile()) {
    if (record.origin !== SAAS_API_ORIGIN) validatedSelfHostedOrigins.add(record.origin);
  }
}

/**
 * ORIGIN TRUST: may Concord hold credentials for, and talk to, this origin at all?
 * This is NOT the dial predicate — see isTier2DialApproved below. The two questions
 * are deliberately separate functions and must stay separate.
 */
export function isValidatedSelfHostedApiBase(apiBase: string): boolean {
  const origin = originForApiBase(apiBase);
  return origin !== SAAS_API_ORIGIN && validatedSelfHostedOrigins.has(origin);
}

/**
 * The address class the user actually consented to, or null when this origin was
 * never approved. SaaS is null: it holds no approval record, so it keeps the same
 * "not a self-hosted grant" answer isValidatedSelfHostedApiBase gives it.
 */
export function approvalTierForApiBase(apiBase: string): 'tier2' | 'public' | null {
  const origin = originForApiBase(apiBase);
  if (origin === SAAS_API_ORIGIN) return null;
  if (!validatedSelfHostedOrigins.has(origin)) return null; // in-memory set is the mint gate
  return findApprovalRecord(origin)?.tierAtApproval ?? null; // absent record ⇒ fail closed
}

/**
 * DIAL PERMISSION: the predicate `isDialPermitted`'s tier-2 branch must consume.
 * A ceremony that displayed "Resolves to: 203.0.113.10, on the internet" is not
 * consent to dial 127.0.0.0/8, RFC1918, fc00::/7, or CGNAT under that name after a
 * DNS flip — so origin membership alone must never authorize a tier-2 dial.
 */
export function isTier2DialApproved(apiBase: string): boolean {
  if (approvalTierForApiBase(apiBase) === 'tier2') return true;
  // Additive only — a pending ceremony can never REMOVE a durable grant, so this
  // widens the predicate for exactly one origin for exactly one probe.
  const origin = originForApiBase(apiBase);
  return pendingCeremony?.origin === origin && pendingCeremony.tier === 'tier2';
}

export function _resetSelfHostedProfileForTesting(): void {
  validatedSelfHostedOrigins.clear();
  pendingCeremony = null;
}
