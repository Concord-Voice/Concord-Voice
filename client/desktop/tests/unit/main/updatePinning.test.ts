// @vitest-environment node
import { describe, it, expect, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import {
  computeSpkiSha256,
  createPinningVerifyProc,
  isHostnamePinned,
  type PinConfig,
  type VerifyProcRequest,
} from '../../../src/main/updatePinning';
import { PIN_CONFIG } from '../../../src/main/updatePinningConfig';

const PREVIOUS_PRODUCTION_SPKI = '0a4ccc0dfc2c60c67e4b814292467bbf7e525d6b75d38e32ea646153fc7c49f2'; // pragma: allowlist secret
const CURRENT_PRODUCTION_SPKI = 'adc59f988ed774248efa8e2a5cf3a5114ddf05252abc72ae93ee445ba5d8ce75'; // pragma: allowlist secret
const PREVIOUS_PIN_RECOVERY_WINDOW_END_MS = Date.parse('2026-07-29T00:00:00.000Z');

const FIXTURES_DIR = join(__dirname, '../../fixtures/pinning');
const fingerprints = JSON.parse(
  readFileSync(join(FIXTURES_DIR, 'spki-fingerprints.json'), 'utf-8')
) as Record<'primary' | 'fallback' | 'rogue', string>;
const primaryPem = readFileSync(join(FIXTURES_DIR, 'primary.pem'), 'utf-8');
const fallbackPem = readFileSync(join(FIXTURES_DIR, 'fallback.pem'), 'utf-8');
const roguePem = readFileSync(join(FIXTURES_DIR, 'rogue.pem'), 'utf-8');
const rotated20260629Pem = readFileSync(join(FIXTURES_DIR, 'rotated-2026-06-29.pem'), 'utf-8');

describe('computeSpkiSha256', () => {
  it('matches the committed fingerprint for the primary fixture', () => {
    expect(computeSpkiSha256(primaryPem)).toBe(fingerprints.primary);
  });

  it('matches the committed fingerprint for the fallback fixture', () => {
    expect(computeSpkiSha256(fallbackPem)).toBe(fingerprints.fallback);
  });

  it('matches the committed fingerprint for the rogue fixture', () => {
    expect(computeSpkiSha256(roguePem)).toBe(fingerprints.rogue);
  });

  it('returns 64 lowercase hex chars', () => {
    expect(computeSpkiSha256(primaryPem)).toMatch(/^[0-9a-f]{64}$/);
  });

  it('is deterministic across invocations', () => {
    expect(computeSpkiSha256(primaryPem)).toBe(computeSpkiSha256(primaryPem));
  });

  it('different keypairs yield different SPKI fingerprints', () => {
    expect(computeSpkiSha256(primaryPem)).not.toBe(computeSpkiSha256(roguePem));
  });
});

describe('PIN_CONFIG production pins', () => {
  it('trusts the 2026-06-29 Cloudflare edge SPKI rotation', () => {
    expect(PIN_CONFIG.primaryPins).toContain(computeSpkiSha256(rotated20260629Pem));
  });

  it('trusts the current production Cloudflare edge SPKI', () => {
    expect(PIN_CONFIG.primaryPins).toContain(CURRENT_PRODUCTION_SPKI);
  });

  it('bounds the previous production pin to the recovery window', () => {
    if (PIN_CONFIG.primaryPins.includes(PREVIOUS_PRODUCTION_SPKI)) {
      expect(Date.now()).toBeLessThan(PREVIOUS_PIN_RECOVERY_WINDOW_END_MS);
    }
  });
});

describe('isHostnamePinned (I5, I7)', () => {
  const pinned = ['api.concordvoice.chat'] as const;

  it('matches an exact lowercase hostname', () => {
    expect(isHostnamePinned('api.concordvoice.chat', pinned)).toBe(true);
  });

  it('is case-insensitive', () => {
    expect(isHostnamePinned('API.CONCORDVOICE.CHAT', pinned)).toBe(true);
    expect(isHostnamePinned('Api.Concordvoice.Chat', pinned)).toBe(true);
  });

  it('does NOT match subdomains (no wildcard)', () => {
    expect(isHostnamePinned('staging.concordvoice.chat', pinned)).toBe(false);
    expect(isHostnamePinned('foo.api.concordvoice.chat', pinned)).toBe(false);
  });

  it('does NOT match other hostnames', () => {
    expect(isHostnamePinned('localhost', pinned)).toBe(false);
    expect(isHostnamePinned('attacker.com', pinned)).toBe(false);
    expect(isHostnamePinned('', pinned)).toBe(false);
  });

  it('returns false when the pinned list is empty', () => {
    expect(isHostnamePinned('api.concordvoice.chat', [])).toBe(false);
  });
});

// ── createPinningVerifyProc ──────────────────────────────────────────────────

const mkLogger = () => ({ warn: vi.fn(), error: vi.fn() });

const validConfig: PinConfig = {
  pinnedHosts: ['api.concordvoice.chat'],
  primaryPins: [fingerprints.primary],
  fallbackPins: [fingerprints.fallback],
};

const mkRequest = (overrides: Partial<VerifyProcRequest> = {}): VerifyProcRequest => ({
  hostname: 'api.concordvoice.chat',
  certificate: { data: primaryPem },
  errorCode: 0,
  verificationResult: 'net::OK',
  ...overrides,
});

describe('createPinningVerifyProc — init invariants (I3)', () => {
  it('throws when primaryPins is empty', () => {
    expect(() => createPinningVerifyProc({ ...validConfig, primaryPins: [] }, mkLogger())).toThrow(
      /primary.+pin/i
    );
  });

  it('throws when fallbackPins is empty', () => {
    expect(() => createPinningVerifyProc({ ...validConfig, fallbackPins: [] }, mkLogger())).toThrow(
      /fallback.+pin/i
    );
  });

  it('returns a callable proc for a valid config', () => {
    expect(typeof createPinningVerifyProc(validConfig, mkLogger())).toBe('function');
  });
});

describe('createPinningVerifyProc — hostname gate + Chromium defer (I2, I5, I7)', () => {
  it('passes through non-pinned hosts with callback(-3)', () => {
    const proc = createPinningVerifyProc(validConfig, mkLogger());
    const cb = vi.fn();
    proc(mkRequest({ hostname: 'localhost' }), cb);
    expect(cb).toHaveBeenCalledWith(-3);
    expect(cb).toHaveBeenCalledTimes(1);
  });

  it('passes through subdomains (no wildcard) with callback(-3)', () => {
    const proc = createPinningVerifyProc(validConfig, mkLogger());
    const cb = vi.fn();
    proc(mkRequest({ hostname: 'staging.concordvoice.chat' }), cb);
    expect(cb).toHaveBeenCalledWith(-3);
  });

  it('defers to Chromium (callback(-3)) when errorCode != 0', () => {
    const proc = createPinningVerifyProc(validConfig, mkLogger());
    const cb = vi.fn();
    proc(mkRequest({ errorCode: -6 /* CERT_AUTHORITY_INVALID */ }), cb);
    expect(cb).toHaveBeenCalledWith(-3);
  });
});

describe('createPinningVerifyProc — pin matching (I4, I6, I8)', () => {
  it('accepts with callback(0) when primary pin matches', () => {
    const logger = mkLogger();
    const proc = createPinningVerifyProc(validConfig, logger);
    const cb = vi.fn();
    proc(mkRequest(), cb);
    expect(cb).toHaveBeenCalledWith(0);
    expect(logger.warn).not.toHaveBeenCalled();
  });

  it('accepts with callback(0) when fallback pin matches, and logs warn (I8)', () => {
    const logger = mkLogger();
    const config: PinConfig = {
      ...validConfig,
      primaryPins: [fingerprints.rogue],
      fallbackPins: [fingerprints.primary],
    };
    const proc = createPinningVerifyProc(config, logger);
    const cb = vi.fn();
    proc(mkRequest(), cb);
    expect(cb).toHaveBeenCalledWith(0);
    expect(logger.warn).toHaveBeenCalledWith(expect.stringMatching(/fallback.+pin.+matched/i));
  });

  it('pin comparison is case-insensitive hex (I4)', () => {
    const logger = mkLogger();
    const config: PinConfig = {
      ...validConfig,
      primaryPins: [fingerprints.primary.toUpperCase()],
    };
    const proc = createPinningVerifyProc(config, logger);
    const cb = vi.fn();
    proc(mkRequest(), cb);
    expect(cb).toHaveBeenCalledWith(0);
  });
});

describe('createPinningVerifyProc — fail-closed + hygiene (I1, I9, I10)', () => {
  it('rejects with callback(-2) on pin miss', () => {
    const logger = mkLogger();
    const proc = createPinningVerifyProc(validConfig, logger);
    const cb = vi.fn();
    proc(mkRequest({ certificate: { data: roguePem } }), cb);
    expect(cb).toHaveBeenCalledWith(-2);
    expect(logger.error).toHaveBeenCalledWith(expect.stringMatching(/SECURITY.*pin miss/));
  });

  it('rejects with callback(-2) when SPKI extraction throws (I1 fail-closed)', () => {
    const logger = mkLogger();
    const proc = createPinningVerifyProc(validConfig, logger);
    const cb = vi.fn();
    proc(mkRequest({ certificate: { data: 'NOT A PEM' } }), cb);
    expect(cb).toHaveBeenCalledWith(-2);
    expect(logger.error).toHaveBeenCalledWith(expect.stringMatching(/SECURITY.*verifyProc error/));
  });

  it('logs hostname + SPKI-last-8 but NOT full SPKI or subject fields (I9)', () => {
    const logger = mkLogger();
    const proc = createPinningVerifyProc(validConfig, logger);
    proc(mkRequest({ certificate: { data: roguePem } }), vi.fn());

    const lines = logger.error.mock.calls.map((c) => String(c[0])).join(' ');
    expect(lines).toContain('api.concordvoice.chat');
    expect(lines).toMatch(/SPKI:…[0-9a-f]{8}/);
    expect(lines).not.toMatch(/[0-9a-f]{64}/);
    expect(lines).not.toMatch(/CN=|O=|OU=|C=/);
  });

  it('invokes the callback exactly once on every code path (I10)', () => {
    const proc = createPinningVerifyProc(validConfig, mkLogger());

    for (const scenario of [
      mkRequest(),
      mkRequest({ hostname: 'localhost' }),
      mkRequest({ errorCode: -6 }),
      mkRequest({ certificate: { data: roguePem } }),
      mkRequest({ certificate: { data: 'bogus' } }),
    ]) {
      const cb = vi.fn();
      proc(scenario, cb);
      expect(cb).toHaveBeenCalledTimes(1);
    }
  });
});
