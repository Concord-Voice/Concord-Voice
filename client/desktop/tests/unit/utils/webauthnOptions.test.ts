import {
  methodsForOptions,
  parseWebAuthnOptions,
  rpIdBelongsTo,
  webauthnOptionsOrNull,
} from '@/renderer/utils/webauthnOptions';

const BASE = 'http://localhost:8080';
const bytes = (buf: BufferSource | undefined) => Array.from(new Uint8Array(buf as ArrayBuffer));

describe('parseWebAuthnOptions', () => {
  it('decodes the flat server shape', () => {
    const opts = parseWebAuthnOptions(
      {
        challenge: 'AQID',
        rpId: 'localhost',
        timeout: 60000,
        allowCredentials: [{ type: 'public-key', id: 'BAU', transports: ['usb', 'internal'] }],
        userVerification: 'preferred',
      },
      BASE
    );

    expect(bytes(opts.challenge)).toEqual([1, 2, 3]);
    expect(opts.rpId).toBe('localhost');
    expect(opts.timeout).toBe(60000);
    expect(opts.allowCredentials).toHaveLength(1);
    expect(bytes(opts.allowCredentials?.[0].id)).toEqual([4, 5]);
    expect(opts.allowCredentials?.[0].transports).toEqual(['usb', 'internal']);
    expect(opts.userVerification).toBe('preferred');
  });

  it('unwraps the publicKey envelope', () => {
    const opts = parseWebAuthnOptions(
      { publicKey: { challenge: 'AQID', rpId: 'localhost' } },
      BASE
    );
    expect(bytes(opts.challenge)).toEqual([1, 2, 3]);
    expect(opts.rpId).toBe('localhost');
  });

  it('throws on options without a challenge', () => {
    expect(() => parseWebAuthnOptions({ rpId: 'localhost' }, BASE)).toThrow(
      'Server returned invalid WebAuthn options.'
    );
  });

  // The relying party is the one the browser uses, so a missing rpId would
  // mean the renderer's origin, which every server shares.
  it('throws on options without an rpId', () => {
    expect(() => parseWebAuthnOptions({ challenge: 'AQID' }, BASE)).toThrow(
      'Server returned invalid WebAuthn options.'
    );
  });

  // #3663 review, H1. Mutant: no binding check, so a server names another
  // server's relying party and the assertion it gets back is relayed there.
  it("throws on another server's relying party", () => {
    expect(() =>
      parseWebAuthnOptions(
        { challenge: 'AQID', rpId: 'concordvoice.chat' },
        'https://evil-selfhost.example'
      )
    ).toThrow('Server returned WebAuthn options for another server.');
  });
});

describe('rpIdBelongsTo', () => {
  it.each([
    ['the API host', 'localhost', BASE],
    ['the API host, by full name', 'api.concordvoice.chat', 'https://api.concordvoice.chat'],
    ["the official service's relying party", 'concordvoice.chat', 'https://api.concordvoice.chat'],
    ['any letter case', 'ConcordVoice.Chat', 'https://API.concordvoice.chat'],
    ['an IP host, exactly', '127.0.0.1', 'http://127.0.0.1:8080'],
  ])('accepts %s', (_name, rpId, apiBase) => {
    expect(rpIdBelongsTo(rpId, apiBase)).toBe(true);
  });

  // #3663 review, round 3: a parent domain is accepted only through the
  // official mapping. Mutants: a suffix rule (the delegated-subdomain rows),
  // the mapping keyed on anything but the exact host, and no URL guard.
  it.each([
    ["another server's domain", 'concordvoice.chat', 'https://evil-selfhost.example'],
    ["a delegated subdomain claiming its parent's", 'example.com', 'https://evil.example.com'],
    [
      "another official subdomain claiming the parent's",
      'concordvoice.chat',
      'https://evil.concordvoice.chat',
    ],
    [
      'a host that only starts like the official one',
      'concordvoice.chat',
      'https://api.concordvoice.chat.evil.example',
    ],
    ['a child of the API host', 'a.api.concordvoice.chat', 'https://api.concordvoice.chat'],
    ['an API base that is not a URL', 'localhost', 'not a url'],
  ])('rejects %s', (_name, rpId, apiBase) => {
    expect(rpIdBelongsTo(rpId, apiBase)).toBe(false);
  });
});

describe('webauthnOptionsOrNull', () => {
  it('returns null when the server sent none', () => {
    expect(webauthnOptionsOrNull(undefined, BASE)).toBeNull();
    expect(webauthnOptionsOrNull(null, BASE)).toBeNull();
  });

  it('returns null instead of throwing on malformed options', () => {
    expect(webauthnOptionsOrNull({ rpId: 'localhost' }, BASE)).toBeNull();
    expect(webauthnOptionsOrNull('not-an-object', BASE)).toBeNull();
  });

  it('returns the parsed options when they are valid', () => {
    const opts = webauthnOptionsOrNull({ challenge: 'AQID', rpId: 'localhost' }, BASE);
    expect(opts).not.toBeNull();
    expect(bytes(opts?.challenge)).toEqual([1, 2, 3]);
  });

  // #3663 review. These pass the schema and only fail to decode, so they reach
  // the catch. Mutant: no try/catch, so the throw aborts the refresh or SSO flow.
  it.each([
    ['an undecodable challenge', { challenge: '***', rpId: 'localhost' }],
    [
      'an undecodable credential id',
      {
        challenge: 'AQID',
        rpId: 'localhost',
        allowCredentials: [{ type: 'public-key', id: '***' }],
      },
    ],
  ])('returns null for %s', (_name, options) => {
    expect(webauthnOptionsOrNull(options, BASE)).toBeNull();
  });

  it('warns with a fixed string, never the payload', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    webauthnOptionsOrNull({ challenge: 'secret-challenge-****', rpId: 'localhost' }, BASE);
    expect(warn).toHaveBeenCalledWith('[MFA] server WebAuthn options failed validation');
    expect(JSON.stringify(warn.mock.calls)).not.toContain('secret-challenge');
    warn.mockRestore();
  });

  it.each([
    ['a non-object', 7],
    ['an array', []],
    ['a negative timeout', { challenge: 'AQID', rpId: 'localhost', timeout: -1 }],
    [
      'a credential that is not a public key',
      { challenge: 'AQID', rpId: 'localhost', allowCredentials: [{ type: 'password', id: 'BAU' }] },
    ],
    ["another server's relying party", { challenge: 'AQID', rpId: 'concordvoice.chat' }],
    [
      'credentials that are not a list',
      { challenge: 'AQID', rpId: 'localhost', allowCredentials: 'BAU' },
    ],
    // 1028 characters decode cleanly, so only the cap rejects it.
    ['an oversized challenge', { challenge: 'A'.repeat(1028), rpId: 'localhost' }],
    [
      'too many credentials',
      {
        challenge: 'AQID',
        rpId: 'localhost',
        allowCredentials: Array(65).fill({ type: 'public-key', id: 'BAU' }),
      },
    ],
  ])('rejects %s', (_name, options) => {
    expect(webauthnOptionsOrNull(options, BASE)).toBeNull();
  });

  // Two layers keep a server from adding anything else to the ceremony: the
  // schemas strip unknown keys, and the result is built field by field. Mutant:
  // remove both (a passthrough schema whose output is spread into the result).
  it('passes only the known fields to the ceremony', () => {
    const hostile = JSON.parse(
      '{"challenge":"AQID","rpId":"localhost","extensions":{"appid":"https://x.example"},"hints":["hybrid"],"__proto__":{"polluted":true},"allowCredentials":[{"type":"public-key","id":"BAU","extra":1}]}'
    );
    const opts = webauthnOptionsOrNull(hostile, BASE);
    expect(opts).not.toBeNull();
    expect(Object.keys(opts as object).sort()).toEqual(
      ['allowCredentials', 'challenge', 'rpId', 'timeout'].sort()
    );
    expect(Object.keys(opts?.allowCredentials?.[0] ?? {}).sort()).toEqual(
      ['id', 'transports', 'type'].sort()
    );
    expect(({} as { polluted?: boolean }).polluted).toBeUndefined();
  });
});

describe('methodsForOptions', () => {
  const opts = { challenge: new Uint8Array([1]) } as PublicKeyCredentialRequestOptions;

  it('keeps every method when the options are usable', () => {
    expect(methodsForOptions(['webauthn', 'totp'], opts)).toEqual(['webauthn', 'totp']);
  });

  // Mutant: the list passed through, so the modal opens on a security-key pane
  // with nothing behind it.
  it('drops webauthn without options while another method is left', () => {
    expect(methodsForOptions(['webauthn', 'email'], null)).toEqual(['email']);
  });

  it('keeps webauthn when it is the only method', () => {
    expect(methodsForOptions(['webauthn'], null)).toEqual(['webauthn']);
  });

  // #3663 review: an older shell passes the server's lists over IPC
  // unchecked. Mutant: the list used as given, so `.filter` throws on a string.
  it.each([
    ['a string', 'totp', []],
    ['missing', undefined, []],
    ['holding a non-string', ['email', 7], ['email']],
  ])('keeps only strings when the list is %s', (_name, methods, expected) => {
    expect(methodsForOptions(methods, null)).toEqual(expected);
    expect(methodsForOptions(methods, opts)).toEqual(expected);
  });
});
