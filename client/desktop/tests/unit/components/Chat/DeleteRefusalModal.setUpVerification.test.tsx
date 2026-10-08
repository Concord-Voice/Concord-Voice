import React, { useState } from 'react';
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { render, screen, userEvent, waitFor } from '../../../test-utils';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import DeleteRefusalModal from '@/renderer/components/Chat/DeleteRefusalModal';
import type { DeleteRefusalState } from '@/renderer/hooks/messaging/useChatController';
import {
  captureApiRequestContext,
  type ApiRequestContext,
} from '@/renderer/services/system/requestContext';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import {
  toDeleteRefusalView,
  type DeleteRefusalView,
} from '@/renderer/services/messaging/deleteRefusal';
import { useSettingsNavStore } from '@/renderer/stores/ui/settingsNavStore';
import { useSettingsOverlayStore } from '@/renderer/stores/ui/settingsOverlayStore';

// #3456 §3.6a: the single-message delete soft-lock's enrolment state offers
// "Set up verification". It abandons the delete (the host closes, nothing is
// retried) and opens App Settings at the MFA section with a "Back to chat"
// return recorded.

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

const SET_UP = { name: 'Set up verification' };

function slot(
  view: DeleteRefusalView,
  context: ApiRequestContext = captureApiRequestContext()
): DeleteRefusalState {
  return { messageId: 'm1', view, openedAt: Date.now(), context };
}

/** The soft-lock's real enrolment refusal: `delete_rate_limited` rides along and a Retry-After is sent. */
function softLockEnrollView(): DeleteRefusalView {
  return toDeleteRefusalView(
    403,
    {
      error: 'Set up an authenticator app or security key to do this.',
      delete_rate_limited: true,
      mfa_enrollment_required: true,
    },
    '60'
  );
}

function readAnswers(methods: string[]) {
  return http.get('*/api/v1/mfa/step-up', () =>
    HttpResponse.json({
      methods,
      default_method: methods[0] ?? null,
      backup_code_available: false,
    })
  );
}

/** Mounts the modal in a host that really unmounts it on dismissal, as the chat controller does. */
function renderHost(initial: DeleteRefusalView, context?: ApiRequestContext) {
  const onConfirm = vi.fn(async () => ({ kind: 'success' }) as const);
  const onDismiss = vi.fn();
  const Host: React.FC = () => {
    const [refusal, setRefusal] = useState<DeleteRefusalState | null>(() => slot(initial, context));
    return (
      <DeleteRefusalModal
        refusal={refusal}
        onConfirm={onConfirm}
        onDismiss={() => {
          onDismiss();
          setRefusal(null);
        }}
        purpose="messages.delete"
        surfaceId="surface-main"
      />
    );
  };
  render(<Host />);
  return { onConfirm, onDismiss };
}

describe('DeleteRefusalModal set up verification (#3456 §3.6a)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    resetAllStores();
    server.use(readAnswers(['totp']));
  });

  // Mutation: drop `onSetUpVerification` from the <StepUpCredentials> props -> red.
  it('soft-lock enrolment refusal (delete_rate_limited + mfa_enrollment_required): shows the enrolment state with the link', async () => {
    const view = softLockEnrollView();
    expect(view).toEqual({ view: 'enroll' });
    renderHost(view);

    expect(await screen.findByText(/Set up an authenticator app or security key/)).toBeVisible();
    expect(await screen.findByRole('button', SET_UP)).toBeVisible();
    expect(screen.queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
  });

  // Mutation: drop `capture={capture}` from the stage's <StepUpCredentials> -> the link checks the
  // factor's own capture, which is current, and opens Settings for whoever is signed in now (red).
  it("the link does nothing once the account is not the refused delete's", async () => {
    const refusedAs = captureApiRequestContext();
    useAuthStore.setState((s) => ({ authGeneration: s.authGeneration + 1 }));
    const { onDismiss } = renderHost(softLockEnrollView(), refusedAs);

    await userEvent.setup().click(await screen.findByRole('button', SET_UP));

    expect(await screen.findByText('Sign in again to continue.')).toBeInTheDocument();
    expect(useSettingsOverlayStore.getState().open).toBeNull();
    expect(useSettingsOverlayStore.getState().verificationReturn).toBeNull();
    expect(onDismiss).not.toHaveBeenCalled();
  });

  // Mutation: change `returnTo: { kind: 'chat' }` to a serverSettings return -> red on verificationReturn.
  // Mutation: drop `closeHost: onCancel` -> red on onDismiss / dialog still present.
  // Mutation: change the section id / drop the call to openVerificationSetup -> red on open / focusRequest.
  it('the link closes the modal, opens App Settings at the MFA section and records a chat return', async () => {
    const { onConfirm, onDismiss } = renderHost(softLockEnrollView());

    await userEvent.setup().click(await screen.findByRole('button', SET_UP));

    await waitFor(() => expect(useSettingsOverlayStore.getState().open).toBe('app'));
    expect(onDismiss).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(useSettingsOverlayStore.getState().verificationReturn).toEqual({ kind: 'chat' });
    expect(useSettingsNavStore.getState().focusRequest).toEqual({
      section: 'privacy',
      controlId: 'section-mfa',
    });
    // Mutation: have the link call onConfirm (retry the delete) -> red here.
    expect(onConfirm).not.toHaveBeenCalled();
  });

  // Mutation: add an unconditional "Set up verification" button to CredentialStage -> red on confirm and password.
  it('confirm: no link, since the account can already verify', async () => {
    renderHost({ view: 'confirm', methods: ['totp'] });

    await screen.findByLabelText('Authenticator app code');
    expect(screen.queryByRole('button', SET_UP)).not.toBeInTheDocument();
  });

  it('password: no link, since a password is accepted', async () => {
    server.use(readAnswers([]));
    renderHost({ view: 'password' });

    await screen.findByLabelText('Password');
    expect(screen.queryByRole('button', SET_UP)).not.toBeInTheDocument();
  });

  // Mutation: add a "Set up verification" button to the Close-only branch -> red on all three.
  it.each<[string, DeleteRefusalView]>([
    ['wait', { view: 'wait', reason: 'requests', retryAfterSeconds: 30 }],
    ['unavailable', { view: 'unavailable' }],
    ['failed', { view: 'failed', message: 'Something went wrong.' }],
  ])('%s: Close-only view has no link', async (_name, view) => {
    renderHost(view);

    await screen.findAllByRole('button', { name: 'Close' });
    expect(screen.queryByRole('button', SET_UP)).not.toBeInTheDocument();
  });
});
