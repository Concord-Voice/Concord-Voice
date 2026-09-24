// client/desktop/tests/unit/components/Voice/MediaPolicyDialog.test.tsx
import React from 'react';
import { describe, it, expect, beforeEach } from 'vitest';
import { render, screen, fireEvent, act } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import MediaPolicyDialog from '@/renderer/components/Voice/MediaPolicyDialog';
import { formatRejoinTime, roundUpToMinute } from '@/renderer/services/voice/mediaPolicyEvents';

const AT = Date.UTC(2026, 8, 23, 15, 44, 1);
const closeButton = () =>
  document.querySelector<HTMLButtonElement>('button.media-policy-dialog__close');

describe('MediaPolicyDialog (#2153 handoff §1c/§1d)', () => {
  beforeEach(() => resetAllStores());

  it('renders nothing without an interrupt', () => {
    render(<MediaPolicyDialog />);
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('eviction: title, body and a machine-readable rounded time', () => {
    useVoiceStore.setState({ mediaPolicyInterrupt: { reason: 'evicted', rejoinAt: AT } });
    render(<MediaPolicyDialog />);
    expect(screen.getByRole('dialog', { name: 'Removed from voice' })).toBeInTheDocument();
    expect(
      screen.getByText(
        /Your connection kept sending more data than this call allows, so you were removed\./
      )
    ).toBeInTheDocument();
    const time = document.querySelector('time');
    expect(time).toHaveTextContent(formatRejoinTime(AT));
    expect(time).toHaveAttribute('dateTime', new Date(roundUpToMinute(AT)).toISOString());
  });

  it('cooldown: its own title and body', () => {
    useVoiceStore.setState({ mediaPolicyInterrupt: { reason: 'cooldown', rejoinAt: AT } });
    render(<MediaPolicyDialog />);
    expect(screen.getByRole('dialog', { name: 'You can’t join voice yet' })).toBeInTheDocument();
    expect(screen.getByText(/^Try again after/)).toBeInTheDocument();
  });

  it.each([
    ['evicted', 'You can join voice again later.'],
    ['cooldown', 'Try again later.'],
  ] as const)('%s without a time says "later" and renders no <time>', (reason, sentence) => {
    useVoiceStore.setState({ mediaPolicyInterrupt: { reason, rejoinAt: null } });
    render(<MediaPolicyDialog />);
    expect(screen.getByText(new RegExp(sentence.replace('.', '\\.')))).toBeInTheDocument();
    expect(document.querySelector('time')).toBeNull();
  });

  it('moves focus to Close', () => {
    useVoiceStore.setState({ mediaPolicyInterrupt: { reason: 'evicted', rejoinAt: AT } });
    render(<MediaPolicyDialog />);
    expect(document.activeElement).toBe(closeButton());
  });

  it('Close and Escape both dismiss the interrupt', () => {
    useVoiceStore.setState({ mediaPolicyInterrupt: { reason: 'evicted', rejoinAt: AT } });
    const { unmount } = render(<MediaPolicyDialog />);
    fireEvent.click(closeButton() as HTMLButtonElement);
    expect(useVoiceStore.getState().mediaPolicyInterrupt).toBeNull();
    unmount();

    useVoiceStore.setState({ mediaPolicyInterrupt: { reason: 'cooldown', rejoinAt: AT } });
    render(<MediaPolicyDialog />);
    act(() => {
      fireEvent.keyDown(document, { key: 'Escape' });
    });
    expect(useVoiceStore.getState().mediaPolicyInterrupt).toBeNull();
  });

  it('hides the glyph from assistive technology', () => {
    useVoiceStore.setState({ mediaPolicyInterrupt: { reason: 'evicted', rejoinAt: AT } });
    render(<MediaPolicyDialog />);
    expect(document.querySelector('.media-policy-dialog__glyph')).toHaveAttribute(
      'aria-hidden',
      'true'
    );
  });

  // F17: the explanation was not announced when the dialog opened — Modal set only
  // aria-labelledby, never aria-describedby.
  it('aria-describedby on the dialog references the body explanation', () => {
    useVoiceStore.setState({ mediaPolicyInterrupt: { reason: 'evicted', rejoinAt: AT } });
    render(<MediaPolicyDialog />);
    const dialog = screen.getByRole('dialog', { name: 'Removed from voice' });
    const describedById = dialog.getAttribute('aria-describedby');
    expect(describedById).toBeTruthy();
    expect(document.getElementById(describedById as string)).toHaveTextContent(
      /Your connection kept sending more data than this call allows, so you were removed\./
    );
  });
});
