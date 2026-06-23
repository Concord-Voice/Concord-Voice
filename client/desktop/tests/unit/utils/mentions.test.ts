import { describe, it, expect } from 'vitest';
import {
  parseMentions,
  buildAddendum,
  encodeMentionMeta,
  segmentMessage,
  type MentionResolveContext,
  type ParsedMention,
} from '@/renderer/utils/mentions';

function makeCtx(overrides?: Partial<MentionResolveContext>): MentionResolveContext {
  return {
    members: new Map([
      ['alice', 'user-1'],
      ['bob', 'user-2'],
      ['charlie', 'user-3'],
    ]),
    displayNames: new Map([
      ['alice wonderland', 'user-1'],
      ['bob builder', 'user-2'],
    ]),
    roles: new Map([
      ['admin', 'role-admin'],
      ['moderator', 'role-mod'],
    ]),
    ...overrides,
  };
}

describe('parseMentions', () => {
  it('parses @all as everyone', () => {
    const m = parseMentions('Hey @all check', makeCtx());
    expect(m).toHaveLength(1);
    expect(m[0]).toMatchObject({ type: 'everyone', raw: '@all', label: 'all', start: 4, end: 8 });
  });

  it('parses @everyone as everyone', () => {
    expect(parseMentions('Hello @everyone', makeCtx())[0].type).toBe('everyone');
  });

  it('parses @here', () => {
    expect(parseMentions('Anyone @here?', makeCtx())[0]).toMatchObject({
      type: 'here',
      label: 'here',
    });
  });

  it('parses @online as here', () => {
    expect(parseMentions('@online respond', makeCtx())[0].type).toBe('here');
  });

  it('parses @username as user', () => {
    const m = parseMentions('Hey @alice what?', makeCtx());
    expect(m[0]).toMatchObject({ type: 'user', id: 'user-1', label: 'alice' });
  });

  it('resolves case-insensitively', () => {
    expect(parseMentions('@Alice', makeCtx())[0].id).toBe('user-1');
  });

  it('parses @rolename as role', () => {
    expect(parseMentions('@admin review', makeCtx())[0]).toMatchObject({
      type: 'role',
      id: 'role-admin',
    });
  });

  it('resolves roles case-insensitively', () => {
    expect(parseMentions('@Moderator', makeCtx())[0].id).toBe('role-mod');
  });

  it('resolves display names', () => {
    const ctx = makeCtx({ members: new Map(), displayNames: new Map([['john.doe', 'user-john']]) });
    expect(parseMentions('@john.doe', ctx)[0]).toMatchObject({ type: 'user', id: 'user-john' });
  });

  it('handles <@userId> token format', () => {
    expect(parseMentions('<@user-1>', makeCtx())[0]).toMatchObject({ type: 'user', id: 'user-1' });
  });

  it('handles <@&roleId> token format', () => {
    expect(parseMentions('<@&role-admin>', makeCtx())[0]).toMatchObject({
      type: 'role',
      id: 'role-admin',
    });
  });

  it('parses multiple mentions', () => {
    const m = parseMentions('@alice and @bob check @all', makeCtx());
    expect(m).toHaveLength(3);
  });

  it('returns empty for no mentions', () => {
    expect(parseMentions('Just a message', makeCtx())).toHaveLength(0);
  });

  it('ignores unrecognized mentions', () => {
    expect(parseMentions('@unknownuser', makeCtx())).toHaveLength(0);
  });

  it('records correct positions', () => {
    const text = 'Hello @alice!';
    const m = parseMentions(text, makeCtx());
    expect(text.slice(m[0].start, m[0].end)).toBe('@alice');
  });

  it('returns empty for empty text', () => {
    expect(parseMentions('', makeCtx())).toHaveLength(0);
  });

  it('role takes priority over display name', () => {
    const ctx = makeCtx({
      roles: new Map([['admin', 'role-admin']]),
      members: new Map(),
      displayNames: new Map([['admin', 'user-admin']]),
    });
    expect(parseMentions('@admin', ctx)[0].type).toBe('role');
  });

  it('username takes priority over display name', () => {
    const ctx = makeCtx({
      members: new Map([['test', 'u1']]),
      displayNames: new Map([['test', 'u2']]),
      roles: new Map(),
    });
    expect(parseMentions('@test', ctx)[0].id).toBe('u1');
  });
});

describe('buildAddendum', () => {
  it('returns null for empty', () => {
    expect(buildAddendum([])).toBeNull();
  });

  it('builds user addendum', () => {
    const m: ParsedMention[] = [
      { type: 'user', raw: '@a', start: 0, end: 2, id: 'u1', label: 'a' },
      { type: 'user', raw: '@b', start: 3, end: 5, id: 'u2', label: 'b' },
    ];
    expect(buildAddendum(m)).toEqual({ u: ['u1', 'u2'] });
  });

  it('builds role addendum', () => {
    const m: ParsedMention[] = [
      { type: 'role', raw: '@r', start: 0, end: 2, id: 'r1', label: 'r' },
    ];
    expect(buildAddendum(m)).toEqual({ r: ['r1'] });
  });

  it('builds everyone addendum', () => {
    expect(
      buildAddendum([{ type: 'everyone', raw: '@all', start: 0, end: 4, label: 'all' }])
    ).toEqual({ e: true });
  });

  it('builds here addendum', () => {
    expect(
      buildAddendum([{ type: 'here', raw: '@here', start: 0, end: 5, label: 'here' }])
    ).toEqual({ h: true });
  });

  it('deduplicates users', () => {
    const m: ParsedMention[] = [
      { type: 'user', raw: '@a', start: 0, end: 2, id: 'u1', label: 'a' },
      { type: 'user', raw: '@a', start: 3, end: 5, id: 'u1', label: 'a' },
    ];
    expect(buildAddendum(m)!.u).toEqual(['u1']);
  });

  it('builds mixed addendum', () => {
    const m: ParsedMention[] = [
      { type: 'user', raw: '@a', start: 0, end: 2, id: 'u1', label: 'a' },
      { type: 'role', raw: '@r', start: 3, end: 5, id: 'r1', label: 'r' },
      { type: 'everyone', raw: '@all', start: 6, end: 10, label: 'all' },
      { type: 'here', raw: '@here', start: 11, end: 16, label: 'here' },
    ];
    expect(buildAddendum(m)).toEqual({ u: ['u1'], r: ['r1'], e: true, h: true });
  });

  it('returns null for mentions without IDs', () => {
    expect(buildAddendum([{ type: 'user', raw: '@g', start: 0, end: 2, label: 'g' }])).toBeNull();
  });
});

describe('encodeMentionMeta', () => {
  it('encodes to base64 msgpack', () => {
    const encoded = encodeMentionMeta({ u: ['u1'], e: true });
    expect(typeof encoded).toBe('string');
    expect(() => atob(encoded)).not.toThrow();
  });

  it('encodes empty addendum', () => {
    expect(encodeMentionMeta({}).length).toBeGreaterThan(0);
  });
});

describe('segmentMessage', () => {
  it('single text segment without mentions', () => {
    expect(segmentMessage('Hello', makeCtx())).toEqual([{ type: 'text', content: 'Hello' }]);
  });

  it('mention at start', () => {
    const s = segmentMessage('@alice hi', makeCtx());
    expect(s[0].type).toBe('mention');
    expect(s[1]).toEqual({ type: 'text', content: ' hi' });
  });

  it('mention at end', () => {
    const s = segmentMessage('Hi @alice', makeCtx());
    expect(s[0]).toEqual({ type: 'text', content: 'Hi ' });
    expect(s[1].type).toBe('mention');
  });

  it('mention in middle', () => {
    const s = segmentMessage('Hey @alice check', makeCtx());
    expect(s).toHaveLength(3);
    expect(s[1].type).toBe('mention');
  });

  it('multiple mentions', () => {
    const s = segmentMessage('@alice and @bob', makeCtx());
    expect(s).toHaveLength(3);
  });

  it('empty text', () => {
    expect(segmentMessage('', makeCtx())).toEqual([{ type: 'text', content: '' }]);
  });

  it('mention-only text', () => {
    const s = segmentMessage('@alice', makeCtx());
    expect(s).toHaveLength(1);
    expect(s[0].type).toBe('mention');
  });
});
