import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  getAttestationTokenSafe,
  loadMediaAttestationRelay,
} from '@/renderer/services/system/attestationToken';

const origElectron = globalThis.electron;

function setBridge(bridge: Record<string, unknown> | undefined) {
  (globalThis as any).electron = bridge;
}

describe('attestationToken', () => {
  afterEach(() => {
    globalThis.electron = origElectron;
    vi.restoreAllMocks();
  });

  it('reads the token and machine id for the media handshake', async () => {
    setBridge({
      attestation: { getToken: vi.fn().mockResolvedValue('att') },
      getMachineId: vi.fn().mockResolvedValue('mid'),
    });
    await expect(loadMediaAttestationRelay('https://api.test')).resolves.toEqual({
      attestation_token: 'att',
      machine_id: 'mid',
    });
    expect(globalThis.electron?.getMachineId).toHaveBeenCalledWith('https://api.test');
  });

  it('returns nothing when there is no desktop bridge', async () => {
    setBridge(undefined);
    await expect(getAttestationTokenSafe()).resolves.toBeNull();
    await expect(loadMediaAttestationRelay('https://api.test')).resolves.toEqual({});
  });

  it('never throws when either IPC call fails', async () => {
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    setBridge({
      attestation: { getToken: vi.fn().mockRejectedValue(new Error('untrusted frame')) },
      getMachineId: vi.fn().mockRejectedValue(new Error('ipc down')),
    });
    await expect(loadMediaAttestationRelay('https://api.test')).resolves.toEqual({});
  });

  it('keeps the machine id when only the token read fails', async () => {
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    setBridge({
      attestation: { getToken: vi.fn().mockRejectedValue(new Error('untrusted frame')) },
      getMachineId: vi.fn().mockResolvedValue('mid'),
    });
    await expect(loadMediaAttestationRelay('https://api.test')).resolves.toEqual({
      machine_id: 'mid',
    });
  });

  it('sends the token alone on a bridge without getMachineId', async () => {
    setBridge({ attestation: { getToken: vi.fn().mockResolvedValue('att') } });
    await expect(loadMediaAttestationRelay('https://api.test')).resolves.toEqual({
      attestation_token: 'att',
    });
  });
});
