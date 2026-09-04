/**
 * Ring-buffer log capture for bug reports (#158).
 *
 * `install()` monkey-patches `console.log`, `console.warn`, `console.error`,
 * `console.debug`, and `console.info` to ALSO push a sanitized snapshot of the
 * call into an in-memory ring buffer.
 * The original console methods still fire (so DevTools and Electron log forwarding
 * are unaffected); we just shadow-record their args.
 *
 * Capture is split into two independent ring buffers by priority so a burst of
 * high-volume `debug`/`info` right before a crash cannot evict the triage-critical
 * `warn`/`error` lines (which, in a single shared FIFO, they otherwise would):
 *   - priority buffer — `warn`, `error` — capped at MAX_PRIORITY_ENTRIES
 *   - general buffer  — `log`, `debug`, `info` — capped at MAX_GENERAL_ENTRIES
 * Each segment drops its own oldest entry on overflow; `getEntries()` merges the
 * two back into a single chronological view (ties broken by capture order).
 *
 * Snapshots are PII-scrubbed at capture time, not at read time — this means we
 * never hold the raw payload in memory, so any future feature that reads the
 * buffer (bug-report submit, in-app log viewer, crash dump) gets the sanitized
 * form by construction.
 *
 * **Why client-side scrub matters:** even though the server re-sanitizes as
 * defense-in-depth, the buffer is process-resident on the user's machine until
 * GC or app close. Any in-process disclosure (devtools snapshot, crash dump,
 * future telemetry) inherits the scrub.
 */

// General levels (`log`/`debug`/`info`) keep the historical 500-entry window.
// `warn`/`error` get a separate reserved window so a high-volume debug/info
// burst can never push them out; the reserve is additive, not carved out of the
// general capacity, so common-case log retention is unchanged.
const MAX_GENERAL_ENTRIES = 500;
const MAX_PRIORITY_ENTRIES = 250;

export type LogLevel = 'log' | 'warn' | 'error' | 'debug' | 'info';

// Levels that must survive a debug/info flood for triage.
const PRIORITY_LEVELS: ReadonlySet<LogLevel> = new Set<LogLevel>(['warn', 'error']);

export interface LogEntry {
  ts: number;
  level: LogLevel;
  message: string;
}

// Stored form carries a monotonic sequence number so the merged, chronological
// view in getEntries() preserves true capture order even for entries that share
// a millisecond timestamp. `seq` is internal and never exposed via LogEntry.
interface StoredEntry extends LogEntry {
  seq: number;
}

interface OriginalConsole {
  log: typeof console.log;
  warn: typeof console.warn;
  error: typeof console.error;
  debug: typeof console.debug;
  info: typeof console.info;
}

let generalBuffer: StoredEntry[] = [];
let priorityBuffer: StoredEntry[] = [];
let seqCounter = 0;
let installed = false;
let original: OriginalConsole | null = null;

// ─── PII Scrub ────────────────────────────────────────────────────────────
//
// Each pattern targets a category that bug reports could legitimately carry.
// The replacement tokens (`<email>`, `<jwt>`, etc.) are intentionally readable
// so a triager can tell what was redacted vs missing.
//
// Order matters: longest/most-specific patterns first, so that (e.g.) a JWT
// inside a URL gets caught as JWT, not as part of the URL.

// Per-pattern upper bound for unbounded quantifiers (Sonar typescript:S5852,
// ReDoS-defense). Comfortably above realistic worst-case sizes for each
// category (a real JWT is ~300 bytes; a real path is ~256 chars). Bounding
// prevents catastrophic backtracking on adversarial inputs.
const PATTERN_MAX = 4096;

const PATTERNS: ReadonlyArray<{ re: RegExp; replacement: string }> = [
  // JWT — three base64url segments separated by dots
  {
    re: new RegExp(
      String.raw`eyJ[A-Za-z0-9_-]{10,${PATTERN_MAX}}\.[A-Za-z0-9_-]{10,${PATTERN_MAX}}\.[A-Za-z0-9_-]{10,${PATTERN_MAX}}`,
      'g'
    ),
    replacement: '<jwt>',
  },
  // Bearer tokens
  {
    re: new RegExp(String.raw`\bBearer\s+[A-Za-z0-9._~+/=-]{10,${PATTERN_MAX}}`, 'gi'),
    replacement: 'Bearer <token>',
  },
  // Email
  {
    re: new RegExp(
      String.raw`[A-Za-z0-9._%+-]{1,${PATTERN_MAX}}@[A-Za-z0-9.-]{1,${PATTERN_MAX}}\.[A-Za-z]{2,16}`,
      'g'
    ),
    replacement: '<email>',
  },
  // Filesystem paths containing usernames — POSIX form
  {
    re: new RegExp(String.raw`/Users/[^/\s"']{1,${PATTERN_MAX}}`, 'g'),
    replacement: '/Users/<user>',
  },
  {
    re: new RegExp(String.raw`/home/[^/\s"']{1,${PATTERN_MAX}}`, 'g'),
    replacement: '/home/<user>',
  },
  // Filesystem paths — Windows form
  {
    re: new RegExp(String.raw`[A-Z]:\\Users\\[^\\/\s"']{1,${PATTERN_MAX}}`, 'g'),
    replacement: String.raw`C:\Users\<user>`,
  },
  // IPv4
  { re: /\b(?:\d{1,3}\.){3}\d{1,3}\b/g, replacement: '<ip>' },
  // IPv6 — match colon-separated hex groups (lenient; covers compressed forms)
  { re: /\b(?:[0-9a-fA-F]{1,4}:){2,7}[0-9a-fA-F]{1,4}\b/g, replacement: '<ip>' },
  // Long hex strings (hashes, raw token bytes) — 32+ hex chars
  { re: new RegExp(String.raw`\b[0-9a-fA-F]{32,${PATTERN_MAX}}\b`, 'g'), replacement: '<hex>' },
  // Long base64 strings (keys, encrypted blobs) — 40+ url-safe-base64 chars
  {
    re: new RegExp(String.raw`\b[A-Za-z0-9+/=_-]{40,${PATTERN_MAX}}\b`, 'g'),
    replacement: '<base64>',
  },

  // ─── TURN relay credentials (#3104) ─────────────────────────────────────
  //
  // These four entries sit AFTER the long-base64 entry deliberately: a longer
  // blob must be consumed as <base64> first, so the narrow 28-char band at the
  // end can never chop a fragment out of the middle of one.
  //
  // Why they are needed at all: the control plane mints ephemeral coturn
  // credentials per voice join (services/control-plane/pkg/config/turn.go).
  // The username is `<unix expiry>:<userID>` and the credential is base64 of an
  // HMAC-SHA1 digest — 20 bytes, which encode to EXACTLY 27 base64 characters
  // plus one '=' pad. 28 characters is under the 40-char floor above, so the
  // credential passed the whole scrub untouched, and `pseudonymizeLogUuids`
  // (submit time) rescued only the UUID half of the username. These are live
  // relay credentials with a 24 h TTL and the buffer feeds a bug report that
  // reaches a PUBLIC repo.

  // Key-shaped redaction first (most specific, and shape-independent — it
  // still holds if the control plane changes the credential algorithm).
  // `stringifyArg` serializes an RTCIceServer with JSON.stringify, giving
  // `"credential":"…"`; a pre-formatted util.inspect string gives
  // `credential: '…'`. Both forms, and both `credential` and `password`,
  // with an optional identifier prefix (`turnCredential`, `newPassword`).
  //
  // The two entries are split by whether the KEY is quoted, and the bare-key
  // one accepts EITHER quote style around its value — `credential: 'x'` from
  // util.inspect, `credential: "x"` from a hand-formatted or JS-object-literal
  // line. That last form used to be redacted by NOTHING: the JSON entry
  // requires a quoted key and this one required a single-quoted value, so a
  // bare key with a double-quoted value satisfied neither. It is written as
  // two complete alternation branches rather than one widened `["']` class so
  // that each branch's value class excludes only its OWN quote — a shared
  // class would stop at an embedded foreign quote and leak the rest of the
  // value. Both branches keep the PATTERN_MAX bound (Sonar S5852), and neither
  // is ambiguous with its closing quote, so the alternation adds no
  // backtracking. The Go backstop carries the same shape for the same reason;
  // see services/control-plane/internal/feedback/sanitize.go.
  {
    re: new RegExp(
      String.raw`"([^"\\]{0,32}(?:credential|password))"\s{0,8}:\s{0,8}"[^"\\]{0,${PATTERN_MAX}}"`,
      'gi'
    ),
    replacement: '"$1":"<redacted>"',
  },
  {
    re: new RegExp(
      String.raw`([A-Za-z0-9_$]{0,32}(?:credential|password))\s{0,8}:\s{0,8}(?:'[^'\\]{0,${PATTERN_MAX}}'|"[^"\\]{0,${PATTERN_MAX}}")`,
      'gi'
    ),
    replacement: "$1: '<redacted>'",
  },
  // TURN REST-API username — `<unix expiry>:<userID>` (turn.go:31). 9-12
  // digits covers every plausible second-precision expiry; the userID half is
  // a UUID. Redacting the whole token here is what removes the expiry, which
  // the submit-time UUID pseudonymizer never touched.
  {
    re: /\b\d{9,12}:[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b/g,
    replacement: '<turn-username>',
  },
  // TURN REST-API credential — the 28-char band the 40-char floor misses.
  // The lookaround pair pins it to a STANDALONE 27-chars-plus-one-pad token:
  // inside any longer padded token every 27-char window is preceded by another
  // base64 character, so a 32-char token (base64 of 23 bytes) does not match.
  // Deliberately replaced with the EXISTING <base64> token rather than a
  // TURN-specific one — the shape is base64-of-20-bytes, not proof of a TURN
  // credential, and labelling an unrelated 20-byte digest `<turn-credential>`
  // would be a false tripwire. `<turn-username>` above is the tripwire: it has
  // no other plausible producer, and a TURN entry always carries both.
  {
    re: /(?<![A-Za-z0-9+/])[A-Za-z0-9+/]{27}=(?![A-Za-z0-9+/=])/g,
    replacement: '<base64>',
  },
];

/**
 * Sanitize a string by running each PII pattern in order. Returns the
 * sanitized copy; the input is not mutated. Empty / non-string inputs are
 * coerced to '' so callers can safely chain.
 */
export function sanitize(input: string): string {
  if (typeof input !== 'string' || input.length === 0) return '';
  let out = input;
  for (const { re, replacement } of PATTERNS) {
    out = out.replace(re, replacement);
  }
  return out;
}

/**
 * Best-effort serializer for console call args. Strings pass through.
 * Errors are formatted as `name: message\nstack`. Other values JSON.stringify;
 * if that throws (circular refs, BigInt, etc.) we fall back to String(value).
 */
function stringifyArg(value: unknown): string {
  if (typeof value === 'string') return value;
  if (value instanceof Error) {
    const stack = value.stack ? `\n${value.stack}` : '';
    return `${value.name}: ${value.message}${stack}`;
  }
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

function record(level: LogLevel, args: unknown[]): void {
  const message = sanitize(args.map(stringifyArg).join(' '));
  const entry: StoredEntry = { ts: Date.now(), level, message, seq: seqCounter++ };
  if (PRIORITY_LEVELS.has(level)) {
    priorityBuffer.push(entry);
    if (priorityBuffer.length > MAX_PRIORITY_ENTRIES) {
      priorityBuffer.splice(0, priorityBuffer.length - MAX_PRIORITY_ENTRIES);
    }
  } else {
    generalBuffer.push(entry);
    if (generalBuffer.length > MAX_GENERAL_ENTRIES) {
      generalBuffer.splice(0, generalBuffer.length - MAX_GENERAL_ENTRIES);
    }
  }
}

/**
 * Install the buffer by shadow-wrapping console methods. Idempotent — calling
 * twice is a no-op. Call once during renderer bootstrap (main.tsx).
 *
 * The shadow does NOT replace the originals — both call paths fire. DevTools
 * still sees every log line; the ring buffer just keeps a sanitized parallel
 * record.
 */
export function install(): void {
  if (installed) return;
  installed = true;
  // Capture bound originals so the shadow wrapper can still fire them.
  const c = console as Record<LogLevel, (...args: unknown[]) => void>;
  original = {
    log: c.log.bind(console),
    warn: c.warn.bind(console),
    error: c.error.bind(console),
    debug: c.debug.bind(console),
    info: c.info.bind(console),
  };
  const levels: LogLevel[] = ['log', 'warn', 'error', 'debug', 'info'];
  for (const level of levels) {
    const orig = original[level];
    c[level] = (...args: unknown[]) => {
      try {
        record(level, args);
      } catch {
        // never let the buffer break the console call
      }
      orig(...args);
    };
  }
}

/**
 * Uninstall the buffer — restores the original console methods. Intended for
 * test teardown; production code never calls this.
 */
export function uninstall(): void {
  if (!installed || !original) return;
  const c = console as Record<LogLevel, (...args: unknown[]) => void>;
  c.log = original.log;
  c.warn = original.warn;
  c.error = original.error;
  c.debug = original.debug;
  c.info = original.info;
  installed = false;
  original = null;
}

/**
 * Snapshot of the current buffer. Returns a defensive copy so callers cannot
 * mutate the internal storage. Empty array when nothing has been captured yet
 * or `install()` was never called.
 */
export function getEntries(): LogEntry[] {
  return [...priorityBuffer, ...generalBuffer]
    .sort((a, b) => a.ts - b.ts || a.seq - b.seq)
    .map(({ ts, level, message }) => ({ ts, level, message }));
}

/**
 * Format the entries as one line per entry: `ISO  [level]  message`.
 * Suitable for direct embedding in a bug-report payload. Returns an empty
 * string when the buffer is empty.
 */
export function formatEntries(entries: LogEntry[] = getEntries()): string {
  return entries
    .map((e) => `${new Date(e.ts).toISOString()}  [${e.level}]  ${e.message}`)
    .join('\n');
}

/**
 * Clear the buffer. Test-only — production code does not call this.
 */
export function _resetForTest(): void {
  generalBuffer = [];
  priorityBuffer = [];
  seqCounter = 0;
}
