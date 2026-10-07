import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { fireEvent, render, screen, userEvent, waitFor } from '../../../test-utils';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import { FIXTURE_PW, MINT_PATH } from '../../../helpers/stepUpTokenWire';
import { useDMStore, type DMConversation } from '@/renderer/stores/chat/dmStore';
import DMThreadRemovalDialog from '@/renderer/components/DirectMessages/DMThreadRemovalDialog';

// Codex on #3509: DM Clear's MFA stage dropped the methods the server named
// and rendered a numeric code field only, so an account whose one inline
// factor is a security key could never finish a Clear. On the shared stage the
// read names the key up front, and a refusal's methods replace it.
//
// "Mutant:" comments name the production change each test exists to turn red.

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

const conversation: DMConversation = {
  id: 'dm-1',
  isGroup: false,
  isPersonal: false,
  name: 'Momo',
  participants: [
    { userId: 'user-1', username: 'alice' },
    { userId: 'user-2', username: 'momo' },
  ],
  lastMessage: { content: 'hello', userId: 'user-2', createdAt: '2026-09-23T12:00:00Z' },
  unreadCount: 0,
  createdAt: '2026-09-23T11:00:00Z',
};

const READ_PATH = '*/api/v1/mfa/step-up';
const CLEAR_PATH = '*/api/v1/dm/conversations/:id/clear';
const CODE = '123456';

beforeEach(() => {
  resetAllStores();
  useDMStore.setState({
    conversations: [conversation],
    activeConversationId: conversation.id,
    removeConversation: vi.fn(),
    discardConversationView: vi.fn(),
    fetchConversations: vi.fn(async () => {}),
    leaveGroup: vi.fn(),
  });
});

/** What the account's step-up read answers: its inline methods, strongest first. */
function readAnswers(methods: string[]) {
  return http.get(READ_PATH, () =>
    HttpResponse.json({
      methods,
      default_method: methods[0] ?? null,
      backup_code_available: false,
    })
  );
}

const passwordRequired = () =>
  HttpResponse.json(
    { error: 'Current password required to clear history', password_required: true },
    { status: 403 }
  );

const cleared = () =>
  HttpResponse.json({ conversation_id: 'dm-1', cleared_at: '2026-10-01T00:00:00Z' });

/** Opens the credential stage; with protection on, "Continue" sends nothing. */
async function openClear(onClose = vi.fn()) {
  render(
    <DMThreadRemovalDialog
      target={{ conversation, action: 'clear' }}
      onClose={onClose}
      onRemoved={vi.fn()}
    />
  );
  await userEvent.setup().click(screen.getByRole('button', { name: 'Continue' }));
  await screen.findByRole('heading', { name: 'Confirm it is you' });
  return onClose;
}

const verify = () => screen.getByRole('button', { name: 'Verify and clear' });
const enterPassword = async (value: string) =>
  fireEvent.change(await screen.findByLabelText('Password'), { target: { value } });
const enterCode = async (value = CODE) =>
  userEvent.setup().type(await screen.findByLabelText('Authenticator app code'), value);

const BEGIN_PATH = '*/api/v1/mfa/webauthn/verify-inline/begin';
const FINISH_PATH = '*/api/v1/mfa/webauthn/verify-inline/finish';
const WEBAUTHN_TOKEN = 'webauthn-inline-token';

const CREDENTIAL = {
  id: 'credential-id',
  rawId: new Uint8Array([1, 2, 3]).buffer,
  type: 'public-key',
  response: {
    authenticatorData: new Uint8Array([10, 20]).buffer,
    clientDataJSON: new Uint8Array([30, 40]).buffer,
    signature: new Uint8Array([50, 60]).buffer,
    userHandle: null,
  },
};

let mockGet: ReturnType<typeof vi.fn>;

beforeEach(() => {
  mockGet = vi.fn().mockResolvedValue(CREDENTIAL);
  Object.defineProperty(navigator, 'credentials', {
    value: { get: mockGet },
    writable: true,
    configurable: true,
  });
});

const KEY_COPY = 'Passkey or security key';

/** begin + finish stand-ins; `begins` records every begin body. */
function ceremonyWorks(begins: unknown[] = []) {
  server.use(
    http.post(BEGIN_PATH, async ({ request }) => {
      begins.push(await request.json());
      return HttpResponse.json({
        publicKey: { challenge: 'AQID', rpId: 'localhost', allowCredentials: [] },
      });
    }),
    http.post(FINISH_PATH, () => HttpResponse.json({ mfa_token: WEBAUTHN_TOKEN }))
  );
}

describe('DMThreadRemovalDialog security-key confirmation (#3509 review)', () => {
  it('offers the security key when the read names webauthn', async () => {
    server.use(readAnswers(['webauthn']));
    await openClear();

    expect(await screen.findByText(KEY_COPY)).toBeInTheDocument();
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
  });

  it('begins the security-key ceremony for dm.clear, the one route that can spend it', async () => {
    // Mutant: a ceremony begun for another purpose (it would mint a token Clear refuses).
    server.use(readAnswers(['webauthn']));
    const begins: unknown[] = [];
    server.use(
      http.post(BEGIN_PATH, async ({ request }) => {
        begins.push(await request.json());
        return HttpResponse.json({ error: 'stop here' }, { status: 400 });
      })
    );
    await openClear();

    fireEvent.click(verify());

    await vi.waitFor(() => expect(begins).toEqual([{ purpose: 'dm.clear' }]));
  });

  it('sends the security-key token as the first Clear request mfa_code', async () => {
    const begins: unknown[] = [];
    const bodies: unknown[] = [];
    server.use(
      readAnswers(['webauthn']),
      http.post(CLEAR_PATH, async ({ request }) => {
        bodies.push(await request.json());
        return cleared();
      })
    );
    ceremonyWorks(begins);
    const onClose = await openClear();

    fireEvent.click(verify());

    await waitFor(() => expect(onClose).toHaveBeenCalledOnce());
    expect(begins).toEqual([{ purpose: 'dm.clear' }]);
    expect(bodies).toEqual([{ mfa_code: WEBAUTHN_TOKEN }]);
    expect(mockGet).toHaveBeenCalledOnce();
  });

  it('a cancelled ceremony sends no Clear and returns focus to the primary', async () => {
    const bodies: unknown[] = [];
    server.use(
      readAnswers(['webauthn']),
      http.post(CLEAR_PATH, async ({ request }) => {
        bodies.push(await request.json());
        return cleared();
      })
    );
    ceremonyWorks();
    mockGet.mockRejectedValue(new DOMException('cancelled', 'NotAllowedError'));
    await openClear();

    fireEvent.click(verify());

    expect(await screen.findByRole('status')).toHaveTextContent(
      'Passkey or security key request was cancelled or timed out. Try again.'
    );
    await waitFor(() => expect(verify()).toHaveFocus());
    expect(bodies).toEqual([]);
  });

  it("offers the security key when Clear's own refusal names webauthn", async () => {
    server.use(
      readAnswers(['totp']),
      http.post(CLEAR_PATH, () =>
        HttpResponse.json(
          { error: 'MFA verification required', mfa_required: true, methods: ['webauthn'] },
          { status: 403 }
        )
      )
    );
    await openClear();
    await enterCode();
    fireEvent.click(verify());

    expect(await screen.findByText(KEY_COPY)).toBeInTheDocument();
    expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
    await waitFor(() => expect(verify()).toHaveFocus());
  });

  it('offers the security key when the password mint names webauthn', async () => {
    server.use(
      readAnswers([]),
      http.post(`*${MINT_PATH}`, () =>
        HttpResponse.json(
          { error: 'MFA verification required', mfa_required: true, mfa_methods: ['webauthn'] },
          { status: 403 }
        )
      )
    );
    await openClear();
    fireEvent.change(await screen.findByLabelText('Password'), { target: { value: FIXTURE_PW } });
    fireEvent.click(verify());

    expect(await screen.findByText(KEY_COPY)).toBeInTheDocument();
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
  });

  // Control: the harness reaches the MFA stage, and an authenticator-app
  // account is offered a code, not a security key.
  it('offers a code and no security key when only totp is named', async () => {
    server.use(readAnswers(['totp']));
    await openClear();

    expect(await screen.findByLabelText('Authenticator app code')).toBeInTheDocument();
    expect(screen.queryByText(KEY_COPY)).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /use passkey or security key/i })).toBeNull();
  });

  it('lets an account with both switch between them', async () => {
    server.use(readAnswers(['webauthn', 'totp']));
    await openClear();
    expect(await screen.findByText(KEY_COPY)).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'Use authenticator app instead' }));

    expect(await screen.findByLabelText('Authenticator app code')).toBeInTheDocument();
  });
});
