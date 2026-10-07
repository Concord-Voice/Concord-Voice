import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll } from 'vitest';
import { http, HttpResponse } from 'msw';
import { render, screen, userEvent } from '../../../test-utils';
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
    // The channel/server soft-lock keeps its own prompt: it must never start
    // the DM step-up stage's requirements read (GET /api/v1/mfa/step-up).
    let reads = 0;
    server.use(
      http.get('*/api/v1/mfa/step-up', () => {
        reads += 1;
        return HttpResponse.json({
          methods: ['totp'],
          default_method: 'totp',
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
    // Submitted from the field itself, so the focused element is the one that
    // unmounts when the challenge changes.
    await user.type(await screen.findByLabelText('Password'), `${FIXTURE_PW}{Enter}`);

    await screen.findByRole('button', { name: 'Verify with security key' });
    expect(document.activeElement).not.toBe(document.body);
    expect(screen.getByRole('dialog').contains(document.activeElement)).toBe(true);
    expect(reads).toBe(0);
  });
});
