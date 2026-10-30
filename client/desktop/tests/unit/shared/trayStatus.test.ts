import { describe, it, expect } from 'vitest';
import {
  TRAY_ACTION,
  TRAY_ACTION_KINDS,
  TRAY_ICON_STYLES,
  TRAY_PRESENCES,
  TRAY_SET_STATUS,
  TRAY_UNREAD_MAX,
  firstInvalidField,
  isTrayAction,
  isTrayStatusSnapshot,
  type TrayStatusSnapshot,
} from '@/shared/trayStatus';

const valid: TrayStatusSnapshot = {
  v: 1,
  auth: 'signed-in',
  presence: 'online',
  conn: 'ok',
  voice: 'channel',
  speaking: false,
  muted: false,
  deafened: false,
  sharing: false,
  micLocked: false,
  deafenLocked: false,
  unread: 3,
  mention: false,
  reducedMotion: false,
  iconStyle: 'auto',
};

const with_ = (patch: Record<string, unknown>): Record<string, unknown> => ({ ...valid, ...patch });

describe('tray contract constants', () => {
  it('pins the channel names', () => {
    expect(TRAY_SET_STATUS).toBe('tray:setStatus');
    expect(TRAY_ACTION).toBe('tray:action');
  });

  it('freezes every enum tuple so a consumer cannot widen the guard at runtime', () => {
    expect(TRAY_ICON_STYLES).toEqual(['auto', 'light', 'dark', 'colour']);
    expect(Object.isFrozen(TRAY_ICON_STYLES)).toBe(true);
    expect(Object.isFrozen(TRAY_PRESENCES)).toBe(true);
    expect(Object.isFrozen(TRAY_ACTION_KINDS)).toBe(true);
    expect(TRAY_UNREAD_MAX).toBe(99);
  });
});

describe('isTrayStatusSnapshot', () => {
  it('accepts a valid snapshot', () => {
    expect(isTrayStatusSnapshot(valid)).toBe(true);
    expect(firstInvalidField(valid)).toBeNull();
  });

  it('accepts the unread bounds 0 and 99 and a null-prototype object', () => {
    expect(isTrayStatusSnapshot(with_({ unread: 0 }))).toBe(true);
    expect(isTrayStatusSnapshot(with_({ unread: 99 }))).toBe(true);
    const bare = Object.assign(Object.create(null) as object, valid);
    expect(isTrayStatusSnapshot(bare)).toBe(true);
  });

  it('accepts every icon style', () => {
    for (const iconStyle of TRAY_ICON_STYLES) {
      expect(isTrayStatusSnapshot(with_({ iconStyle }))).toBe(true);
    }
  });

  it.each<[string, unknown]>([
    ['an extra key', with_({ name: 'alice' })],
    [
      'a missing key',
      (() => {
        const { mention: _m, ...rest } = valid;
        return rest;
      })(),
    ],
    ['v: 2', with_({ v: 2 })],
    ['a string in a boolean slot', with_({ muted: 'true' })],
    ["presence: 'away'", with_({ presence: 'away' })],
    ['unread: 100', with_({ unread: 100 })],
    ['unread: 1.5', with_({ unread: 1.5 })],
    ['unread: -1', with_({ unread: -1 })],
    ['unread: NaN', with_({ unread: Number.NaN })],
    ["unread: '3'", with_({ unread: '3' })],
    ["iconStyle: 'neon'", with_({ iconStyle: 'neon' })],
    ["conn: 'down'", with_({ conn: 'down' })],
    ["voice: 'group'", with_({ voice: 'group' })],
    ["auth: 'guest'", with_({ auth: 'guest' })],
    ['an array', [valid]],
    ['null', null],
    ['undefined', undefined],
    ['a string', 'tray'],
  ])('rejects %s', (_label, input) => {
    expect(isTrayStatusSnapshot(input)).toBe(false);
    expect(firstInvalidField(input)).not.toBeNull();
  });

  it('rejects an object carrying an own __proto__ key', () => {
    const poisoned = JSON.parse(
      '{"__proto__":{"x":1},' + JSON.stringify(valid).slice(1)
    ) as unknown;
    expect(Object.prototype.hasOwnProperty.call(poisoned, '__proto__')).toBe(true);
    expect(isTrayStatusSnapshot(poisoned)).toBe(false);
  });

  it('rejects a class instance (prototype is not Object.prototype)', () => {
    class Snap {}
    expect(isTrayStatusSnapshot(Object.assign(new Snap(), valid))).toBe(false);
  });

  it('rejects an accessor property even when it returns a valid value', () => {
    const tricky: Record<string, unknown> = { ...valid };
    Object.defineProperty(tricky, 'muted', { get: () => false, enumerable: true });
    expect(isTrayStatusSnapshot(tricky)).toBe(false);
  });

  it('rejects a symbol-keyed extra property', () => {
    const tricky: Record<string | symbol, unknown> = { ...valid, [Symbol('x')]: 1 };
    expect(isTrayStatusSnapshot(tricky)).toBe(false);
  });
});

describe('firstInvalidField', () => {
  it("names the field for unread: 100 → 'unread'", () => {
    expect(firstInvalidField(with_({ unread: 100 }))).toBe('unread');
  });

  it('names the missing field from the fixed key table', () => {
    const { mention: _m, ...rest } = valid;
    expect(firstInvalidField(rest)).toBe('mention');
  });

  it('never echoes an attacker-chosen key name for an extra key', () => {
    const field = firstInvalidField(with_({ 'drop table; <script>': 1 }));
    expect(field).toBe('$keys');
  });

  it("returns '$root' for a non-object", () => {
    expect(firstInvalidField(null)).toBe('$root');
    expect(firstInvalidField([valid])).toBe('$root');
  });

  it('only ever returns a known field name or a fixed token', () => {
    const allowed = new Set([...Object.keys(valid), '$root', '$keys']);
    const inputs: unknown[] = [
      with_({ presence: 'away' }),
      with_({ iconStyle: 'neon' }),
      with_({ zzz: 1 }),
      with_({ speaking: 1 }),
      42,
    ];
    for (const input of inputs) {
      const field = firstInvalidField(input);
      expect(field).not.toBeNull();
      expect(allowed.has(field as string)).toBe(true);
    }
  });
});

describe('isTrayAction', () => {
  it.each<[unknown]>([
    [{ kind: 'set-muted', value: true }],
    [{ kind: 'set-muted', value: false }],
    [{ kind: 'set-deafened', value: true }],
    [{ kind: 'leave-voice' }],
    [{ kind: 'stop-share' }],
    [{ kind: 'set-presence', value: 'online' }],
    [{ kind: 'set-presence', value: 'dnd' }],
    [{ kind: 'set-presence', value: 'invisible' }],
    [{ kind: 'open-unread' }],
  ])('accepts %j', (input) => {
    expect(isTrayAction(input)).toBe(true);
  });

  it.each<[string, unknown]>([
    ['set-muted without value', { kind: 'set-muted' }],
    ['set-muted with a string value', { kind: 'set-muted', value: 'true' }],
    ['set-deafened with a number value', { kind: 'set-deafened', value: 1 }],
    ["set-presence 'offline' (not user-selectable)", { kind: 'set-presence', value: 'offline' }],
    ['set-presence without value', { kind: 'set-presence' }],
    ['an unknown kind', { kind: 'evil' }],
    ['leave-voice with an extra key', { kind: 'leave-voice', value: true }],
    ['open-unread carrying a target id', { kind: 'open-unread', targetId: 'abc' }],
    ['set-muted with an extra key', { kind: 'set-muted', value: true, extra: 1 }],
    ['an array', [{ kind: 'leave-voice' }]],
    ['null', null],
    ['a string', 'leave-voice'],
  ])('rejects %s', (_label, input) => {
    expect(isTrayAction(input)).toBe(false);
  });

  it('rejects an own __proto__ key on an action', () => {
    const poisoned = JSON.parse('{"__proto__":{"x":1},"kind":"leave-voice"}') as unknown;
    expect(isTrayAction(poisoned)).toBe(false);
  });
});
