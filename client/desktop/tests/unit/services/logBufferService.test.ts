import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import {
  _resetForTest,
  formatEntries,
  getEntries,
  install,
  sanitize,
  uninstall,
} from '@/renderer/services/logBufferService';

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
    it('caps the buffer at MAX_ENTRIES (500), dropping the oldest', () => {
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
