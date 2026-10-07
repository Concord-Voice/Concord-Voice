import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { render, screen, userEvent } from '../../../test-utils';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import PurgeMessagesModal from '@/renderer/components/Purge/PurgeMessagesModal';

// REPRODUCTION (H4, pre-existing): PurgeMessagesModal resets only on `isOpen`,
// but GroupInfoPanel mounts it permanently with `scopeId={conversation.id}`. A
// scope change while open must end the dialog for the old scope; today it
// stays open and the chosen range carries over to the new conversation.
// Expected to FAIL against the current tree on the `onClose` assertion.

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

beforeEach(() => {
  resetAllStores();
  server.use(
    http.get('*/api/v1/mfa/step-up', () =>
      HttpResponse.json({ methods: ['totp'], default_method: 'totp', backup_code_available: false })
    )
  );
});

describe('PurgeMessagesModal: scope change while open', () => {
  it('closes the dialog when scopeId changes, instead of carrying the chosen range to the next scope', async () => {
    const user = userEvent.setup();
    const onClose = vi.fn();
    const modal = (scopeId: string) => (
      <PurgeMessagesModal
        context="group"
        isOpen
        scopeId={scopeId}
        scopeName="Crew"
        onClose={onClose}
      />
    );

    const { rerender } = render(modal('g1'));
    await user.selectOptions(screen.getByRole('combobox', { name: 'Range' }), 'Last 7 days');
    expect(screen.getByRole('combobox', { name: 'Range' })).toHaveValue('7d');

    rerender(modal('g2'));

    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
