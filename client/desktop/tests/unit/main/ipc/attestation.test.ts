/**
 * Unit tests for the attestation:* IPC handlers (#677).
 *
 * Coverage:
 *   - sender-frame validation rejects untrusted origins on both channels
 *   - get-token returns the cached token (delegates to attestationService)
 *   - clear-token delegates to clearAttestationToken
 *
 * The attestationService is mocked — these tests exercise the IPC plumbing
 * (frame validation + delegation), not the token cache itself (that's covered
 * by attestationService.test.ts).
 */
import { describe, expect, it, vi, beforeEach } from 'vitest';

const mocked = vi.hoisted(() => {
  const handleSpy = vi.fn();
  const getAttestationToken = vi.fn();
  const clearAttestationToken = vi.fn();
  return { handleSpy, getAttestationToken, clearAttestationToken };
});

vi.mock('electron', () => ({
  ipcMain: {
    handle: mocked.handleSpy,
  },
}));

vi.mock('@/main/attestationService', () => ({
  getAttestationToken: mocked.getAttestationToken,
  clearAttestationToken: mocked.clearAttestationToken,
}));

import { registerAttestationIpc } from '@/main/ipc/attestation';

interface FakeInvokeEvent {
  senderFrame: { url: string };
}

const TRUSTED = 'http://localhost:3001';
const UNTRUSTED = 'https://attacker.example';

const getSpaBaseUrl = () => null;

describe('attestation IPC handlers', () => {
  let getTokenHandler: (event: FakeInvokeEvent) => string | null;
  let clearTokenHandler: (event: FakeInvokeEvent) => void;

  beforeEach(() => {
    vi.clearAllMocks();
    registerAttestationIpc(getSpaBaseUrl);

    const getCall = mocked.handleSpy.mock.calls.find((c) => c[0] === 'attestation:get-token');
    const clearCall = mocked.handleSpy.mock.calls.find((c) => c[0] === 'attestation:clear-token');
    if (!getCall || !clearCall) {
      throw new Error('IPC handlers not registered');
    }
    getTokenHandler = getCall[1];
    clearTokenHandler = clearCall[1];
  });

  describe('sender-frame validation', () => {
    it('attestation:get-token throws for untrusted sender frames', () => {
      expect(() => getTokenHandler({ senderFrame: { url: UNTRUSTED } })).toThrow(/untrusted/i);
      expect(mocked.getAttestationToken).not.toHaveBeenCalled();
    });

    it('attestation:clear-token throws for untrusted sender frames', () => {
      expect(() => clearTokenHandler({ senderFrame: { url: UNTRUSTED } })).toThrow(/untrusted/i);
      expect(mocked.clearAttestationToken).not.toHaveBeenCalled();
    });
  });

  describe('delegation on trusted frames', () => {
    it('attestation:get-token returns the cached token from attestationService', () => {
      mocked.getAttestationToken.mockReturnValue('tok-abc');
      const result = getTokenHandler({ senderFrame: { url: TRUSTED } });
      expect(result).toBe('tok-abc');
      expect(mocked.getAttestationToken).toHaveBeenCalledTimes(1);
    });

    it('attestation:get-token returns null when no token is cached', () => {
      mocked.getAttestationToken.mockReturnValue(null);
      const result = getTokenHandler({ senderFrame: { url: TRUSTED } });
      expect(result).toBeNull();
    });

    it('attestation:clear-token delegates to clearAttestationToken', () => {
      clearTokenHandler({ senderFrame: { url: TRUSTED } });
      expect(mocked.clearAttestationToken).toHaveBeenCalledTimes(1);
    });
  });
});
