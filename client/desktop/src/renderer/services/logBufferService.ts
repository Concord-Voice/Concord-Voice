/**
 * Ring-buffer log capture for bug reports (#158).
 *
 * `install()` monkey-patches `console.log`, `console.warn`, and `console.error`
 * to ALSO push a sanitized snapshot of the call into an in-memory ring buffer.
 * The original console methods still fire (so DevTools and Electron log forwarding
 * are unaffected); we just shadow-record their args.
 *
 * The buffer is capped at MAX_ENTRIES (500). On overflow the oldest entry is
 * dropped. Snapshots are PII-scrubbed at capture time, not at read time — this
 * means we never hold the raw payload in memory, so any future feature that
 * reads the buffer (bug-report submit, in-app log viewer, crash dump) gets the
 * sanitized form by construction.
 *
 * **Why client-side scrub matters:** even though the server re-sanitizes as
 * defense-in-depth, the buffer is process-resident on the user's machine until
 * GC or app close. Any in-process disclosure (devtools snapshot, crash dump,
 * future telemetry) inherits the scrub.
 */

const MAX_ENTRIES = 500;

export type LogLevel = 'log' | 'warn' | 'error';

export interface LogEntry {
  ts: number;
  level: LogLevel;
  message: string;
}

interface OriginalConsole {
  log: typeof console.log;
  warn: typeof console.warn;
  error: typeof console.error;
}

let buffer: LogEntry[] = [];
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
  buffer.push({ ts: Date.now(), level, message });
  if (buffer.length > MAX_ENTRIES) {
    buffer.splice(0, buffer.length - MAX_ENTRIES);
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
  };
  const levels: LogLevel[] = ['log', 'warn', 'error'];
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
  installed = false;
  original = null;
}

/**
 * Snapshot of the current buffer. Returns a defensive copy so callers cannot
 * mutate the internal storage. Empty array when nothing has been captured yet
 * or `install()` was never called.
 */
export function getEntries(): LogEntry[] {
  return buffer.slice();
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
  buffer = [];
}
