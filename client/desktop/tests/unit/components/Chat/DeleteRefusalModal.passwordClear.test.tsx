import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { render, screen, userEvent, waitFor } from '../../../test-utils';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import DeleteRefusalModal from '@/renderer/components/Chat/DeleteRefusalModal';
import type { DeleteRefusalState } from '@/renderer/hooks/messaging/useChatController';
import { captureApiRequestContext } from '@/renderer/services/system/requestContext';
import type { DeleteRefusalView } from '@/renderer/services/messaging/deleteRefusal';

// Reproduction from the #3509 frontend review (L1): once the password has
// been sent, nothing may keep it in component state. A delete that went
// password → wait → password (same attempt key) showed the old password
// still in the field. Picker PR 3: the sent password now leaves the field the
// moment the attempt settles, and a view that hosts no credential unmounts the
// stage that owned it.

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

const FIXTURE_PW = 'hunter2-fixture';

function slot(view: DeleteRefusalView): DeleteRefusalState {
  return { messageId: 'm1', view, openedAt: 1, context: captureApiRequestContext() };
}

const OTHER_VIEWS = [
  { view: 'wait', reason: 'requests' } as const,
  { view: 'unavailable' } as const,
  { view: 'failed' } as const,
];

beforeEach(() => {
  resetAllStores();
  server.use(
    http.get('*/api/v1/mfa/step-up', () =>
      HttpResponse.json({ methods: [], default_method: null, backup_code_available: false })
    )
  );
});

describe('DeleteRefusalModal password hygiene (#3509 frontend review)', () => {
  it('L1: the sent password is gone when the password view returns after another view', async () => {
    const user = userEvent.setup();
    const onConfirm = vi.fn(async () => ({ kind: 'answered' }) as const);
    const props = {
      onConfirm,
      onDismiss: vi.fn(),
      purpose: 'messages.delete' as const,
      surfaceId: 's1',
    };
    const { rerender } = render(
      <DeleteRefusalModal refusal={slot({ view: 'password' })} {...props} />
    );

    await user.type(await screen.findByLabelText('Password'), FIXTURE_PW);
    await user.click(screen.getByRole('button', { name: /confirm/i }));
    await waitFor(() => expect(onConfirm).toHaveBeenCalledTimes(1));
    expect(onConfirm.mock.calls[0]).toEqual([{ currentPassword: FIXTURE_PW }, expect.anything()]);

    for (const view of OTHER_VIEWS) {
      rerender(<DeleteRefusalModal refusal={slot(view)} {...props} />);
      rerender(<DeleteRefusalModal refusal={slot({ view: 'password' })} {...props} />);
      expect(await screen.findByLabelText('Password')).toHaveValue('');
    }
  });

  it('a password typed but never sent does not survive a view that hosts no credential', async () => {
    const user = userEvent.setup();
    const props = {
      onConfirm: vi.fn(),
      onDismiss: vi.fn(),
      purpose: 'messages.delete' as const,
      surfaceId: 's1',
    };
    const { rerender } = render(
      <DeleteRefusalModal refusal={slot({ view: 'password' })} {...props} />
    );
    await user.type(await screen.findByLabelText('Password'), FIXTURE_PW);

    for (const view of OTHER_VIEWS) {
      rerender(<DeleteRefusalModal refusal={slot(view)} {...props} />);
      expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
      rerender(<DeleteRefusalModal refusal={slot({ view: 'password' })} {...props} />);
      expect(await screen.findByLabelText('Password')).toHaveValue('');
    }
    expect(props.onConfirm).not.toHaveBeenCalled();
  });
});
