import { readFileSync } from 'node:fs';
import path from 'node:path';
import { describe, expect, it } from 'vitest';
import { decodeVoiceCommand, isVersionedVoiceCommand } from '../src/lib/voiceCommand.js';

const streamGeneration = '11111111-1111-4111-8111-111111111111';
const fixtures = JSON.parse(
  readFileSync(
    path.resolve(
      import.meta.dirname,
      '../../control-plane/internal/voicecommand/testdata/envelopes.json'
    ),
    'utf8'
  )
) as {
  valid: Array<{ name: string; command: Record<string, unknown> }>;
  invalid: Array<{ name: string; command: Record<string, unknown> }>;
};
const encode = (value: unknown): Uint8Array => new TextEncoder().encode(JSON.stringify(value));
const schemaError = /^Invalid voice command schema$/;

describe('decodeVoiceCommand shared Go/TypeScript wire contract', () => {
  it.each(fixtures.valid)('accepts $name without coercing frozen identities', ({ command }) => {
    expect(decodeVoiceCommand(encode(command), streamGeneration)).toEqual(command);
  });

  it.each(fixtures.invalid)('rejects $name at the schema boundary', ({ command }) => {
    expect(() => decodeVoiceCommand(encode(command), streamGeneration)).toThrow(schemaError);
  });

  it('keeps BIGINT sequence and authorization revision exact beyond Number precision', () => {
    const command = decodeVoiceCommand(encode(fixtures.valid[0].command), streamGeneration);

    expect(command.sequence).toBe('9007199254740993');
    expect(command.authorizationRevision).toBe('9223372036854775807');
    expect(typeof command.sequence).toBe('string');
    expect(typeof command.authorizationRevision).toBe('string');
  });

  it('rejects a valid command belonging to a different transport generation', () => {
    expect(() =>
      decodeVoiceCommand(
        encode(fixtures.valid[0].command),
        '77777777-7777-4777-8777-777777777777'
      )
    ).toThrow(schemaError);
  });

  it.each(['', 'null', '[]', 'true', '42', '"fixture-private-payload"', '{} trailing'])(
    'rejects malformed or non-object wire bytes %j with a fixed public error',
    (wire) => {
      expect(() => decodeVoiceCommand(new TextEncoder().encode(wire), streamGeneration)).toThrow(
        schemaError
      );
    }
  );

  it('rejects invalid UTF-8 without disclosing parser details', () => {
    const wire = JSON.stringify(fixtures.valid[0].command).replace('socket-fixture', 'utf8-marker');
    const [prefix, suffix] = wire.split('utf8-marker');
    const bytes = new Uint8Array([
      ...new TextEncoder().encode(prefix),
      0xc3,
      0x28,
      ...new TextEncoder().encode(suffix),
    ]);
    // A non-fatal decoder would turn the invalid pair into a valid JSON socket
    // string, so this fixture pins UTF-8 validation independently of JSON shape.
    expect(() => decodeVoiceCommand(bytes, streamGeneration)).toThrow(schemaError);
  });

  it('rejects an oversized envelope even if its JSON would otherwise be valid', () => {
    const wire = JSON.stringify(fixtures.valid[0].command).padEnd(4097, ' ');
    expect(() => decodeVoiceCommand(new TextEncoder().encode(wire), streamGeneration)).toThrow(
      schemaError
    );
  });

  it.each([
    ['exact UTF-8 byte limit', 'é'.repeat(64), true],
    ['UTF-8 bytes exceed limit despite fewer UTF-16 units', 'é'.repeat(65), false],
  ])('enforces socket identity %s', (_name, socketId, valid) => {
    const command = structuredClone(fixtures.valid[0].command);
    command.target = {
      ...(command.target as Record<string, unknown>),
      socketId,
    };
    if (valid) {
      expect(decodeVoiceCommand(encode(command), streamGeneration).target?.socketId).toBe(socketId);
    } else {
      expect(() => decodeVoiceCommand(encode(command), streamGeneration)).toThrow(schemaError);
    }
  });

  it.each(['sequence', 'seque\\u006ece'])(
    'rejects duplicate JSON fields including escaped aliases %j',
    (duplicateKey) => {
      const wire = JSON.stringify(fixtures.valid[0].command).replace(
        '"sequence":"9007199254740993"',
        `"sequence":"1","${duplicateKey}":"9007199254740993"`
      );
      expect(() => decodeVoiceCommand(new TextEncoder().encode(wire), streamGeneration)).toThrow(
        schemaError
      );
    }
  );

  it.each([1, 2, 3, 4, 5, 6, 7, 8])('accepts canonical enforcement UUID version %i', (version) => {
    const command = {
      ...fixtures.valid[0].command,
      userId: `aaaaaaaa-aaaa-${version}aaa-8aaa-aaaaaaaaaaaa`,
    };
    expect(decodeVoiceCommand(encode(command), streamGeneration).userId).toBe(command.userId);
  });
});


describe('compatibility envelope classification', () => {
  const legacySnapshot = {
    version: 1,
    channelId: '22222222-2222-4222-8222-222222222222',
    userId: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
    action: 'mute',
    authorizationRevision: '42',
    serverMuted: true,
    serverDeafened: false,
  };

  it.each([
    'schema',
    'commandId',
    'streamGeneration',
    'target',
    'sequence',
    'scope',
    'kind',
    'serverId',
    'reason',
  ])('classifies incomplete durable-only %s as versioned and rejects it', (property) => {
    const payload = { ...legacySnapshot, [property]: 'marker' };

    expect(isVersionedVoiceCommand(payload), `${property} must select strict versioned decoding`).toBe(
      true
    );
    expect(
      () => decodeVoiceCommand(encode(payload), streamGeneration),
      `${property} must fail strict decoding`
    ).toThrow(schemaError);
  });

  it.each([
    ['version:1 server snapshot', legacySnapshot],
    [
      'legacy action payload',
      {
        version: 1,
        channelId: '22222222-2222-4222-8222-222222222222',
        userId: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
        action: 'mute',
      },
    ],
  ])('keeps the %s on the legacy classification path', (_name, payload) => {
    expect(isVersionedVoiceCommand(payload)).toBe(false);
  });
});
