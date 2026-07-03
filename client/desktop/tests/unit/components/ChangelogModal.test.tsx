import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import ChangelogModalHost, {
  ChangelogModal,
} from '../../../src/renderer/components/ChangelogModal/ChangelogModal';
import { useChangelogStore } from '../../../src/renderer/stores/changelogStore';
import { useAuthStore } from '../../../src/renderer/stores/authStore';
import { useAttestationFailureStore } from '../../../src/renderer/stores/attestationFailureStore';
import { useClientConfigStore } from '../../../src/renderer/stores/clientConfigStore';
import { resetAllStores } from '../../helpers/store-helpers';
import type { ChangelogSection } from '../../../src/renderer/services/changelog';

// jsdom has no <dialog>.showModal — spy it like SubscriptionResetModal.test.tsx does.
beforeEach(() => {
  resetAllStores();
  vi.restoreAllMocks();
  vi.spyOn(HTMLDialogElement.prototype, 'showModal').mockImplementation(function (
    this: HTMLDialogElement
  ) {
    this.setAttribute('open', '');
  });
});

const section = (version: string, body: string): ChangelogSection => ({
  version,
  label: version,
  date: '2026-07-01',
  body,
});

describe('ChangelogModal (presentational)', () => {
  it('renders an aria-modal dialog with labelled title, describedby summary, and initial focus on the close button', () => {
    render(
      <ChangelogModal
        currentVersion="0.2.21"
        sections={[section('0.2.21', 'body text')]}
        onDismiss={() => {}}
      />
    );
    const dialog = screen.getByRole('dialog');
    expect(dialog).toHaveAttribute('aria-modal', 'true');
    expect(dialog).toHaveAttribute('aria-labelledby', 'changelog-modal-title');
    expect(dialog).toHaveAttribute('aria-describedby', 'changelog-modal-summary');
    expect(screen.getByText(/What's new in v0\.2\.21/)).toBeInTheDocument();
    expect(document.activeElement).toBe(screen.getByRole('button', { name: /got it/i }));
  });

  it('renders newest 3 sections plus an "earlier versions" line', () => {
    const sections = ['0.2.21', '0.2.20', '0.2.19', '0.2.18', '0.2.17'].map((v) =>
      section(v, `notes for ${v}`)
    );
    render(<ChangelogModal currentVersion="0.2.21" sections={sections} onDismiss={() => {}} />);
    expect(screen.getByText('notes for 0.2.21')).toBeInTheDocument();
    expect(screen.getByText('notes for 0.2.19')).toBeInTheDocument();
    expect(screen.queryByText('notes for 0.2.18')).not.toBeInTheDocument();
    expect(screen.getByText(/and 2 earlier versions/)).toBeInTheDocument();
  });

  it('exposes the notes as a named region with keyboard-focusable link content', () => {
    render(
      <ChangelogModal
        currentVersion="0.2.21"
        sections={[
          section(
            '0.2.21',
            'See [#2000](https://github.com/Concord-Voice/Concord-Voice-Alpha/pull/2000).'
          ),
        ]}
        onDismiss={() => {}}
      />
    );
    // <section aria-label> carries the implicit region role (Sonar S6819/S6845 —
    // no role/tabIndex props); keyboard scrolling rides the focusable links.
    const region = screen.getByRole('region', { name: /release notes/i });
    expect(region.tagName).toBe('SECTION');
    expect(region.querySelector('a')).not.toBeNull();
  });

  it('shows compact fallback copy when sections are empty', () => {
    render(<ChangelogModal currentVersion="0.2.21" sections={[]} onDismiss={() => {}} />);
    expect(screen.getByText(/updated to v0\.2\.21/i)).toBeInTheDocument();
  });

  it('does NOT render a MentionChip for mention-looking changelog text', () => {
    render(
      <ChangelogModal
        currentVersion="0.2.21"
        sections={[section('0.2.21', '@mention notification routing improvements')]}
        onDismiss={() => {}}
      />
    );
    expect(document.querySelector('.mention-chip')).toBeNull();
    expect(screen.getByText(/@mention notification routing/)).toBeInTheDocument();
  });

  it('calls onDismiss when the close button is clicked', () => {
    const onDismiss = vi.fn();
    render(<ChangelogModal currentVersion="0.2.21" sections={[]} onDismiss={onDismiss} />);
    fireEvent.click(screen.getByRole('button', { name: /got it/i }));
    expect(onDismiss).toHaveBeenCalled();
  });

  it('bridges native dialog close (Escape) to onDismiss', () => {
    const onDismiss = vi.fn();
    render(<ChangelogModal currentVersion="0.2.21" sections={[]} onDismiss={onDismiss} />);
    fireEvent(screen.getByRole('dialog'), new Event('close'));
    expect(onDismiss).toHaveBeenCalled();
  });
});

describe('ChangelogModalHost (decision table + suppression)', () => {
  const authenticate = () => {
    useAuthStore.getState().setAccessToken('mock-token');
    useAuthStore.setState({ emailVerified: true });
  };
  const stubVersion = (v: string) => {
    (globalThis as { electron?: Record<string, unknown> }).electron = {
      ...(globalThis as { electron?: Record<string, unknown> }).electron,
      getVersion: vi.fn().mockResolvedValue(v),
    };
  };

  it('fresh install (null lastSeen): records silently, shows nothing', async () => {
    authenticate();
    stubVersion('0.2.21');
    render(<ChangelogModalHost />);
    await waitFor(() => expect(useChangelogStore.getState().lastSeenVersion).toBe('0.2.21'));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(screen.getByTestId('changelog-host-idle')).toBeInTheDocument();
  });

  it('upgrade: shows the modal and dismissal persists the seen version', async () => {
    authenticate();
    stubVersion('0.2.21');
    useChangelogStore.getState().markSeen('0.2.20');
    render(<ChangelogModalHost />);
    const dialog = await screen.findByRole('dialog');
    expect(dialog).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: /got it/i }));
    expect(useChangelogStore.getState().lastSeenVersion).toBe('0.2.21');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('equal versions: renders idle and leaves the store untouched', async () => {
    authenticate();
    stubVersion('0.2.21');
    useChangelogStore.getState().markSeen('0.2.21');
    render(<ChangelogModalHost />);
    await waitFor(() => expect(screen.getByTestId('changelog-host-idle')).toBeInTheDocument());
    expect(useChangelogStore.getState().lastSeenVersion).toBe('0.2.21');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('downgrade: records silently, shows nothing', async () => {
    authenticate();
    stubVersion('0.2.20');
    useChangelogStore.getState().markSeen('0.2.21');
    render(<ChangelogModalHost />);
    await waitFor(() => expect(useChangelogStore.getState().lastSeenVersion).toBe('0.2.20'));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('suppressed pre-auth (login screen): renders nothing, records nothing', async () => {
    stubVersion('0.2.21');
    useChangelogStore.getState().markSeen('0.2.20');
    const { container } = render(<ChangelogModalHost />);
    await waitFor(() => expect(container).toBeEmptyDOMElement());
    expect(useChangelogStore.getState().lastSeenVersion).toBe('0.2.20');
  });

  it('suppressed while attestation failure is visible', async () => {
    authenticate();
    stubVersion('0.2.21');
    useChangelogStore.getState().markSeen('0.2.20');
    useAttestationFailureStore.setState({ visible: true, code: 'ATTESTATION_REVOKED' });
    const { container } = render(<ChangelogModalHost />);
    await waitFor(() => expect(container).toBeEmptyDOMElement());
  });

  it('suppressed while force-update is required (current < minVersion)', async () => {
    authenticate();
    stubVersion('0.2.21');
    useChangelogStore.getState().markSeen('0.2.20');
    useClientConfigStore.setState({ minVersion: '0.3.0', lastFetchedAt: Date.now() });
    const { container } = render(<ChangelogModalHost />);
    await waitFor(() => expect(container).toBeEmptyDOMElement());
  });
});
