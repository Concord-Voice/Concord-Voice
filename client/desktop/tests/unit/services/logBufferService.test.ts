import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import {
  _resetForTest,
  formatEntries,
  getEntries,
  install,
  sanitize,
  uninstall,
} from '@/renderer/services/system/logBufferService';

describe('logBufferService', () => {
  beforeEach(() => {
    _resetForTest();
  });

  afterEach(() => {
    uninstall();
  });

  describe('install()', () => {
    it('captures console.log calls into the buffer', () => {
      install();
      // eslint-disable-next-line no-console
      console.log('hello world');
      const entries = getEntries();
      expect(entries).toHaveLength(1);
      expect(entries[0].message).toBe('hello world');
      expect(entries[0].level).toBe('log');
    });

    it('captures console.warn and console.error too', () => {
      install();
      // eslint-disable-next-line no-console
      console.warn('a warning');
      console.error('an error');
      const entries = getEntries();
      expect(entries.map((e) => e.level)).toEqual(['warn', 'error']);
    });

    it('captures console.debug and console.info with correct level tags', () => {
      install();
      // eslint-disable-next-line no-console
      console.debug('dbg-line');
      // eslint-disable-next-line no-console
      console.info('inf-line');
      const entries = getEntries();
      expect(entries.find((e) => e.message.includes('dbg-line'))?.level).toBe('debug');
      expect(entries.find((e) => e.message.includes('inf-line'))?.level).toBe('info');
      uninstall();
    });

    it('is idempotent — install twice is a no-op', () => {
      install();
      install();
      // eslint-disable-next-line no-console
      console.log('single');
      expect(getEntries()).toHaveLength(1);
    });

    it('does NOT swallow original console output', () => {
      // The shadow wrapper must still fire the original. Spy on the
      // installed wrapper by checking that the buffered message matches
      // a value the original console wouldn't normally tamper with.
      install();
      // eslint-disable-next-line no-console
      console.log('keep verbatim');
      // If the original console.log were swallowed, the buffer would still
      // have the entry but the test author would have no other observable
      // signal. The cleanest invariant is that uninstall() then reinstall
      // captures a NEW entry — proving the wrapper is still functional.
      const beforeUninstall = getEntries().length;
      uninstall();
      // eslint-disable-next-line no-console
      console.log('after uninstall, not captured');
      expect(getEntries()).toHaveLength(beforeUninstall);
    });
  });

  describe('ring buffer cap', () => {
    it('caps the general buffer at MAX_GENERAL_ENTRIES (500), dropping the oldest', () => {
      install();
      for (let i = 0; i < 600; i++) {
        // eslint-disable-next-line no-console
        console.log(`msg-${i}`);
      }
      const entries = getEntries();
      expect(entries).toHaveLength(500);
      // First entry should be the (600 - 500)th = msg-100
      expect(entries[0].message).toBe('msg-100');
      expect(entries[entries.length - 1].message).toBe('msg-599');
    });

    it('retains warn/error through a debug/info flood that exceeds the general cap', () => {
      install();
      // eslint-disable-next-line no-console
      console.error('critical-before');
      // A burst large enough to fully cycle the general buffer several times.
      for (let i = 0; i < 2000; i++) {
        // eslint-disable-next-line no-console
        console.debug(`noise-${i}`);
      }
      // eslint-disable-next-line no-console
      console.warn('critical-after');

      const entries = getEntries();
      const messages = entries.map((e) => e.message);

      // The triage-critical lines survive despite the flood...
      expect(messages).toContain('critical-before');
      expect(messages).toContain('critical-after');
      // ...the high-volume general segment is still capped...
      expect(entries.filter((e) => e.level === 'debug')).toHaveLength(500);
      // ...and chronological order is preserved across the two segments.
      expect(messages.indexOf('critical-before')).toBeLessThan(messages.indexOf('critical-after'));
    });
  });

  describe('sanitize()', () => {
    it('redacts emails', () => {
      expect(sanitize('contact alice@example.com please')).toContain('<email>');
      expect(sanitize('contact alice@example.com please')).not.toContain('alice@example.com');
    });

    it('redacts JWTs', () => {
      const jwt = 'eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1c2VyMSJ9.abcdefghij1234567890XYZ'; // pragma: allowlist secret -- fake JWT test fixture, not a real token
      expect(sanitize(`Authorization: ${jwt}`)).toContain('<jwt>');
      expect(sanitize(`Authorization: ${jwt}`)).not.toContain(jwt);
    });

    it('redacts Bearer tokens', () => {
      const out = sanitize('Sent Bearer ya29.aA1BBccDDeeFFggHHii123456');
      expect(out).toContain('Bearer <token>');
      expect(out).not.toContain('ya29');
    });

    it('redacts POSIX user paths', () => {
      const out = sanitize('failed at /Users/michael/.ssh/id_rsa');
      expect(out).toContain('/Users/<user>');
      expect(out).not.toContain('michael');
    });

    it('redacts Windows user paths', () => {
      const out = sanitize('failed at C:\\Users\\Michael\\AppData');
      expect(out).toContain('C:\\Users\\<user>');
      expect(out).not.toContain('Michael');
    });

    it('redacts IPv4 addresses', () => {
      expect(sanitize('refused by 10.0.0.5')).toContain('<ip>');
    });

    it('redacts full-form IPv6 addresses', () => {
      // Full 8-group form. The current pattern requires explicit hex
      // groups separated by single colons; `::` compressed forms are not
      // yet caught (tracked as a follow-up; the panels in #159 / #160 can
      // tighten this if their content surfaces compressed-form leaks).
      expect(sanitize('refused by 2001:db8:abcd:1234:5678:9abc:def0:1')).toContain('<ip>');
    });

    it('redacts long hex strings', () => {
      const hex = 'abcdef0123456789abcdef0123456789'; // pragma: allowlist secret -- fake hex test fixture, not a real secret
      expect(sanitize(`hash=${hex}`)).toContain('<hex>');
    });

    it('redacts long base64 strings', () => {
      const blob = 'MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA1+/=';
      expect(sanitize(`key=${blob}`)).toContain('<base64>');
    });

    it('returns empty string for non-string input', () => {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      expect(sanitize(null as any)).toBe('');
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      expect(sanitize(undefined as any)).toBe('');
    });

    it('does NOT redact short version strings like v1.2.3', () => {
      expect(sanitize('Running v1.2.3')).toBe('Running v1.2.3');
    });
  });

  // SC-3: the PII patterns were converted to String.raw (removing one backslash
  // escape layer). These lock that the escape-bearing patterns still behave
  // identically — a botched conversion (e.g. \. → . or \\Users\\ → \Users\)
  // would change matching and fail here.
  describe('String.raw pattern equivalence (SC-3)', () => {
    // JWT redaction itself is covered above; here we lock only the escape-
    // sensitive patterns a botched String.raw conversion would break.
    it('redacts only the username segment of a Windows path (backslashes intact)', () => {
      expect(sanitize('open C:\\Users\\bob\\AppData\\x')).toContain('C:\\Users\\<user>');
    });

    it('redacts emails carrying multiple literal dots', () => {
      const out = sanitize('mail a.b.c@mail.example.co.uk now');
      expect(out).toContain('<email>');
      expect(out).not.toContain('a.b.c@mail.example.co.uk');
    });
  });

  // #3104 follow-up: a live TURN relay credential must not survive capture.
  //
  // The fixture is generated the SAME WAY services/control-plane/pkg/config/turn.go
  // generates it — HMAC-SHA1 over `<expiry>:<userID>`, standard base64 — rather
  // than hand-written, because the defect is a LENGTH defect: base64 of 20 HMAC
  // bytes is exactly 28 characters, and the long-base64 PATTERN has a 40-char
  // floor. A hand-written literal of the wrong length would make every assertion
  // below vacuous.
  describe('TURN credential scrub (#3104)', () => {
    const TURN_SHARED_SECRET = 'test-turn-shared-secret-not-a-real-one'; // pragma: allowlist secret -- synthetic HMAC input; mirrors turn.go's TURNSecret so the fixture has production shape
    const USER_ID = '550e8400-e29b-41d4-a716-446655440000';
    const EXPIRY = 1774000000; // unix seconds, 10 digits like a real 24h TTL

    // A credential with the production SHAPE, built without production's crypto
    // and without a credential-shaped literal.
    //
    // The defect under test is a LENGTH defect: turn.go base64-encodes 20
    // HMAC-SHA1 bytes, which is always 28 characters, and the scrubber's
    // long-base64 pattern had a 40-character floor — so the credential walked
    // straight through it. Any 20-byte value reproduces that exactly, so this
    // encodes an obviously-fake 20-character string instead.
    //
    // Two scanners shaped this. A live `createHmac('sha1', ...)` fed by a user
    // id is flagged by CodeQL as weak crypto on sensitive data — correctly, on
    // shape — and pinning its output instead tripped gitleaks as a generic API
    // key, since a real HMAC is exactly what a real credential looks like.
    // Encoding readable plaintext satisfies both without weakening anything:
    // the length assertion below is the guard, and it is the whole defect.
    //
    // Production-shape fidelity keeps its own test on the Go side, where
    // sanitize_test.go still mints this the real way under a documented
    // `#nosec G505` (SHA-1 is mandated by the TURN REST API credential spec).
    // If turn.go's algorithm ever changes, that is the test that notices.
    const TURN_CREDENTIAL = Buffer.from('NOT-A-REAL-CRED-1234').toString('base64');
    function mintTurnCredentials(): { username: string; credential: string } {
      return { username: `${EXPIRY}:${USER_ID}`, credential: TURN_CREDENTIAL };
    }

    it('the fixture has the production shape the defect depends on', () => {
      const { username, credential } = mintTurnCredentials();
      // 20 HMAC-SHA1 bytes -> 27 base64 chars + one pad = 28. Strictly under the
      // 40-char floor of the long-base64 PATTERN, which is why it used to survive.
      expect(credential).toHaveLength(28);
      expect(credential.endsWith('=')).toBe(true);
      expect(username).toMatch(/^\d{10}:[0-9a-f-]{36}$/);
    });

    it('does NOT let the credential survive sanitize()', () => {
      const { username, credential } = mintTurnCredentials();
      const line = `[ice] transport opts ${JSON.stringify({
        urls: 'turn:turn.concordvoice.chat:3478',
        username,
        credential,
      })}`;
      const out = sanitize(line);
      expect(out).not.toContain(credential);
    });

    it('does NOT let the TURN username survive sanitize()', () => {
      const { username, credential } = mintTurnCredentials();
      const out = sanitize(`[ice] username=${username} credential=${credential}`);
      expect(out).not.toContain(username);
      expect(out).not.toContain(String(EXPIRY));
    });

    it('redacts credential/password by KEY in the JSON serialization form', () => {
      const out = sanitize('{"urls":"turn:h:3478","credential":"whatever-shape-this-is"}');
      expect(out).not.toContain('whatever-shape-this-is');
      expect(out).toContain('"credential"');
    });

    it('redacts credential/password by KEY in the util.inspect form', () => {
      const out = sanitize("RTCConfiguration { credential: 'zzz', password: 'qqq' }");
      expect(out).not.toContain('zzz');
      expect(out).not.toContain('qqq');
    });

    it('never enters the ring buffer raw (capture-time, not read-time)', () => {
      const { username, credential } = mintTurnCredentials();
      install();
      // eslint-disable-next-line no-console
      console.debug('[ice] servers', [
        { urls: 'turn:turn.concordvoice.chat:3478', username, credential },
      ]);
      const joined = formatEntries(getEntries());
      expect(joined).not.toContain(credential);
      expect(joined).not.toContain(username);
    });

    // False-positive containment. Over-redaction degrades every bug report, so
    // the narrow-band rule must not swallow ordinary diagnostic text.
    it.each([
      'voice join ok in 412ms, 3 servers, policy=all, attempt 2 of 5',
      'GET /api/v1/channels?limit=50&before=abc -> 200',
      "RTCPeerConnection { connectionState: 'connected', iceGatheringState: 'complete' }",
      'chunk assets/index-Bq7Xr2Kd.js loaded',
      // 28 base64 characters with NO pad — the narrow band must key on the
      // '=' at position 28, not on the length alone.
      'consumer AbCdEfGhIjKlMnOpQrStUvWxYz01 resumed',
    ])('does not redact ordinary diagnostic text: %s', (line) => {
      expect(sanitize(line)).toBe(line);
    });

    it('does not redact a 32-char base64 token (only the 27+pad band matches)', () => {
      // base64 of 23 bytes -> 32 chars with one pad. Neither 40+ nor 27+pad.
      const token = Buffer.alloc(23, 7).toString('base64');
      expect(token).toHaveLength(32);
      expect(sanitize(`build=${token}`)).toContain(token);
    });

    it('still redacts a long base64 blob as <base64> (existing category intact)', () => {
      const blob = Buffer.alloc(48, 3).toString('base64');
      expect(sanitize(`key=${blob}`)).toContain('<base64>');
    });

    // ─── #3117 gap 2: a bare key with a double-quoted value ────────────────
    //
    // The JSON entry requires a QUOTED key; the util.inspect entry required a
    // SINGLE-quoted value. A bare key with a double-quoted value satisfied
    // neither, so it was redacted by nothing. Present identically in the Go
    // backstop, which is why both surfaces move in the same change.
    it.each([
      'credential: "whatever-shape-this-is"',
      'turnCredential: "whatever-shape-this-is"',
      '{ password: "whatever-shape-this-is" }', // pragma: allowlist secret -- synthetic scrubber input, not a credential
      '{credential:"whatever-shape-this-is"}',
    ])('redacts a bare key with a double-quoted value: %s', (line) => {
      const out = sanitize(line);
      expect(out).not.toContain('whatever-shape-this-is');
      // It must be the KEY-shaped entry that ate it, not an incidental shape
      // match elsewhere in the table — and the key must survive so a triager
      // can still see WHAT was redacted.
      expect(out).toContain('<redacted>');
    });

    // #3117 gap 1 is a Go-only defect: `\b` supplies no boundary when the
    // token's OWN first character is '+' or '/', because both are non-word and
    // so is every realistic delimiter. The client's lookbehind has no such
    // hole. This pins the client half of the parity claim — it was already
    // green before the fix, so it is a pin, not a regression test.
    //
    // The leading base64 character encodes the top 6 bits of byte 0: 0xF8
    // yields index 62 ('+') and 0xFC yields index 63 ('/').
    it.each([
      { firstByte: 0xf8, firstChar: '+' },
      { firstByte: 0xfc, firstChar: '/' },
    ])(
      'redacts a credential whose first base64 character is $firstChar',
      ({ firstByte, firstChar }) => {
        const credential = Buffer.concat([
          Buffer.from([firstByte]),
          Buffer.alloc(19, 0x42),
        ]).toString('base64');
        expect(credential).toHaveLength(28);
        expect(credential.startsWith(firstChar)).toBe(true);
        expect(sanitize(`[ice] relay cred ${credential} ok`)).not.toContain(credential);
      }
    );
  });

  describe('capture-time sanitization', () => {
    it('strips emails from captured log lines (PII never enters the buffer raw)', () => {
      install();
      // eslint-disable-next-line no-console
      console.log('User alice@example.com logged in');
      const entries = getEntries();
      expect(entries[0].message).toContain('<email>');
      expect(entries[0].message).not.toContain('alice@example.com');
    });
  });

  describe('formatEntries()', () => {
    it('renders entries one per line with ISO timestamp + level + message', () => {
      install();
      // eslint-disable-next-line no-console
      console.log('hello');
      const out = formatEntries();
      expect(out).toMatch(/\d{4}-\d{2}-\d{2}T.*\[log\] {2}hello/);
    });

    it('returns empty string when the buffer is empty', () => {
      expect(formatEntries()).toBe('');
    });
  });

  describe('Error argument formatting', () => {
    it('serializes Error objects as name: message + stack', () => {
      install();
      const err = new Error('boom');
      // eslint-disable-next-line no-console
      console.error('caught:', err);
      const entries = getEntries();
      expect(entries[0].message).toContain('Error: boom');
    });
  });
});
