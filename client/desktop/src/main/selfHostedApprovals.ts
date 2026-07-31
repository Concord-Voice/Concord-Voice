import { app } from 'electron';
import fs from 'node:fs';
import path from 'node:path';

export interface ApprovalRecord {
  origin: string;
  approvedAt: string;
  lastSeenAddress: string;
  tierAtApproval: 'tier2' | 'public';
}

interface ApprovalsFile {
  version: 1;
  approvals: ApprovalRecord[];
}

// Root-level, NOT inside profiles/<sha256>/ — this file is the index of which profiles
// may exist. Path via the pinned userData root (ADR-0020); do not perturb the pin.
function approvalsPath(): string {
  return path.join(app.getPath('userData'), 'self-hosted-approvals.json');
}

let cache: ApprovalRecord[] | null = null;

/**
 * `scheme://host:port` for the longest host normalizeSelfHostedUrl will accept (RFC 1035
 * §2.3.4's 253) plus generous room for scheme and port. A record wider than any origin
 * this app can ever mint is corrupt or hand-written.
 */
const MAX_ORIGIN_LENGTH = 320;
/** One record per approval ceremony, and the ceremony is budget-rationed. */
const MAX_RECORDS = 256;
/** ISO-8601 and an IPv6 address are both far under this; it only bounds a hostile file. */
const MAX_FIELD_LENGTH = 128;

function isBounded(v: unknown, max: number): v is string {
  return typeof v === 'string' && v.length <= max;
}

function isValidRecord(v: unknown): v is ApprovalRecord {
  if (typeof v !== 'object' || v === null) return false;
  const r = v as Record<string, unknown>;
  if (!isBounded(r.origin, MAX_ORIGIN_LENGTH)) return false;
  try {
    // A malformed origin, or one that is not already canonical, invalidates the WHOLE file.
    const url = new URL(r.origin);
    if (url.origin !== r.origin) return false;
    // `new URL().origin` round-trips any SPECIAL scheme, so `ftp://x.example` survives the
    // canonicality check above. Only the two schemes normalizeSelfHostedUrl admits may
    // appear in a trust record.
    if (url.protocol !== 'https:' && url.protocol !== 'http:') return false;
  } catch {
    return false;
  }
  return (
    isBounded(r.approvedAt, MAX_FIELD_LENGTH) &&
    isBounded(r.lastSeenAddress, MAX_FIELD_LENGTH) &&
    (r.tierAtApproval === 'tier2' || r.tierAtApproval === 'public')
  );
}

/** Parse once, cache. ANY failure yields the empty set — never a partial one. */
export function readApprovalsFile(): ApprovalRecord[] {
  if (cache) return cache;
  let raw: string;
  try {
    raw = fs.readFileSync(approvalsPath(), 'utf8'); // missing file is normal (first run)
  } catch {
    cache = [];
    return cache;
  }
  try {
    const parsed = JSON.parse(raw) as ApprovalsFile;
    if (parsed?.version !== 1 || !Array.isArray(parsed.approvals)) throw new Error('shape');
    if (parsed.approvals.length > MAX_RECORDS) throw new Error('too many records');
    if (!parsed.approvals.every(isValidRecord)) throw new Error('record');
    cache = parsed.approvals;
  } catch {
    cache = []; // do not salvage valid-looking entries: partial recovery of a trust file is a trust bug
  }
  return cache;
}

/** Atomic 0o600 write + re-read-verify. Returns false on any failure; leaves cache unchanged. */
export function appendApprovalRecord(record: ApprovalRecord): boolean {
  const next: ApprovalsFile = { version: 1, approvals: [...readApprovalsFile(), record] };
  const dest = approvalsPath();
  const tmp = `${dest}.${process.pid}.tmp`;
  try {
    fs.writeFileSync(tmp, JSON.stringify(next), { mode: 0o600 });
    fs.renameSync(tmp, dest); // same-directory atomic
    cache = null;
    const verified = readApprovalsFile();
    return verified.some((r) => r.origin === record.origin);
  } catch {
    try {
      fs.rmSync(tmp, { force: true });
    } catch {
      /* best effort */
    }
    return false;
  }
}

/**
 * The MOST RECENT record for an origin, or undefined when it was never approved.
 * The file is append-only, so a re-approval taken at a stronger tier must win over
 * the earlier weaker one — searching from the front would pin the origin to its
 * first-ever consent forever.
 */
export function findApprovalRecord(origin: string): ApprovalRecord | undefined {
  const all = readApprovalsFile();
  for (let i = all.length - 1; i >= 0; i--) {
    if (all[i].origin === origin) return all[i];
  }
  return undefined;
}

export function _resetApprovalsCacheForTesting(): void {
  cache = null;
}
