/**
 * Attachment sealed under a key the client already knows was revoked.
 *
 * The per-file key read (attachmentBatchRevokedEpoch.test.ts) fixed a pair
 * carried across a whole batch. Inside one file a pair could still outlive a
 * processed key_revocation: the chunked path keeps its key across the session
 * open (a network round trip) and across a 410 restart that re-seals the whole
 * file, and the legacy path builds its form after encrypting without checking
 * the key is still current. For a DM this is not a pre-successor window:
 * dm_rotation.go commits the successor wraps before broadcasting
 * key_revocation, so a fresh read already returns the successor.
 *
 * Real e2eeService, useFileUpload, uploadAttachmentChunked and AES-GCM; only
 * the server (apiFetch) is faked. The two crypto wrappers below only record
 * the moment a seal starts and call straight through.
 * From @red-team's round-3 review of dm-send-atomic-epoch (PoCs 1-3).
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import {
  generateRegistrationKeys,
  generateChannelKey,
  wrapChannelKey,
} from '@/renderer/utils/crypto/crypto';
import { decryptAttachmentBlob } from '@/renderer/utils/crypto/attachmentChunkedCrypto';
import { e2eeService } from '@/renderer/services/e2ee/e2eeService';
import { useE2EEStore } from '@/renderer/stores/auth/e2eeStore';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';
import { useFileUpload } from '@/renderer/hooks/messaging/useFileUpload';

const h = vi.hoisted(() => ({
  apiFetch: vi.fn(),
  /** Runs while a legacy seal is in flight (WebCrypto dispatched, not resolved). */
  duringSeal: null as null | (() => void),
}));

vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: (...args: unknown[]) => h.apiFetch(...args),
  safeJson: async (res: { json: () => Promise<unknown> }) => res.json(),
  API_BASE: 'http://localhost:8080',
}));

// Legacy path: dispatch the real AES-GCM, then run the scenario while it is
// pending, which is exactly where a WebSocket handler can run.
vi.mock('@/renderer/utils/crypto/attachmentCrypto', async () => {
  const actual = await vi.importActual<typeof import('@/renderer/utils/crypto/attachmentCrypto')>(
    '@/renderer/utils/crypto/attachmentCrypto'
  );
  return {
    ...actual,
    encryptFile: async (data: ArrayBuffer, key: CryptoKey) => {
      const pending = actual.encryptFile(data, key);
      h.duringSeal?.();
      return pending;
    },
  };
});

const CONV = '33333333-3333-4333-8333-333333333333';
const PW = 'TestPassword123!'; // pragma: allowlist secret
const SESSION = '/api/v1/media/upload/attachment/session';
const SIDS = ['A'.repeat(43), 'B'.repeat(43)];

async function opensChunked(part: Uint8Array, key: CryptoKey): Promise<boolean> {
  try {
    await decryptAttachmentBlob(new Uint8Array(part), key, 'application/octet-stream');
    return true;
  } catch {
    return false;
  }
}

interface Fixture {
  k1: CryptoKey; // revoked epoch: the removed member holds it
  k2: CryptoKey; // successor, committed before key_revocation is broadcast
  /** What the client does on key_revocation: the WS handler's invalidation. */
  processKeyRevocation: () => void;
}

let route: (u: string, init?: RequestInit) => Promise<unknown> = async (u) => {
  throw new Error(`unexpected request ${u}`);
};

async function fixture(): Promise<Fixture> {
  const reg = await generateRegistrationKeys(PW);
  await e2eeService.initialize(PW, reg.wrappedPrivateKey, reg.keyDerivationSalt);
  const k1 = await generateChannelKey();
  const k2 = await generateChannelKey();
  const wraps: Record<number, string> = {
    1: await wrapChannelKey(k1, reg.publicKey),
    2: await wrapChannelKey(k2, reg.publicKey),
  };
  let servedEpoch = 1;
  h.apiFetch.mockImplementation(async (url: unknown, init?: RequestInit) => {
    const u = String(url);
    if (u.startsWith(`/api/v1/e2ee/keys/${CONV}`) && !init?.method) {
      return {
        ok: true,
        status: 200,
        json: async () => ({
          key: { wrapped_key: wraps[servedEpoch], key_version: servedEpoch },
        }),
      };
    }
    return route(u, init);
  });
  return {
    k1,
    k2,
    processKeyRevocation: () => {
      e2eeService.invalidateChannelKey(CONV);
      servedEpoch = 2;
    },
  };
}

function okJson(status: number, body: unknown) {
  return { ok: true, status, json: async () => body, clone: () => ({ text: async () => '' }) };
}

function sessionKeyVersion(init?: RequestInit): unknown {
  return (JSON.parse(String(init?.body)) as { key_version?: unknown }).key_version;
}

const fileOf = (name: string, fill: number) =>
  new File([new Uint8Array(16).fill(fill)], name, { type: 'application/octet-stream' });

async function runUpload(files: File[]): Promise<{ ids: string[] | undefined; status: string[] }> {
  const { result } = renderHook(() => useFileUpload());
  await act(async () => {
    await result.current.addFiles(files);
  });
  let uploaded: { ids: string[] } | undefined;
  await act(async () => {
    uploaded = await result.current.uploadAll(CONV, CONV);
  });
  return { ids: uploaded?.ids, status: result.current.files.map((x) => x.status) };
}

describe('attachment upload after a processed key_revocation', () => {
  beforeEach(() => {
    e2eeService.clearKeys();
    (e2eeService as unknown as { rateLimitedUntil: number }).rateLimitedUntil = 0;
    useE2EEStore.getState().reset();
    useClientConfigStore.getState().setChunkedUploadCapability({ status: 'loading' });
    h.apiFetch.mockReset();
    h.duringSeal = null;
  });
  afterEach(() => {
    useClientConfigStore.getState().setChunkedUploadCapability({ status: 'loading' });
    e2eeService.clearKeys();
  });

  // regression: dm-send-atomic-epoch round-3 review, @red-team PoC-1
  it('chunked: a revocation during the session open sends no part, errors the file and cancels the session', async () => {
    useClientConfigStore.getState().setChunkedUploadCapability({ status: 'supported' });
    const f = await fixture();
    const puts: string[] = [];
    const deletes: string[] = [];
    route = async (u, init) => {
      if (u === SESSION && init?.method === 'POST') {
        f.processKeyRevocation();
        return okJson(201, { session_id: SIDS[0] });
      }
      if (init?.method === 'PUT') {
        puts.push(u);
        return okJson(200, {});
      }
      if (init?.method === 'DELETE') {
        deletes.push(u);
        return okJson(204, {});
      }
      throw new Error(`unexpected request ${init?.method ?? 'GET'} ${u}`);
    };

    const r = await runUpload([fileOf('a.bin', 7)]);

    expect(puts, 'a part sealed under the revoked key was sent').toEqual([]);
    expect(r.ids).toEqual([]);
    expect(r.status).toEqual(['error']);
    expect(deletes).toEqual([`${SESSION}/${SIDS[0]}`]);
  });

  // regression: dm-send-atomic-epoch round-3 review, @red-team PoC-2
  it('chunked: a 410 restart after a revocation sends no re-sealed part', async () => {
    useClientConfigStore.getState().setChunkedUploadCapability({ status: 'supported' });
    const f = await fixture();
    const restartPuts: string[] = [];
    let opened = 0;
    route = async (u, init) => {
      if (u === SESSION && init?.method === 'POST') {
        return okJson(201, { session_id: SIDS[opened++] });
      }
      if (u === `${SESSION}/${SIDS[0]}/chunk/0` && init?.method === 'PUT') {
        f.processKeyRevocation();
        return { ok: false, status: 410, clone: () => ({ text: async () => 'gone' }) };
      }
      if (init?.method === 'PUT') {
        restartPuts.push(u);
        return okJson(200, {});
      }
      if (init?.method === 'DELETE') return okJson(204, {});
      throw new Error(`unexpected request ${init?.method ?? 'GET'} ${u}`);
    };

    const r = await runUpload([fileOf('b.bin', 9)]);

    expect(restartPuts, 'the restart re-sealed the file under the revoked key').toEqual([]);
    expect(r.ids).toEqual([]);
    expect(r.status).toEqual(['error']);
  });

  // regression: dm-send-atomic-epoch round-3 review, @red-team PoC-3
  it('legacy: a revocation during the in-flight encryption sends no upload request', async () => {
    const f = await fixture();
    const uploads: string[] = [];
    route = async (u, init) => {
      if (u === '/api/v1/media/upload/attachment') {
        uploads.push(String((init?.body as FormData).get('key_version')));
        return okJson(201, { file_id: 'file-l', file_type: 'file', file_size: 16 });
      }
      throw new Error(`unexpected request ${init?.method ?? 'GET'} ${u}`);
    };
    let fired = false;
    h.duringSeal = () => {
      if (fired) return;
      fired = true;
      f.processKeyRevocation();
    };

    const r = await runUpload([fileOf('c.bin', 5)]);

    expect(uploads, 'a file sealed under the revoked key was uploaded').toEqual([]);
    expect(r.ids).toEqual([]);
    expect(r.status).toEqual(['error']);
  });

  // Positive control, and the chunked-path epoch pin (round-3 @test-reviewer P1).
  it('chunked: without a revocation the upload completes, labelled and sealed under epoch 1', async () => {
    useClientConfigStore.getState().setChunkedUploadCapability({ status: 'supported' });
    const f = await fixture();
    const parts: Uint8Array[] = [];
    const openedVersions: unknown[] = [];
    route = async (u, init) => {
      if (u === SESSION && init?.method === 'POST') {
        openedVersions.push(sessionKeyVersion(init));
        return okJson(201, { session_id: SIDS[0] });
      }
      if (u === `${SESSION}/${SIDS[0]}/chunk/0` && init?.method === 'PUT') {
        parts.push(new Uint8Array(init.body as Uint8Array));
        return okJson(200, {});
      }
      if (u === `${SESSION}/${SIDS[0]}/commit`) {
        return okJson(200, {
          file_id: 'file-ok',
          storage_key: 'k',
          file_type: 'file',
          file_size: 16,
        });
      }
      throw new Error(`unexpected request ${init?.method ?? 'GET'} ${u}`);
    };

    const r = await runUpload([fileOf('d.bin', 3)]);

    expect(r.ids).toEqual(['file-ok']);
    expect(openedVersions).toEqual([1]);
    expect(await opensChunked(parts[0], f.k1)).toBe(true);
  });

  // round-3 @test-reviewer P1: the chunked path reads its pair per file.
  it('chunked: a file that starts after a revocation is labelled and sealed under the successor', async () => {
    useClientConfigStore.getState().setChunkedUploadCapability({ status: 'supported' });
    const f = await fixture();
    const parts: Uint8Array[] = [];
    const openedVersions: unknown[] = [];
    let opened = 0;
    route = async (u, init) => {
      if (u === SESSION && init?.method === 'POST') {
        openedVersions.push(sessionKeyVersion(init));
        return okJson(201, { session_id: SIDS[opened++] });
      }
      if (init?.method === 'PUT') {
        parts.push(new Uint8Array(init.body as Uint8Array));
        return okJson(200, {});
      }
      if (u.endsWith('/commit')) {
        // File 1 is done before the revocation arrives, so file 2 starts after it.
        if (opened === 1) f.processKeyRevocation();
        return okJson(200, {
          file_id: `file-${opened}`,
          storage_key: 'k',
          file_type: 'file',
          file_size: 16,
        });
      }
      throw new Error(`unexpected request ${init?.method ?? 'GET'} ${u}`);
    };

    const r = await runUpload([fileOf('e.bin', 1), fileOf('f.bin', 2)]);

    expect(r.ids).toEqual(['file-1', 'file-2']);
    expect(openedVersions).toEqual([1, 2]);
    expect(await opensChunked(parts[0], f.k1)).toBe(true);
    expect(await opensChunked(parts[1], f.k1), 'file 2 opens with the revoked key').toBe(false);
    expect(await opensChunked(parts[1], f.k2)).toBe(true);
  });
});
