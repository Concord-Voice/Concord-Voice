import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll } from 'vitest';
import { http, HttpResponse } from 'msw';
import { render, screen, userEvent, waitFor } from '../../../test-utils';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import { FIXTURE_PW, MINT_PATH } from '../../../helpers/stepUpTokenWire';
import PurgeMessagesModal from '@/renderer/components/Purge/PurgeMessagesModal';

// Codex on #3509 (P2): when a retry changes the soft-lock challenge from the
// password field to a security key, the focused field unmounts. Focus must
// stay inside the dialog rather than fall to the document body.

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());
beforeEach(() => resetAllStores());

describe('PurgeMessagesModal soft-lock factor change', () => {
  it('keeps focus in the dialog when the password stage becomes a security-key prompt', async () => {
    // The soft-lock stage reads the account's methods before it offers the
    // password field (an account with no inline method). Focus must stay in
    // the dialog throughout, including across that read and the mint's reply.
    let reads = 0;
    server.use(
      http.get('*/api/v1/mfa/step-up', () => {
        reads += 1;
        return HttpResponse.json({
          methods: [],
          default_method: null,
          backup_code_available: false,
        });
      }),
      http.delete('*/api/v1/channels/:id/messages', () =>
        HttpResponse.json(
          {
            error: 'Current password required to keep deleting messages',
            delete_rate_limited: true,
            password_required: true,
          },
          { status: 403, headers: { 'Retry-After': '30' } }
        )
      ),
      http.post(`*${MINT_PATH}`, () =>
        HttpResponse.json(
          { error: 'MFA verification required', mfa_required: true, mfa_methods: ['webauthn'] },
          { status: 403 }
        )
      )
    );
    const user = userEvent.setup();
    render(
      <PurgeMessagesModal
        context="channel"
        isOpen={true}
        scopeId="c1"
        scopeName="general"
        onClose={() => {}}
      />
    );
    await user.selectOptions(screen.getByRole('combobox', { name: 'Range' }), 'Last 7 days');
    await user.click(screen.getByRole('button', { name: 'Purge Messages' }));
    const field = await screen.findByLabelText('Password');
    await user.type(field, FIXTURE_PW);
    // The field is focused when the challenge changes, so it is the focused
    // element that unmounts.
    field.focus();
    const dialog = screen.getByRole('dialog');
    expect(dialog.contains(document.activeElement)).toBe(true);
    await user.click(screen.getByRole('button', { name: 'Confirm and Purge' }));

    // The mint named a security key: the password field gives way to the key's
    // panel (a single method, so its own primary rather than a picker).
    await waitFor(() => expect(screen.queryByLabelText('Password')).not.toBeInTheDocument());
    expect(await screen.findByText('Passkey or security key')).toBeInTheDocument();
    expect(document.activeElement).not.toBe(document.body);
    expect(screen.getByRole('dialog').contains(document.activeElement)).toBe(true);
    // The requirements read is allowed, and it happens (the field waited for it).
    expect(reads).toBeGreaterThan(0);
  });
});
