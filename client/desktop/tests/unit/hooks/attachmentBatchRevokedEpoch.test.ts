/**
 * Attachment batch sealed under an epoch revoked mid-batch.
 *
 * uploadAll used to read ONE {channelKey, keyVersion} pair before the batch
 * and seal every file with it. The label stayed honest, but a file whose
 * encryption started after the client processed `key_revocation`
 * (invalidateChannelKey, as the WebSocket handler does) was still sealed under
 * the revoked epoch, which the removed member holds. The server admits it:
 * attachment uploads check only that the epoch was issued
 * (validateAttestedEpoch, internal/media/upload_session.go), deliberately.
 *
 * Real e2eeService and real encryptFile/decryptFile; only the server is faked.
 * From @red-team's pre-PR review of dm-send-atomic-epoch (F1).
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import {
  generateRegistrationKeys,
  generateChannelKey,
  wrapChannelKey,
} from '@/renderer/utils/crypto/crypto';
import { decryptFile } from '@/renderer/utils/crypto/attachmentCrypto';
import { e2eeService } from '@/renderer/services/e2ee/e2eeService';
import { useE2EEStore } from '@/renderer/stores/auth/e2eeStore';
import { useFileUpload } from '@/renderer/hooks/messaging/useFileUpload';

const h = vi.hoisted(() => ({
  apiFetch: vi.fn(),
  sealed: [] as ArrayBuffer[],
}));

vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: (...args: unknown[]) => h.apiFetch(...args),
  safeJson: async (res: { json: () => Promise<unknown> }) => res.json(),
  API_BASE: 'http://localhost:8080',
}));

vi.mock('@/renderer/services/messaging/attachmentUploadSession', () => ({
  uploadAttachmentChunked: vi.fn(),
  abandonSessionOnUnload: vi.fn(),
  UploadAbortedError: class UploadAbortedError extends Error {},
}));

vi.mock('@/renderer/utils/crypto/attachmentCrypto', async () => {
  const actual = await vi.importActual<typeof import('@/renderer/utils/crypto/attachmentCrypto')>(
    '@/renderer/utils/crypto/attachmentCrypto'
  );
  return {
    ...actual,
    encryptFile: async (data: ArrayBuffer, key: CryptoKey) => {
      const out = await actual.encryptFile(data, key);
      h.sealed.push(out);
      return out;
    },
  };
});

const CH = '22222222-2222-4222-8222-222222222222';
const PW = 'TestPassword123!'; // pragma: allowlist secret

async function opens(blob: ArrayBuffer, key: CryptoKey): Promise<boolean> {
  try {
    await decryptFile(blob, key);
    return true;
  } catch {
    return false;
  }
}

describe('attachment batch across a mid-batch key_revocation', () => {
  beforeEach(() => {
    e2eeService.clearKeys();
    (e2eeService as unknown as { rateLimitedUntil: number }).rateLimitedUntil = 0;
    useE2EEStore.getState().reset();
    h.apiFetch.mockReset();
    h.sealed.length = 0;
  });
  afterEach(() => e2eeService.clearKeys());

  // regression: dm-send-atomic-epoch review F1
  it('seals a file encrypted after key_revocation under the successor epoch, not the revoked one', async () => {
    const reg = await generateRegistrationKeys(PW);
    await e2eeService.initialize(PW, reg.wrappedPrivateKey, reg.keyDerivationSalt);
    const k1 = await generateChannelKey(); // held by the member being removed
    const k2 = await generateChannelKey(); // successor
    const wraps: Record<number, string> = {
      1: await wrapChannelKey(k1, reg.publicKey),
      2: await wrapChannelKey(k2, reg.publicKey),
    };
    let servedEpoch = 1;
    const labels: string[] = [];

    h.apiFetch.mockImplementation(async (url: unknown, init?: RequestInit) => {
      const u = String(url);
      if (u.startsWith(`/api/v1/e2ee/keys/${CH}`)) {
        return {
          ok: true,
          status: 200,
          json: async () => ({
            key: { wrapped_key: wraps[servedEpoch], key_version: servedEpoch },
          }),
        };
      }
      if (u === '/api/v1/media/upload/attachment') {
        labels.push(String((init?.body as FormData).get('key_version')));
        if (labels.length === 1) {
          // While file 1 is in flight a member is removed: the key_revocation
          // handler invalidates the channel key and the rotation lands epoch 2.
          e2eeService.invalidateChannelKey(CH);
          servedEpoch = 2;
        }
        return {
          ok: true,
          status: 201,
          json: async () => ({
            file_id: `file-${labels.length}`,
            file_type: 'file',
            file_size: 16,
          }),
        };
      }
      throw new Error(`unexpected request ${u}`);
    });

    const { result } = renderHook(() => useFileUpload());
    await act(async () => {
      await result.current.addFiles([
        new File([new Uint8Array(16).fill(1)], 'a.bin', { type: 'application/octet-stream' }),
        new File([new Uint8Array(16).fill(2)], 'b.bin', { type: 'application/octet-stream' }),
      ]);
    });
    let uploaded: { ids: string[] } | undefined;
    await act(async () => {
      uploaded = await result.current.uploadAll(CH);
    });

    expect(uploaded?.ids).toEqual(['file-1', 'file-2']);
    expect(h.sealed).toHaveLength(2);
    // Positive control: file 1 was sealed before the revocation, under epoch 1.
    expect(labels[0]).toBe('1');
    expect(await opens(h.sealed[0], k1)).toBe(true);
    // File 2 began encrypting after invalidateChannelKey.
    expect(
      await opens(h.sealed[1], k1),
      'file 2 opens with the REVOKED epoch-1 key the removed member holds'
    ).toBe(false);
    expect(labels[1]).toBe('2');
    expect(await opens(h.sealed[1], k2)).toBe(true);
  });

  // regression: dm-send-atomic-epoch review cycle 2, LOW-2
  it('fails only the file whose key read fails, and sends nothing for it', async () => {
    const reg = await generateRegistrationKeys(PW);
    await e2eeService.initialize(PW, reg.wrappedPrivateKey, reg.keyDerivationSalt);
    const k1 = await generateChannelKey();
    const wrap1 = await wrapChannelKey(k1, reg.publicKey);
    let keyFetches = 0;
    let uploads = 0;

    h.apiFetch.mockImplementation(async (url: unknown) => {
      const u = String(url);
      if (u.startsWith(`/api/v1/e2ee/keys/${CH}`)) {
        keyFetches += 1;
        if (keyFetches > 1) throw new Error('key fetch failed');
        return {
          ok: true,
          status: 200,
          json: async () => ({ key: { wrapped_key: wrap1, key_version: 1 } }),
        };
      }
      if (u === '/api/v1/media/upload/attachment') {
        uploads += 1;
        // File 1 is in flight when the key is invalidated, so file 2 must fetch
        // a fresh key, and that fetch fails.
        e2eeService.invalidateChannelKey(CH);
        return {
          ok: true,
          status: 201,
          json: async () => ({ file_id: `file-${uploads}`, file_type: 'file', file_size: 16 }),
        };
      }
      throw new Error(`unexpected request ${u}`);
    });

    const { result } = renderHook(() => useFileUpload());
    await act(async () => {
      await result.current.addFiles([
        new File([new Uint8Array(16).fill(1)], 'a.bin', { type: 'application/octet-stream' }),
        new File([new Uint8Array(16).fill(2)], 'b.bin', { type: 'application/octet-stream' }),
      ]);
    });
    let uploaded: { ids: string[] } | undefined;
    await act(async () => {
      uploaded = await result.current.uploadAll(CH);
    });

    expect(keyFetches, 'file 2 asked for a fresh key').toBe(2);
    expect(uploads, 'no upload request was sent for file 2').toBe(1);
    expect(uploaded?.ids).toEqual(['file-1']);
    expect(result.current.files.map((f) => f.status)).toEqual(['done', 'error']);
    expect(result.current.isUploading).toBe(false);
  });
});
