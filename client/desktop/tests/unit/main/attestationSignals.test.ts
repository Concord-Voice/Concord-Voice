// @vitest-environment node
/**
 * Unit tests for the attestation signal assembly module (#677).
 *
 * Covers all four platform branches plus one value-shape edge case:
 *  - macos: cert_hash + machine_id + all spa fields
 *  - windows: cert_hash + machine_id (covers the || 'windows' branch)
 *  - linux: no cert_hash, machine_id present
 *  - web: no cert_hash, no machine_id; spa fields still present
 *  - macos with empty cert hash: cert_hash omitted (not sent as "")
 *
 * getBuildCertHash is a hoisted vi.fn() so individual tests can override
 * its return value via mockReturnValueOnce.
 */

import { describe, it, expect, vi } from 'vitest';

const { mockGetBuildCertHash } = vi.hoisted(() => ({
  mockGetBuildCertHash: vi.fn((): string => 'sha256:bin'), // pragma: allowlist secret
}));

vi.mock('@/main/machineId', () => ({ getMachineId: () => 'm-abc' }));
vi.mock('@/main/buildInfo', () => ({ getBuildCertHash: mockGetBuildCertHash }));
vi.mock('@/main/spaState', () => ({
  getSpaHash: () => 'sha256:spa', // pragma: allowlist secret
  getSpaVersion: () => '20260529',
}));

import { collectAttestationSignals } from '@/main/attestationSignals';

describe('collectAttestationSignals', () => {
  it('macOS — all optional fields populated + required spa fields present', async () => {
    const signals = await collectAttestationSignals({ platform: 'macos', version: '0.2.7' });
    expect(signals.cert_hash).toBe('sha256:bin'); // pragma: allowlist secret
    expect(signals.machine_id).toBe('m-abc');
    expect(signals.spa_hash).toBe('sha256:spa'); // pragma: allowlist secret
    expect(signals.spa_version).toBe('20260529');
    expect(signals.version).toBe('0.2.7');
    expect(signals.platform).toBe('macos');
  });

  it('windows — cert_hash present (|| windows branch) and machine_id present', async () => {
    const signals = await collectAttestationSignals({ platform: 'windows', version: '0.2.7' });
    expect(signals.cert_hash).toBe('sha256:bin'); // pragma: allowlist secret
    expect(signals.machine_id).toBe('m-abc');
    expect(signals.spa_hash).toBe('sha256:spa'); // pragma: allowlist secret
  });

  it('linux — cert_hash is undefined, machine_id is present', async () => {
    const signals = await collectAttestationSignals({ platform: 'linux', version: '0.2.7' });
    expect(signals.cert_hash).toBeUndefined();
    expect(signals.machine_id).toBe('m-abc');
    expect(signals.spa_hash).toBe('sha256:spa'); // pragma: allowlist secret
  });

  it('web — cert_hash and machine_id are both undefined; spa fields still present', async () => {
    const signals = await collectAttestationSignals({ platform: 'web', version: '0.2.7' });
    expect(signals.cert_hash).toBeUndefined();
    expect(signals.machine_id).toBeUndefined();
    expect(signals.spa_hash).toBe('sha256:spa'); // pragma: allowlist secret
    expect(signals.spa_version).toBe('20260529');
  });

  it('macOS — empty cert hash is omitted from the payload, not sent as ""', async () => {
    mockGetBuildCertHash.mockReturnValueOnce('');
    const signals = await collectAttestationSignals({ platform: 'macos', version: '0.2.7' });
    expect(signals.cert_hash).toBeUndefined();
    // The wire payload must not carry the key at all (absent ≠ empty string).
    expect('cert_hash' in signals).toBe(false);
  });
});
