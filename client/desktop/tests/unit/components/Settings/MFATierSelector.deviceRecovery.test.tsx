import { render, screen, fireEvent, waitFor, act } from '../../../test-utils';
import { beforeAll, afterAll, beforeEach, afterEach, it, expect, vi } from 'vitest';
import { setupServer } from 'msw/node';
import { http, HttpResponse } from 'msw';
import vectors from '../../../../../../docs/design/trusted-recovery-v2-vectors.json';
import MFATierSelector from '@/renderer/components/Settings/MFATierSelector';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import {
  apiUrl,
  getApiBase,
  resetRuntimeServerBase,
} from '@/renderer/services/system/runtimeServerBase';
import { e2eeService } from '@/renderer/services/e2ee/e2eeService';
import type { DeviceRecoveryRequest } from '@/renderer/services/system/deviceRecoveryContract';
import * as recoveryService from '@/renderer/services/system/deviceRecoveryService';

const server = setupServer();
let row: DeviceRecoveryRequest;
let listStatus: number;
const props = { activeMethods: ['totp'], onSetupTOTP: vi.fn(), onSetupWebAuthn: vi.fn() };
beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterAll(() => server.close());
beforeEach(() => {
  resetAllStores();
  resetRuntimeServerBase();
  useAuthStore.getState().setAccessToken('mock-token');
  useUserStore.setState({
    user: { id: vectors.user_id, username: 'local', email: 'local@example.test' },
  });
  row = {
    ...vectors.context,
    ...vectors.offer,
    protocol_version: 2,
    status: 'offered',
    expires_at: Date.now() + 60_000,
    server_origin: new URL(getApiBase()).origin,
  };
  listStatus = 200;
  server.use(
    http.get(apiUrl('/api/v1/mfa/recovery-requests'), () =>
      HttpResponse.json({ requests: [row] }, { status: listStatus })
    ),
    http.get(apiUrl('/api/v1/mfa/recovery-key'), () =>
      HttpResponse.json({ has_recovery_key: false })
    ),
    http.get(apiUrl('/api/v1/mfa/trusted-devices'), () => HttpResponse.json({ devices: [] })),
    http.get(apiUrl('/api/v1/mfa/recovery-circle'), () => HttpResponse.json({ has_circle: false })),
    http.post(apiUrl(`/api/v1/mfa/recovery-requests/${vectors.context.request_id}/respond`), () =>
      HttpResponse.json({}, { status: 500 })
    )
  );
});
afterEach(() => {
  server.resetHandlers();
  vi.restoreAllMocks();
  resetRuntimeServerBase();
});

it('preserves an unresolved offered row after failed rejection and closing, then requires restart without reoffering', async () => {
  const keys = vi.spyOn(e2eeService, 'getWrappingKey');
  render(<MFATierSelector {...props} />);
  await screen.findByRole('button', { name: 'Review' });
  fireEvent.click(screen.getByRole('button', { name: 'Review' }));
  await screen.findByRole('alert');
  expect(screen.getByRole('alert')).toHaveTextContent('comparison key');
  fireEvent.click(screen.getByRole('button', { name: 'Reject' }));
  await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('connection failed'));
  fireEvent.click(screen.getAllByRole('button', { name: 'Close' })[0]);
  await screen.findByRole('button', { name: 'Review' });
  fireEvent.click(screen.getByRole('button', { name: 'Review' }));
  await screen.findByRole('alert');
  expect(screen.getByRole('alert')).toHaveTextContent('Reject it and start a new request');
  expect(keys).not.toHaveBeenCalled();
});
it('keeps the last validated row when a manual list refresh fails', async () => {
  render(<MFATierSelector {...props} />);
  await screen.findByRole('button', { name: 'Review' });
  listStatus = 500;
  fireEvent.click(screen.getByRole('button', { name: 'Refresh recovery requests' }));
  await screen.findByRole('alert');
  expect(screen.getByRole('button', { name: 'Review' })).toBeInTheDocument();
});
it('clears owned rows and refuses foreign binding after an account change', async () => {
  render(<MFATierSelector {...props} />);
  await screen.findByRole('button', { name: 'Review' });
  await act(async () => {
    useUserStore.setState({
      user: {
        id: '22222222-2222-4333-8444-555555555555',
        username: 'other',
        email: 'other@example.test',
      },
    });
  });
  await screen.findByRole('alert');
  expect(screen.queryByRole('button', { name: 'Review' })).not.toBeInTheDocument();
});

it('does not resurrect an acknowledged rejection when an older list refresh arrives later', async () => {
  const reads = vi.spyOn(recoveryService, 'listDeviceRecoveryRequests');
  render(<MFATierSelector {...props} />);
  await screen.findByRole('button', { name: 'Review' });
  let release: (() => void) | undefined;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  let refreshStarted = false;
  server.use(
    http.get(apiUrl('/api/v1/mfa/recovery-requests'), async () => {
      const snapshot = row;
      const unresolved = { ...row, request_id: '11111111-2222-4333-8444-000000000001' };
      refreshStarted = true;
      await held;
      return HttpResponse.json({ requests: [snapshot, unresolved] });
    }),
    http.post(apiUrl(`/api/v1/mfa/recovery-requests/${row.request_id}/respond`), () =>
      HttpResponse.json({ request_id: row.request_id, protocol_version: 2, status: 'rejected' })
    )
  );
  fireEvent.click(screen.getByRole('button', { name: 'Refresh recovery requests' }));
  await waitFor(() => expect(refreshStarted).toBe(true));
  fireEvent.click(screen.getByRole('button', { name: 'Review' }));
  await screen.findByRole('alert');
  fireEvent.click(screen.getByRole('button', { name: 'Reject' }));
  await screen.findByText('Rejection acknowledged.');
  fireEvent.click(screen.getAllByRole('button', { name: 'Close' })[0]);
  await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  expect(screen.queryByRole('button', { name: 'Review' })).not.toBeInTheDocument();
  await act(async () => {
    release?.();
    await reads.mock.results[1].value;
  });
  expect(screen.getAllByRole('button', { name: 'Review' })).toHaveLength(1);
});
