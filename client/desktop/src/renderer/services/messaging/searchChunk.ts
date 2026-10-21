/**
 * One search chunk (#3875): walk the scopes back in time together, at most
 * `chunkWindowDays` or `chunkMaxMessages` (whichever comes first), paced by a
 * token bucket and decrypted in slices that yield to the main thread.
 *
 * Fences: every decrypted row is re-checked with `canIndexBackfillMessage`
 * after its await (scope invalidation, account reset and delete tombstones),
 * and coverage writes go through the same backfill guard.
 */
import { z } from 'zod';
import { apiFetch, safeJson } from '../system/apiClient';
import { e2eeService } from '../e2ee/e2eeService';
import { E2EEKeyUnavailableError, classifyError } from '../e2ee/e2eeErrors';
import { unwrapGifEnvelope } from '../../utils/messaging/gifEnvelope';
import { ACTIVE_SEARCH_PROFILE, type SearchProfile } from './searchProfile';
import {
  attachSearchRecord,
  canIndexBackfillMessage,
  clearScopeCoverage,
  currentContentRevision,
  getScopeCoverage,
  getSearchRecord,
  indexSearchRecord,
  indexedScopeOf,
  isIndexed,
  matchMessages,
  oldestSearchRecord,
  removeScope,
  setScopeCoverage,
  type AttachOutcome,
  type ScopeCoverage,
  type SearchBackfillGuard,
  type SearchRecord,
} from './searchService';

const DAY_MS = 86_400_000;
export const DEFAULT_RETRY_AFTER_MS = 60_000;
const MAX_RETRY_AFTER_S = 120;

/** Token bucket in front of every bulk request. Shared by every search session. */
export class RequestPacer {
  private tokens: number;
  private last: number;
  private blockedUntilMs = 0;

  constructor(
    private readonly ratePerMinute: number,
    private readonly burst: number,
    private readonly now: () => number = Date.now
  ) {
    this.tokens = burst;
    this.last = now();
  }

  /** When the current 429 block ends; 0 or a past time means none. */
  get blockedUntil(): number {
    return this.blockedUntilMs;
  }

  /** Milliseconds until the next request may go out; 0 means now. */
  delayMs(): number {
    const t = this.now();
    if (t < this.blockedUntilMs) return this.blockedUntilMs - t;
    this.tokens = Math.min(
      this.burst,
      this.tokens + ((t - this.last) * this.ratePerMinute) / 60_000
    );
    this.last = t;
    return this.tokens >= 1 ? 0 : Math.ceil(((1 - this.tokens) * 60_000) / this.ratePerMinute);
  }

  take(): void {
    this.tokens -= 1;
  }

  /**
   * After a 429: nothing until `until`, then at most one request. The block
   * never adds a token, so a run of short 429s cannot lift the rate.
   */
  block(until: number): void {
    this.blockedUntilMs = Math.max(this.blockedUntilMs, until);
    this.tokens = Math.min(this.tokens, 1);
    this.last = Math.max(this.last, until);
  }

  /** A new account starts with a full bucket: the server's limit is per user. */
  reset(): void {
    this.tokens = this.burst;
    this.last = this.now();
    this.blockedUntilMs = 0;
  }
}

export const bulkRequestPacer = new RequestPacer(
  ACTIVE_SEARCH_PROFILE.requestsPerMinute,
  ACTIVE_SEARCH_PROFILE.requestBurst
);

/** `Retry-After` in seconds, clamped to 1–120 s; anything else is 60 s. */
export function parseRetryAfterMs(header: string | null): number {
  if (header === null || header.trim() === '') return DEFAULT_RETRY_AFTER_MS;
  const seconds = Number(header);
  if (!Number.isFinite(seconds) || seconds < 0) return DEFAULT_RETRY_AFTER_MS;
  return Math.min(Math.max(seconds, 1), MAX_RETRY_AFTER_S) * 1000;
}

export function buildBulkUrl(
  scope: string,
  opts: { limit: number; before: string | null; since: string | null }
): string {
  const params = new URLSearchParams({ limit: String(opts.limit) });
  if (opts.before) params.set('before', opts.before);
  if (opts.since) params.set('since', opts.since);
  return `/api/v1/channels/${encodeURIComponent(scope)}/messages/bulk?${params.toString()}`;
}

// Ids are UUIDs (36 characters); the server caps usernames at 32 and display
// names at 100 bytes. A row past these bounds is not an honest row, so the page fails.
// An empty id would become an empty cursor, which drops `before` and fetches the
// newest page again; a time that does not parse would make the next floor NaN.
const WIRE_ID_MAX = 64;
const WIRE_NAME_MAX = 256;
const BulkMessageSchema = z.object({
  id: z.string().min(1).max(WIRE_ID_MAX),
  user_id: z.string().max(WIRE_ID_MAX),
  username: z.string().max(WIRE_NAME_MAX),
  display_name: z.string().max(WIRE_NAME_MAX).nullish(),
  content: z.string(),
  key_version: z.number().int().positive().optional(),
  created_at: z.string().max(WIRE_ID_MAX).datetime({ offset: true }),
  type: z.string().max(WIRE_ID_MAX).nullish(),
});
const BulkPageSchema = z.object({ messages: z.array(BulkMessageSchema) });
type BulkMessage = z.infer<typeof BulkMessageSchema>;

/**
 * A system row (an expiration event, a call event) carries no ciphertext to
 * search. Every ordinary row arrives as `type: 'user'` (the column defaults to
 * it), so an absent or `'user'` type is a message (as in `canEditMessage`).
 */
function isSystemRow(m: BulkMessage): boolean {
  return m.type !== undefined && m.type !== null && m.type !== 'user';
}

type PageResult =
  | { kind: 'ok'; messages: BulkMessage[] }
  | { kind: 'rateLimited'; retryAfterMs: number }
  | { kind: 'noAccess' }
  | { kind: 'failed' };

function isAbort(err: unknown): boolean {
  return err instanceof DOMException && err.name === 'AbortError';
}

async function fetchBulkPage(url: string, signal: AbortSignal): Promise<PageResult> {
  let res: Response;
  try {
    res = await apiFetch(url, { signal });
  } catch (err) {
    if (signal.aborted || isAbort(err)) throw err;
    return { kind: 'failed' };
  }
  if (res.status === 429) {
    return { kind: 'rateLimited', retryAfterMs: parseRetryAfterMs(res.headers.get('Retry-After')) };
  }
  if (res.status === 403 || res.status === 404) return { kind: 'noAccess' };
  if (!res.ok) return { kind: 'failed' };
  try {
    const parsed = BulkPageSchema.safeParse(await safeJson(res));
    return parsed.success ? { kind: 'ok', messages: parsed.data.messages } : { kind: 'failed' };
  } catch {
    return { kind: 'failed' };
  }
}

/**
 * A decrypted row, a row no key will ever open (counted as searched, never
 * shown), or a row that may open later: a key still on its way, a rotation in
 * progress, a fetch that failed. The walk stops before a `retry` row, so the
 * session never records it as searched.
 */
export type DecryptResult =
  { kind: 'ok'; text: string } | { kind: 'unreadable' } | { kind: 'retry' };

const RETRY: DecryptResult = { kind: 'retry' };
const UNREADABLE: DecryptResult = { kind: 'unreadable' };

function isPermanentDecryptFailure(err: unknown): boolean {
  if (err instanceof E2EEKeyUnavailableError) {
    // A revoked epoch heals when the successor is distributed.
    return err.code !== 'REVOKED_EPOCH' && !classifyError(err).retryable;
  }
  // GCM refused the ciphertext, or it is not base64: no key that arrives later opens it.
  return (
    err instanceof DOMException &&
    (err.name === 'OperationError' || err.name === 'InvalidCharacterError')
  );
}

/** Decrypt with the labelled epoch (`[internal]rules/e2ee.md`). */
export async function decryptBulkMessage(msg: BulkMessage, scope: string): Promise<DecryptResult> {
  if (!e2eeService.isInitialized) return RETRY;
  try {
    const text = msg.key_version
      ? await e2eeService.decryptForChannelWithVersion(scope, msg.content, msg.key_version)
      : await e2eeService.decryptForChannel(scope, msg.content);
    return { kind: 'ok', text };
  } catch (err) {
    return isPermanentDecryptFailure(err) ? UNREADABLE : RETRY;
  }
}

export function yieldToMain(): Promise<void> {
  const s = (globalThis as { scheduler?: { yield?: () => Promise<void> } }).scheduler;
  if (typeof s?.yield === 'function') return s.yield();
  return new Promise((resolve) => setTimeout(resolve, 0));
}

function whenVisible(signal: AbortSignal): Promise<void> {
  if (typeof document === 'undefined' || !document.hidden) return Promise.resolve();
  return new Promise((resolve) => {
    const finish = () => {
      document.removeEventListener('visibilitychange', onChange);
      signal.removeEventListener('abort', finish);
      resolve();
    };
    const onChange = () => {
      if (!document.hidden) finish();
    };
    document.addEventListener('visibilitychange', onChange);
    signal.addEventListener('abort', finish, { once: true });
  });
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const timer = setTimeout(done, ms);
    function done() {
      clearTimeout(timer);
      signal.removeEventListener('abort', done);
      resolve();
    }
    signal.addEventListener('abort', done, { once: true });
  });
}

async function paced(pacer: RequestPacer, signal: AbortSignal): Promise<boolean> {
  for (let wait = pacer.delayMs(); wait > 0; wait = pacer.delayMs()) {
    await sleep(wait, signal);
    if (signal.aborted) return false;
  }
  pacer.take();
  return true;
}

interface PageDeps {
  scope: string;
  guard: SearchBackfillGuard;
  signal: AbortSignal;
  profile: SearchProfile;
  /** The content-revision epoch read before this page was fetched. */
  revision: number;
  isDisplayable: (id: string, scope: string) => boolean;
}

interface IndexedPage {
  /** Ids now searchable. */
  searchable: Set<string>;
  /** The index refused a row: nothing from it on was written. */
  full: boolean;
  /** A row may decrypt later: nothing from it on was written. */
  stalled: boolean;
  /** The last row handled before the refusal or the stall, if any. */
  lastHandled: BulkMessage | undefined;
}

/** What became of one row: not indexable, offered to the index, or refused for lack of room. */
type RowOutcome = 'skipped' | 'offered' | 'refused';

const ATTACHED: Record<AttachOutcome, RowOutcome> = {
  attached: 'offered',
  fenced: 'skipped',
  full: 'refused',
};

/**
 * Already searchable in this scope with no decrypt: its record, or a row the
 * chat path indexed here and still shows. The server names the ids on a page,
 * so a row held under another scope is never known here (it is decrypted, then
 * refused).
 */
function isKnown(id: string, deps: PageDeps): boolean {
  const record = getSearchRecord(id);
  if (record) return record.scope === deps.scope;
  return indexedScopeOf(id) === deps.scope && deps.isDisplayable(id, deps.scope);
}

function indexRow(m: BulkMessage, plain: DecryptResult | null, deps: PageDeps): RowOutcome {
  if (
    isSystemRow(m) ||
    plain?.kind !== 'ok' ||
    !canIndexBackfillMessage(deps.guard, m.id, deps.scope)
  ) {
    return 'skipped';
  }
  const { text: content } = unwrapGifEnvelope(plain.text);
  if (!content) return 'skipped';
  const record: SearchRecord = {
    id: m.id,
    scope: deps.scope,
    createdAt: m.created_at,
    authorId: m.user_id,
    username: m.username,
    displayName: m.display_name ?? undefined,
    content,
  };
  if (!isIndexed(m.id)) return indexSearchRecord(record) ? 'offered' : 'refused';
  // The chat path indexed it but no longer shows it: keep the text for display,
  // never the index. Fenced if it changed after the page was fetched.
  return ATTACHED[attachSearchRecord(record, deps.revision, deps.guard)];
}

/** Decrypt one slice and index it into `page`, stopping at the first row the index refuses. */
async function indexSlice(
  slice: readonly BulkMessage[],
  deps: PageDeps,
  page: IndexedPage
): Promise<void> {
  const known = slice.map((m) => isKnown(m.id, deps));
  const plain = await Promise.all(
    slice.map((m, j) =>
      isSystemRow(m) || known[j] ? Promise.resolve(null) : decryptBulkMessage(m, deps.scope)
    )
  );
  if (deps.signal.aborted) return;
  for (const [j, m] of slice.entries()) {
    if (plain[j]?.kind === 'retry') {
      page.stalled = true;
      return;
    }
    const outcome = known[j] ? 'offered' : indexRow(m, plain[j], deps);
    if (outcome === 'refused') {
      page.full = true;
      return;
    }
    // Asked after the row is handled: a delete, an edit or another scope's id is not a hit.
    if (isKnown(m.id, deps)) page.searchable.add(m.id);
    page.lastHandled = m;
  }
}

/** Decrypt and index one page in slices, stopping at the first row the index refuses. */
async function indexPage(messages: readonly BulkMessage[], deps: PageDeps): Promise<IndexedPage> {
  const page: IndexedPage = {
    searchable: new Set(),
    full: false,
    stalled: false,
    lastHandled: undefined,
  };
  let sliceStart = performance.now();
  for (
    let i = 0;
    i < messages.length && !page.full && !page.stalled;
    i += deps.profile.decryptConcurrency
  ) {
    await whenVisible(deps.signal);
    if (deps.signal.aborted || !deps.guard.isCurrent(deps.scope)) break;
    await indexSlice(messages.slice(i, i + deps.profile.decryptConcurrency), deps, page);
    if (deps.signal.aborted) break;
    if (performance.now() - sliceStart >= deps.profile.sliceBudgetMs) {
      await yieldToMain();
      sliceStart = performance.now();
    }
  }
  return page;
}

/** Scopes still being walked: not dropped, not at the start of history, not full. */
function liveScopes(scopes: readonly string[], dead: ReadonlySet<string>): string[] {
  return scopes.filter((s) => {
    const coverage = getScopeCoverage(s);
    return !dead.has(s) && !coverage?.done && !coverage?.full;
  });
}

function coveredAtMs(coverage: ScopeCoverage | undefined): number {
  return coverage?.coveredFrom == null ? Infinity : Date.parse(coverage.coveredFrom);
}

/** "Searched back to": the newest point every live scope has reached; null if one has not started. */
export function reachedAt(scopes: readonly string[]): string | null {
  let newest: string | null = null;
  for (const scope of scopes) {
    const coverage = getScopeCoverage(scope);
    if (coverage?.done) continue;
    if (coverage?.coveredFrom == null) return null;
    if (newest === null || Date.parse(coverage.coveredFrom) > Date.parse(newest)) {
      newest = coverage.coveredFrom;
    }
  }
  return newest;
}

function pickNextScope(scopes: readonly string[], floorMs: number): string | null {
  let best: string | null = null;
  let bestAt = -Infinity;
  for (const scope of scopes) {
    const at = coveredAtMs(getScopeCoverage(scope));
    if (at > floorMs && at > bestAt) {
      best = scope;
      bestAt = at;
    }
  }
  return best;
}

/**
 * The older of two points. A re-anchored walk restarts from a newer row than
 * the one it lost, and "Searched back to" must not move forward while it
 * walks back over ground already searched.
 */
function olderOf(a: string | null, b: string): string {
  return a !== null && Date.parse(a) < Date.parse(b) ? a : b;
}

/**
 * Coverage after one page. It never reaches past a row the index refused or a
 * row that may decrypt later: it claims only up to the last row handled before
 * it. A full scope is not walked again (see `liveScopes`); a stalled one is
 * retried from that row by the next chunk.
 */
function coverageAfter(
  coverage: ScopeCoverage,
  messages: readonly BulkMessage[],
  page: IndexedPage,
  bounds: { limit: number; since: string; probed: boolean }
): ScopeCoverage {
  if (page.full || page.stalled) {
    const last = page.lastHandled;
    if (last === undefined) return page.full ? { ...coverage, full: true } : coverage;
    return {
      cursorId: last.id,
      coveredFrom: olderOf(coverage.coveredFrom, last.created_at),
      done: false,
      ...(page.full ? { full: true } : { reanchoredFrom: coverage.reanchoredFrom }),
      cursorIndexed: isHeld(last.id),
    };
  }
  const oldest = messages.at(-1);
  if (oldest === undefined) return { ...coverage, done: true };
  const short = messages.length < bounds.limit;
  const backAtLost = coverage.reanchoredFrom === oldest.id;
  return {
    cursorId: oldest.id,
    coveredFrom: olderOf(
      coverage.coveredFrom,
      !bounds.probed && short ? bounds.since : oldest.created_at
    ),
    done: backAtLost || (bounds.probed && short),
    cursorIndexed: isHeld(oldest.id),
    reanchoredFrom: backAtLost ? undefined : coverage.reanchoredFrom,
  };
}

function isHeld(id: string): boolean {
  return getSearchRecord(id) !== undefined || isIndexed(id);
}

/**
 * An empty answer for this cursor may be the server not finding a row deleted
 * since: it cannot page past an id it no longer has. A cursor the session held
 * and no longer holds is gone. One it never held (an attachment-only or system
 * row) cannot be told apart from the start of history, so it is checked by
 * walking back to it from a held row; with none held, the empty answer is the
 * start, so a channel with nothing to decrypt never loops.
 */
function cursorGone(scope: string, coverage: ScopeCoverage): boolean {
  const id = coverage.cursorId;
  if (id === null || isHeld(id)) return false;
  return coverage.cursorIndexed === true || oldestSearchRecord(scope) !== undefined;
}

/**
 * Move a lost cursor to the oldest row this session still holds (a search
 * record, or a row the chat shows), keeping how far the scope was searched.
 * With nothing held, forget the coverage so the scope is walked again from the
 * newest message. Never marks it finished.
 * @returns whether the scope can be walked on in this chunk.
 */
function reanchor(
  scope: string,
  coverage: ScopeCoverage,
  guard: SearchBackfillGuard,
  chatAnchor: { id: string; createdAt: string } | undefined
): boolean {
  const record = oldestSearchRecord(scope);
  const anchor =
    record === undefined ||
    (chatAnchor !== undefined && Date.parse(chatAnchor.createdAt) < Date.parse(record.createdAt))
      ? chatAnchor
      : record;
  if (anchor === undefined) {
    clearScopeCoverage(scope, guard);
    return false;
  }
  setScopeCoverage(
    scope,
    {
      ...coverage,
      cursorId: anchor.id,
      cursorIndexed: true,
      reanchoredFrom: coverage.cursorId ?? undefined,
    },
    guard
  );
  return true;
}

/** What a chunk a 429 cut short still owes: its floor and the rest of its budget. */
export interface ChunkResume {
  floorMs: number;
  remaining: number;
}

export type ChunkOutcome =
  | { kind: 'done' }
  | { kind: 'rateLimited'; resumeAt: number; resume?: ChunkResume }
  | { kind: 'failed' };

export interface RunChunkDeps {
  scopes: readonly string[];
  query: string;
  profile: SearchProfile;
  pacer: RequestPacer;
  guard: SearchBackfillGuard;
  signal: AbortSignal;
  /** Scopes that answered 403/404 this session. */
  deadScopes: Set<string>;
  onHits: (ids: string[]) => void;
  onProgress: () => void;
  /**
   * Whether the chat view still shows this row readably. Absent: nothing is,
   * so an indexed row is decrypted and given its own record.
   */
  isDisplayable?: (id: string, scope: string) => boolean;
  /**
   * The oldest row of this scope the chat view shows readably and the index
   * holds. A lost cursor can move back to it when no search record is older.
   */
  oldestDisplayable?: (scope: string) => { id: string; createdAt: string } | undefined;
  now?: () => number;
  /** Finish this cut-short chunk instead of starting a new one. */
  resume?: ChunkResume;
}

interface FetchedWindow {
  page: PageResult;
  /** The page came from the probe without `since`. */
  probed: boolean;
  /** The content-revision epoch read before the page's request. */
  revision: number;
}

/** One window of a scope; an empty window is not the start of history, so ask once without the bound. */
async function fetchWindow(
  scope: string,
  query: { limit: number; before: string | null; since: string },
  deps: RunChunkDeps
): Promise<FetchedWindow | null> {
  if (!(await paced(deps.pacer, deps.signal))) return null;
  let revision = currentContentRevision();
  const page = await fetchBulkPage(buildBulkUrl(scope, query), deps.signal);
  if (page.kind !== 'ok' || page.messages.length > 0) return { page, probed: false, revision };
  if (!(await paced(deps.pacer, deps.signal))) return null;
  revision = currentContentRevision();
  const probe = await fetchBulkPage(buildBulkUrl(scope, { ...query, since: null }), deps.signal);
  return { page: probe, probed: true, revision };
}

const NOT_DISPLAYABLE = () => false;

/** One chunk's bounds and what it has learned so far. */
interface ChunkState {
  floorMs: number;
  since: string;
  skipped: Set<string>;
  /** A scope's cursor is re-anchored at most once per chunk: a second empty answer is the start. */
  reanchored: Set<string>;
  remaining: number;
  /** A scope stopped at a row that may decrypt later; the chunk ends failed. */
  stalled: boolean;
}

/** After one scope: walk on, stop with what is found, or end the chunk with an outcome. */
type ScopeStep = 'next' | 'stop' | ChunkOutcome;

/** What a page the server refused means for the chunk. */
function settleRefusal(
  page: Exclude<PageResult, { kind: 'ok' }>,
  scope: string,
  deps: RunChunkDeps,
  now: () => number
): ScopeStep {
  if (page.kind === 'rateLimited') {
    const resumeAt = now() + page.retryAfterMs;
    deps.pacer.block(resumeAt);
    return { kind: 'rateLimited', resumeAt };
  }
  if (page.kind === 'noAccess') {
    // The server says this user can no longer read the scope: drop what was decrypted from it.
    removeScope(scope);
    if (deps.scopes.length === 1) return { kind: 'failed' };
    deps.deadScopes.add(scope);
    // Every channel refused: nothing was searched, so this is no "Searched all messages".
    return deps.scopes.every((s) => deps.deadScopes.has(s)) ? { kind: 'failed' } : 'next';
  }
  return { kind: 'failed' };
}

/** Fetch, decrypt and index one window of the scope furthest from the floor. */
async function walkNextScope(
  deps: RunChunkDeps,
  state: ChunkState,
  now: () => number
): Promise<ScopeStep> {
  const live = liveScopes(deps.scopes, deps.deadScopes).filter((s) => !state.skipped.has(s));
  const scope = pickNextScope(live, state.floorMs);
  if (scope === null) return 'stop';
  if (!deps.guard.isCurrent(scope)) {
    state.skipped.add(scope);
    return 'next';
  }
  const coverage = getScopeCoverage(scope) ?? { cursorId: null, coveredFrom: null, done: false };
  // An even share of the chunk per live scope, less whatever keeps one message
  // for each scope this chunk has not reached yet. Unreached scopes are walked
  // first (`pickNextScope`), so every live scope gets a request in the chunk
  // while there are no more of them than `chunkMaxMessages` (spec §3.3, D1).
  const unreached = live.filter(
    (s) => s !== scope && coveredAtMs(getScopeCoverage(s)) === Infinity
  );
  const limit = Math.max(
    1,
    Math.min(
      deps.profile.pageSize,
      Math.floor(deps.profile.chunkMaxMessages / live.length),
      state.remaining - unreached.length
    )
  );
  const since = state.since;

  const fetched = await fetchWindow(scope, { limit, before: coverage.cursorId, since }, deps);
  if (fetched === null) return 'stop';
  const { page, probed } = fetched;
  if (page.kind !== 'ok') return settleRefusal(page, scope, deps, now);

  // The client asked for `limit` rows; the server does not get to size the chunk.
  const messages = page.messages.slice(0, limit);
  if (
    probed &&
    messages.length === 0 &&
    cursorGone(scope, coverage) &&
    !state.reanchored.has(scope)
  ) {
    state.reanchored.add(scope);
    if (!reanchor(scope, coverage, deps.guard, deps.oldestDisplayable?.(scope))) {
      state.skipped.add(scope);
    }
    return 'next';
  }
  const indexed = await indexPage(messages, {
    scope,
    guard: deps.guard,
    signal: deps.signal,
    profile: deps.profile,
    revision: fetched.revision,
    isDisplayable: deps.isDisplayable ?? NOT_DISPLAYABLE,
  });
  if (deps.signal.aborted) return 'stop';
  const next = coverageAfter(coverage, messages, indexed, { limit, since, probed });
  // A stall on the first row of the first page leaves nothing searched to record.
  if (next !== coverage) setScopeCoverage(scope, next, deps.guard);
  state.remaining -= messages.length;
  const hits = matchMessages(deps.query, indexed.searchable, scope);
  if (hits.length > 0) deps.onHits(hits);
  deps.onProgress();
  if (indexed.stalled) {
    // The other scopes walk on; this one is retried from the stalled row by the next chunk.
    state.stalled = true;
    state.skipped.add(scope);
  }
  return 'next';
}

export async function runChunk(deps: RunChunkDeps): Promise<ChunkOutcome> {
  const now = deps.now ?? Date.now;
  // A block from an earlier session's 429 still holds: say so instead of
  // showing "Searching…" while the pacer waits it out.
  if (deps.pacer.blockedUntil > now()) {
    return { kind: 'rateLimited', resumeAt: deps.pacer.blockedUntil, resume: deps.resume };
  }
  const floorMs = deps.resume?.floorMs ?? chunkFloor(deps, now);
  const state: ChunkState = {
    floorMs,
    since: new Date(floorMs).toISOString(),
    skipped: new Set(),
    reanchored: new Set(),
    remaining: deps.resume?.remaining ?? deps.profile.chunkMaxMessages,
    stalled: false,
  };

  while (state.remaining > 0 && !deps.signal.aborted && deps.guard.isCurrent()) {
    const step = await walkNextScope(deps, state, now);
    if (step === 'stop') break;
    if (step === 'next') continue;
    // A 429 does not end the chunk: the resume finishes it within the same floor and budget.
    if (step.kind === 'rateLimited') {
      return { ...step, resume: { floorMs: state.floorMs, remaining: state.remaining } };
    }
    return step;
  }
  return state.stalled ? { kind: 'failed' } : { kind: 'done' };
}

/** A new chunk reaches `chunkWindowDays` back from where the live scopes stopped. */
function chunkFloor(deps: RunChunkDeps, now: () => number): number {
  const startAt = reachedAt(liveScopes(deps.scopes, deps.deadScopes));
  return (startAt === null ? now() : Date.parse(startAt)) - deps.profile.chunkWindowDays * DAY_MS;
}
