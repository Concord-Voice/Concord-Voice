import { render, screen, fireEvent, waitFor, act } from '../../../test-utils';
import { vi, beforeAll, afterAll, beforeEach, afterEach } from 'vitest';
import { ResponderDeviceRecoveryAttempt } from '@/renderer/services/system/deviceRecoveryService';
import { _resetClientVersionCache } from '@/renderer/utils/runtime/clientVersion';
import * as apiClient from '@/renderer/services/system/apiClient';
import { http, HttpResponse } from 'msw';
import { setupServer } from 'msw/node';
import vectors from '../../../../../../docs/design/trusted-recovery-v2-vectors.json';
import RecoveryApprovalModal from '@/renderer/components/Auth/RecoveryApprovalModal';
import {
  apiUrl,
  getApiBase,
  resetRuntimeServerBase,
  setRuntimeServerBase,
} from '@/renderer/services/system/runtimeServerBase';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { e2eeService } from '@/renderer/services/e2ee/e2eeService';
import {
  generateKeyPair,
  arrayBufferToBase64,
  exportECDHPublicKey,
  wrapPrivateKey,
  generateDeviceRecoveryKeyPair,
  randomRecoveryNonce,
} from '@/renderer/utils/crypto/crypto';
import {
  deriveDeviceRecoveryKeys,
  decryptDeviceRecoveryPayload,
} from '@/renderer/utils/crypto/trustedDeviceRecovery';
import type {
  DeviceRecoveryContext,
  DeviceRecoveryOffer,
} from '@/renderer/services/system/deviceRecoveryContract';

const server = setupServer();
let wrappingKey: CryptoKey;
let wrappedPrivateKey: string;
let originalPkcs8: ArrayBuffer;
let requester: CryptoKeyPair;
let context: DeviceRecoveryContext;
const submissions: Array<Record<string, unknown>> = [];
const onClose = vi.fn();
const onResolved = vi.fn();
const originalElectronBridge = globalThis.electron;
beforeAll(async () => {
  server.listen({ onUnhandledRequest: 'error' });
  const accountKeys = await generateKeyPair();
  originalPkcs8 = await crypto.subtle.exportKey('pkcs8', accountKeys.privateKey);
  wrappingKey = await crypto.subtle.generateKey({ name: 'AES-GCM', length: 256 }, false, [
    'wrapKey',
    'unwrapKey',
  ]);
  wrappedPrivateKey = arrayBufferToBase64(
    await wrapPrivateKey(accountKeys.privateKey, wrappingKey)
  );
});
afterAll(() => server.close());
beforeEach(async () => {
  resetAllStores();
  resetRuntimeServerBase();
  useAuthStore.getState().setAccessToken('mock-token');
  useUserStore.setState({
    user: { id: vectors.user_id, username: 'test', email: 'test@example.com' },
  });
  submissions.length = 0;
  onClose.mockReset();
  onResolved.mockReset();
  requester = await generateDeviceRecoveryKeyPair();
  context = {
    ...vectors.context,
    protocol_version: 2,
    server_origin: new URL(getApiBase()).origin,
    expires_at: Date.now() + 60_000,
    requester_public_key: await exportECDHPublicKey(requester.publicKey),
    requester_nonce: randomRecoveryNonce(),
  };
  vi.spyOn(e2eeService, 'getWrappingKey').mockReturnValue(wrappingKey);
  vi.spyOn(e2eeService, 'getWrappedPrivateKey').mockReturnValue(wrappedPrivateKey);
  server.use(
    http.post(
      apiUrl(`/api/v1/mfa/recovery-requests/${context.request_id}/respond`),
      async ({ request }) => {
        const body = (await request.json()) as Record<string, unknown>;
        submissions.push(body);
        return HttpResponse.json({
          request_id: context.request_id,
          protocol_version: 2,
          status:
            body.action === 'offer'
              ? 'offered'
              : body.action === 'approve'
                ? 'approved'
                : 'rejected',
        });
      }
    )
  );
});
afterEach(() => {
  server.resetHandlers();
  vi.restoreAllMocks();
  resetRuntimeServerBase();
  (globalThis as unknown as { electron: unknown }).electron = originalElectronBridge;
  _resetClientVersionCache();
});
function mount(request = { ...context, status: 'pending' as const }) {
  return render(
    <RecoveryApprovalModal request={request} onClose={onClose} onResolved={onResolved} />
  );
}
async function ready() {
  await screen.findByText('Ready to compare. Your account key remains locked until you confirm.');
}
function confirm() {
  fireEvent.click(screen.getByRole('button', { name: 'These fingerprints match' }));
}
function holdRejectionIPC(boundary: 'version' | 'attestation') {
  let entered = () => {};
  let release = () => {};
  const paused = new Promise<void>((resolve) => {
    entered = resolve;
  });
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  _resetClientVersionCache();
  (globalThis as unknown as { electron: unknown }).electron = {
    ...originalElectronBridge,
    getVersion: async () => {
      if (boundary === 'version') {
        entered();
        await held;
      }
      return '1.2.3';
    },
    attestation: {
      getToken: async () => {
        if (boundary === 'attestation') {
          entered();
          await held;
        }
        return 'synthetic-attestation';
      },
    },
  };
  return { paused, release };
}

describe('RecoveryApprovalModal fingerprint ceremony', () => {
  it.each(['auth', 'server', 'keys', 'expiry', 'user'] as const)(
    'refuses detached rejection and acknowledgement when %s changes during close IPC',
    async (change) => {
      let unmount = () => {};
      const rendered = render(
        <RecoveryApprovalModal
          request={{ ...context, status: 'pending' }}
          onClose={() => unmount()}
          onResolved={onResolved}
        />
      );
      unmount = rendered.unmount;
      await ready();
      const dispatch = vi.spyOn(apiClient, 'apiFetch');
      const ipc = holdRejectionIPC('version');
      fireEvent.click(screen.getAllByRole('button', { name: 'Close' })[0]);
      await ipc.paused;
      const pending = dispatch.mock.results[0].value as Promise<Response>;
      await act(async () => {
        if (change === 'auth')
          useAuthStore.setState({ authGeneration: useAuthStore.getState().authGeneration + 1 });
        if (change === 'server') setRuntimeServerBase('https://changed.example.test');
        if (change === 'keys') e2eeService.clearKeys();
        if (change === 'expiry') vi.spyOn(Date, 'now').mockReturnValue(context.expires_at);
        if (change === 'user')
          useUserStore.setState({
            user: {
              id: '11111111-2222-4333-8444-000000000001',
              username: 'changed',
              email: 'changed@example.test',
            },
          });
        ipc.release();
        await pending.catch(() => {});
      });
      expect(submissions.map((body) => body.action)).toEqual(['offer']);
      expect(onResolved).not.toHaveBeenCalled();
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    }
  );
  it.each(['version', 'attestation'] as const)(
    'rejects exactly once after close unmounts while %s IPC is held',
    async (boundary) => {
      let unmount = () => {};
      const closed = vi.fn(() => unmount());
      const rendered = render(
        <RecoveryApprovalModal
          request={{ ...context, status: 'pending' }}
          onClose={closed}
          onResolved={onResolved}
        />
      );
      unmount = rendered.unmount;
      await ready();
      const ipc = holdRejectionIPC(boundary);
      fireEvent.click(screen.getAllByRole('button', { name: 'Close' })[0]);
      await ipc.paused;
      expect(closed).toHaveBeenCalledOnce();
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      expect(submissions.map((body) => body.action)).toEqual(['offer']);
      ipc.release();
      await waitFor(() =>
        expect(submissions.map((body) => body.action)).toEqual(['offer', 'reject'])
      );
      await waitFor(() => expect(onResolved).toHaveBeenCalledOnce());
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    }
  );
  it('offers once without rejecting during StrictMode replay, then rejects on real unmount', async () => {
    const disposed = vi.spyOn(ResponderDeviceRecoveryAttempt.prototype, 'dispose');
    const rendered = render(
      <RecoveryApprovalModal request={{ ...context, status: 'pending' }} onClose={onClose} />,
      { reactStrictMode: true }
    );
    await ready();
    expect(disposed).toHaveBeenCalledOnce();
    expect(submissions.map((body) => body.action)).toEqual(['offer']);
    expect(e2eeService.getWrappingKey).not.toHaveBeenCalled();
    rendered.unmount();
    await waitFor(() =>
      expect(submissions.map((body) => body.action)).toEqual(['offer', 'reject'])
    );
    expect(onResolved).not.toHaveBeenCalled();
  });
  it('cancels the old unmount rejection when the same modal receives a different request', async () => {
    const rendered = mount();
    await ready();
    const replacement = {
      ...context,
      request_id: '11111111-2222-4333-8444-000000000001',
      status: 'pending' as const,
    };
    server.use(
      http.post(
        apiUrl(`/api/v1/mfa/recovery-requests/${replacement.request_id}/respond`),
        async ({ request }) => {
          const body = (await request.json()) as Record<string, unknown>;
          submissions.push(body);
          return HttpResponse.json({
            request_id: replacement.request_id,
            protocol_version: 2,
            status: 'offered',
          });
        }
      )
    );
    rendered.rerender(<RecoveryApprovalModal request={replacement} onClose={onClose} />);
    await waitFor(() =>
      expect(submissions.filter((body) => body.action === 'offer')).toHaveLength(2)
    );
    expect(submissions.some((body) => body.action === 'reject')).toBe(false);
  });
  it('offers only ephemeral material on Review, keeps custody locked, and focuses description', async () => {
    const unwrap = vi.spyOn(crypto.subtle, 'unwrapKey');
    const exported = vi.spyOn(crypto.subtle, 'exportKey');
    mount();
    await ready();
    expect(submissions).toHaveLength(1);
    expect(submissions[0]).toMatchObject({ action: 'offer', protocol_version: 2 });
    expect(submissions[0]).not.toHaveProperty('encrypted_payload');
    expect(e2eeService.getWrappingKey).not.toHaveBeenCalled();
    expect(e2eeService.getWrappedPrivateKey).not.toHaveBeenCalled();
    expect(unwrap).not.toHaveBeenCalled();
    expect(exported.mock.calls.some(([format]) => format === 'pkcs8')).toBe(false);
    expect(screen.getByRole('group', { name: 'Recovery fingerprint' }).children).toHaveLength(8);
    await waitFor(() =>
      expect(screen.getByText(/Compare all eight fingerprint groups/)).toHaveFocus()
    );
  });
  it('confirms this digest then transfers the original RSA key through real crypto/HTTP', async () => {
    mount();
    await ready();
    confirm();
    await screen.findByText(
      'Approval submitted. Confirm the fingerprint on the recovering device to continue.'
    );
    expect(submissions).toHaveLength(2);
    const sentOffer = submissions[0] as unknown as DeviceRecoveryOffer;
    const material = await deriveDeviceRecoveryKeys(
      'requester',
      requester.privateKey,
      context,
      sentOffer
    );
    expect(submissions[1]).toEqual({
      action: 'approve',
      protocol_version: 2,
      transcript_hash: material.transcriptHash,
      encrypted_payload: submissions[1].encrypted_payload,
    });
    expect(
      new Uint8Array(
        await decryptDeviceRecoveryPayload(material, String(submissions[1].encrypted_payload))
      )
    ).toEqual(new Uint8Array(originalPkcs8));
    expect(onResolved).toHaveBeenCalledWith(context.request_id);
    fireEvent.click(screen.getAllByRole('button', { name: 'Close' })[0]);
    expect(onClose).toHaveBeenCalledOnce();
  });
  it('does not reoffer an immutable offered row after losing the private attempt', async () => {
    render(
      <RecoveryApprovalModal
        request={{ ...context, ...vectors.offer, status: 'offered' }}
        onClose={onClose}
        onResolved={onResolved}
      />
    );
    await screen.findByRole('alert');
    expect(screen.getByRole('alert')).toHaveTextContent('comparison key');
    expect(submissions).toHaveLength(0);
    expect(e2eeService.getWrappingKey).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: 'These fingerprints match' })).toBeDisabled();
    fireEvent.click(screen.getByRole('button', { name: 'Reject' }));
    await screen.findByText('Rejection acknowledged.');
    expect(submissions).toEqual([{ action: 'reject', protocol_version: 2 }]);
  });
  it.each(['account', 'server', 'key custody', 'expiry', 'unmount'] as const)(
    'fences %s during unwrap before export or approval',
    async (change) => {
      const originalUnwrap = crypto.subtle.unwrapKey.bind(crypto.subtle);
      let unmount = () => {};
      let finishUnwrap = () => {};
      const unwrapFinished = new Promise<void>((resolve) => {
        finishUnwrap = resolve;
      });
      vi.spyOn(crypto.subtle, 'unwrapKey').mockImplementation(async (...args) => {
        const key = await originalUnwrap(...args);
        if (change === 'account')
          useAuthStore.setState({ authGeneration: useAuthStore.getState().authGeneration + 1 });
        if (change === 'server') setRuntimeServerBase('https://other.example.test');
        if (change === 'key custody') e2eeService.clearKeys();
        if (change === 'expiry') vi.spyOn(Date, 'now').mockReturnValue(context.expires_at);
        if (change === 'unmount') unmount();
        finishUnwrap();
        return key;
      });
      const exported = vi.spyOn(crypto.subtle, 'exportKey');
      const rendered = mount();
      unmount = rendered.unmount;
      await ready();
      confirm();
      if (change === 'unmount') {
        await act(async () => {
          await unwrapFinished;
        });
      } else {
        await screen.findByRole('alert');
      }
      expect(exported.mock.calls.some(([format]) => format === 'pkcs8')).toBe(false);
      expect(submissions.some((body) => body.action === 'approve')).toBe(false);
    }
  );
  it.each(['export', 'encrypt'] as const)(
    'wipes plaintext when the server changes during %s',
    async (boundary) => {
      const originalExport = crypto.subtle.exportKey.bind(crypto.subtle);
      const originalEncrypt = crypto.subtle.encrypt.bind(crypto.subtle);
      let raw: ArrayBuffer | null = null;
      vi.spyOn(crypto.subtle, 'exportKey').mockImplementation(async (...args) => {
        const result = await originalExport(...args);
        if (args[0] === 'pkcs8') {
          raw = result as ArrayBuffer;
          if (boundary === 'export') setRuntimeServerBase('https://changed.example.test');
        }
        return result;
      });
      vi.spyOn(crypto.subtle, 'encrypt').mockImplementation(async (...args) => {
        const result = await originalEncrypt(...args);
        if (boundary === 'encrypt') setRuntimeServerBase('https://changed.example.test');
        return result;
      });
      mount();
      await ready();
      confirm();
      await screen.findByRole('alert');
      if (!raw) throw new Error('The guarded export did not return PKCS8');
      expect(new Uint8Array(raw).every((byte) => byte === 0)).toBe(true);
      expect(submissions.some((body) => body.action === 'approve')).toBe(false);
    }
  );
  it.each(['account', 'origin'] as const)(
    'rejects wrong local %s before offering or reading keys',
    async (field) => {
      mount({
        ...context,
        ...(field === 'account'
          ? { account_binding: arrayBufferToBase64(new Uint8Array(32).buffer) }
          : { server_origin: 'https://other.example.test' }),
        status: 'pending',
      });
      await screen.findByRole('alert');
      expect(submissions).toHaveLength(0);
      expect(e2eeService.getWrappingKey).not.toHaveBeenCalled();
    }
  );
  it('does not remove a row or claim rejection when rejection is refused', async () => {
    mount();
    await ready();
    server.use(
      http.post(apiUrl(`/api/v1/mfa/recovery-requests/${context.request_id}/respond`), () =>
        HttpResponse.json({}, { status: 500 })
      )
    );
    fireEvent.click(screen.getByRole('button', { name: 'Reject' }));
    await screen.findByRole('alert');
    expect(onResolved).not.toHaveBeenCalled();
    expect(onClose).not.toHaveBeenCalled();
  });
  it('requires current custody and never claims success for a refused approval', async () => {
    mount();
    await ready();
    vi.mocked(e2eeService.getWrappingKey).mockReturnValue(null);
    confirm();
    await screen.findByRole('alert');
    expect(submissions.some((body) => body.action === 'approve')).toBe(false);
    expect(onResolved).not.toHaveBeenCalled();
  });
});
