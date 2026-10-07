import { describe, expect, it, vi } from 'vitest';
import { render, screen, userEvent } from '../../../test-utils';
import DeleteRefusalModal from '@/renderer/components/Chat/DeleteRefusalModal';
import type { DeleteRefusalState } from '@/renderer/hooks/messaging/useChatController';
import type { DeleteRefusalView } from '@/renderer/services/messaging/deleteRefusal';

// Reproduction from the #3509 frontend review (L1): once the password has
// been sent, nothing may keep it in component state. A delete that went
// password → wait → password (same attempt key) showed the old password
// still in the field.

vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: vi.fn(),
  API_BASE: 'http://localhost:8080',
}));

const FIXTURE_PW = 'hunter2-fixture';

function slot(view: DeleteRefusalView, submitting = false): DeleteRefusalState {
  return { messageId: 'm1', view, submitting, openedAt: 1, promptKey: 0 };
}

describe('DeleteRefusalModal password hygiene (#3509 frontend review)', () => {
  it('L1: the sent password is gone when the password view returns after another view', async () => {
    const user = userEvent.setup();
    const onConfirm = vi.fn();
    const props = { onConfirm, onDismiss: vi.fn(), purpose: 'messages.delete' as const };
    const { rerender } = render(
      <DeleteRefusalModal surfaceId="s1" refusal={slot({ view: 'password' })} {...props} />
    );

    await user.type(screen.getByLabelText('Password'), FIXTURE_PW);
    await user.click(screen.getByRole('button', { name: /confirm/i }));
    expect(onConfirm).toHaveBeenCalledWith({ currentPassword: FIXTURE_PW });

    for (const view of [
      { view: 'wait', reason: 'requests' } as const,
      { view: 'unavailable' } as const,
      { view: 'failed' } as const,
    ]) {
      rerender(<DeleteRefusalModal surfaceId="s1" refusal={slot(view)} {...props} />);
      rerender(
        <DeleteRefusalModal surfaceId="s1" refusal={slot({ view: 'password' })} {...props} />
      );
      expect(screen.getByLabelText('Password')).toHaveValue('');
    }
  });
});
