// Closed-enum tray status contract (spec 2026-10-29-tray-status-design §4).
//
// Shared by the renderer (builds snapshots, dispatches actions), the preload
// (re-validates `tray:action` pushes) and main (validates `tray:setStatus`).
// This module MUST stay import-free: the preload bundle inlines it, and
// `tests/integration/preload-sandbox-contract.test.ts` pins that the bundle
// requires nothing but `electron`.
//
// NO FIELD CAN CARRY FREE TEXT. Every value is a boolean, a member of a frozen
// tuple below, or an integer in 0..99. That is how the no-user-text rule (C4)
// for tray labels, tooltips and `setTitle` is enforced in code rather than by
// convention: nothing a snapshot or action carries can become a string the OS
// shows, because no string slot exists.
//
// The guards accept only a plain object (prototype `Object.prototype` or
// `null`) whose own keys — including symbols and non-enumerables — are EXACTLY
// the expected set, each held as a data property (no accessors, so a value
// cannot change between the check and the read).

export const TRAY_SET_STATUS = 'tray:setStatus';
export const TRAY_ACTION = 'tray:action';

export const TRAY_ICON_STYLES = Object.freeze(['auto', 'light', 'dark', 'colour'] as const);
export type TrayIconStyle = (typeof TRAY_ICON_STYLES)[number];

export const TRAY_AUTH_STATES = Object.freeze(['signed-out', 'signed-in'] as const);
export const TRAY_PRESENCES = Object.freeze(['online', 'dnd', 'invisible', 'offline'] as const);
export const TRAY_CONN_STATES = Object.freeze(['ok', 'reconnecting', 'lost'] as const);
export const TRAY_VOICE_STATES = Object.freeze(['none', 'channel', 'dm-call'] as const);
/** Presences the user can pick from the tray menu. `offline` is observed, never chosen. */
export const TRAY_SETTABLE_PRESENCES = Object.freeze(['online', 'dnd', 'invisible'] as const);
export const TRAY_ACTION_KINDS = Object.freeze([
  'set-muted',
  'set-deafened',
  'leave-voice',
  'stop-share',
  'set-presence',
  'open-unread',
] as const);

/** Upper bound of `unread`; the renderer clamps `computeBadgeTotal()` to it. */
export const TRAY_UNREAD_MAX = 99;

export interface TrayStatusSnapshot {
  v: 1;
  auth: (typeof TRAY_AUTH_STATES)[number];
  presence: (typeof TRAY_PRESENCES)[number];
  conn: (typeof TRAY_CONN_STATES)[number];
  voice: (typeof TRAY_VOICE_STATES)[number];
  speaking: boolean;
  muted: boolean;
  deafened: boolean;
  sharing: boolean;
  /** Mirrors VoiceControls `locked` for the mic (server-muted or policy-paused). */
  micLocked: boolean;
  /** Mirrors VoiceControls `locked` for deafen (server-deafened). */
  deafenLocked: boolean;
  /** Integer 0..99. */
  unread: number;
  mention: boolean;
  reducedMotion: boolean;
  iconStyle: TrayIconStyle;
}

export type TrayAction =
  | { kind: 'set-muted'; value: boolean }
  | { kind: 'set-deafened'; value: boolean }
  | { kind: 'leave-voice' }
  | { kind: 'stop-share' }
  | { kind: 'set-presence'; value: (typeof TRAY_SETTABLE_PRESENCES)[number] }
  | { kind: 'open-unread' };

export type TraySnapshotField = keyof TrayStatusSnapshot;

/**
 * Field-name result of {@link firstInvalidField}. `$root` = not a plain object;
 * `$keys` = an unexpected own key is present. The offending key itself is never
 * returned — it is caller-chosen text and must not reach a log line.
 */
export type TrayInvalidField = TraySnapshotField | '$root' | '$keys';

type FieldCheck = (value: unknown) => boolean;

const isBool: FieldCheck = (value) => typeof value === 'boolean';
const oneOf =
  (tuple: readonly string[]): FieldCheck =>
  (value) =>
    typeof value === 'string' && tuple.includes(value);

// Order is the order fields are checked in, which fixes which name
// `firstInvalidField` reports when several are wrong.
const SNAPSHOT_FIELDS: ReadonlyArray<readonly [TraySnapshotField, FieldCheck]> = Object.freeze([
  ['v', (value: unknown) => value === 1],
  ['auth', oneOf(TRAY_AUTH_STATES)],
  ['presence', oneOf(TRAY_PRESENCES)],
  ['conn', oneOf(TRAY_CONN_STATES)],
  ['voice', oneOf(TRAY_VOICE_STATES)],
  ['speaking', isBool],
  ['muted', isBool],
  ['deafened', isBool],
  ['sharing', isBool],
  ['micLocked', isBool],
  ['deafenLocked', isBool],
  [
    'unread',
    (value: unknown) =>
      typeof value === 'number' &&
      Number.isInteger(value) &&
      value >= 0 &&
      value <= TRAY_UNREAD_MAX,
  ],
  ['mention', isBool],
  ['reducedMotion', isBool],
  ['iconStyle', oneOf(TRAY_ICON_STYLES)],
] as const);

const SNAPSHOT_KEY_SET: ReadonlySet<string> = new Set(SNAPSHOT_FIELDS.map(([name]) => name));

function isPlainRecord(value: unknown): value is Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return false;
  const proto: unknown = Object.getPrototypeOf(value);
  return proto === Object.prototype || proto === null;
}

/**
 * True when every own key of `obj` (strings AND symbols, enumerable or not) is
 * in `allowed`, the count matches, and each is a data property. Together with
 * the per-field checks this is an exact-key-set match.
 */
function hasExactDataKeys(obj: object, allowed: ReadonlySet<string>): boolean {
  const keys = Reflect.ownKeys(obj);
  if (keys.length !== allowed.size) return false;
  for (const key of keys) {
    if (typeof key !== 'string' || !allowed.has(key)) return false;
    const desc = Object.getOwnPropertyDescriptor(obj, key);
    if (desc === undefined || !('value' in desc)) return false;
  }
  return true;
}

/**
 * Name of the first snapshot field that fails validation, or `null` when `v`
 * is a valid {@link TrayStatusSnapshot}. Returns only a name from the fixed
 * field table or one of the fixed tokens `$root` / `$keys` — never a value and
 * never a caller-supplied key — so it is safe for the once-per-session log.
 */
export function firstInvalidField(v: unknown): TrayInvalidField | null {
  if (!isPlainRecord(v)) return '$root';
  for (const [name] of SNAPSHOT_FIELDS) {
    if (!Object.hasOwn(v, name)) return name;
  }
  if (!hasExactDataKeys(v, SNAPSHOT_KEY_SET)) return '$keys';
  for (const [name, check] of SNAPSHOT_FIELDS) {
    if (!check(v[name])) return name;
  }
  return null;
}

export function isTrayStatusSnapshot(v: unknown): v is TrayStatusSnapshot {
  return firstInvalidField(v) === null;
}

const KIND_ONLY: ReadonlySet<string> = new Set(['kind']);
const KIND_VALUE: ReadonlySet<string> = new Set(['kind', 'value']);

export function isTrayAction(v: unknown): v is TrayAction {
  if (!isPlainRecord(v)) return false;
  const kind = v.kind;
  if (typeof kind !== 'string' || !(TRAY_ACTION_KINDS as readonly string[]).includes(kind))
    return false;
  switch (kind as TrayAction['kind']) {
    case 'set-muted':
    case 'set-deafened':
      return hasExactDataKeys(v, KIND_VALUE) && isBool(v.value);
    case 'set-presence':
      return hasExactDataKeys(v, KIND_VALUE) && oneOf(TRAY_SETTABLE_PRESENCES)(v.value);
    case 'leave-voice':
    case 'stop-share':
    case 'open-unread':
      return hasExactDataKeys(v, KIND_ONLY);
  }
}
