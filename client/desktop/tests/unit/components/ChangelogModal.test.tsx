import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import ChangelogModalHost, {
  ChangelogModal,
  MAX_RENDERED_SECTIONS,
} from '../../../src/renderer/components/ChangelogModal/ChangelogModal';
import { useChangelogStore } from '../../../src/renderer/stores/ui/changelogStore';
import { useAuthStore } from '../../../src/renderer/stores/auth/authStore';
import { useAttestationFailureStore } from '../../../src/renderer/stores/auth/attestationFailureStore';
import { useClientConfigStore } from '../../../src/renderer/stores/ui/clientConfigStore';
import { resetAllStores } from '../../helpers/store-helpers';
import type { ChangelogSection } from '../../../src/renderer/services/system/changelog';

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

const section = (version: string, body: string, preamble = ''): ChangelogSection => ({
  version,
  label: version,
  date: '2026-07-01',
  body,
  preamble,
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

  it('renders the newest MAX_RENDERED_SECTIONS plus an "earlier versions" line', () => {
    // Derived from the constant, not a literal, so a cadence-driven cap change
    // does not need a matching edit here (the cap moved 3 → 12 for exactly that).
    const overflow = 2;
    const sections = Array.from({ length: MAX_RENDERED_SECTIONS + overflow }, (_, i) =>
      section(`0.2.${100 - i}`, `notes for index ${i}`)
    );
    render(<ChangelogModal currentVersion="0.2.100" sections={sections} onDismiss={() => {}} />);

    expect(screen.getByText('notes for index 0')).toBeInTheDocument();
    expect(screen.getByText(`notes for index ${MAX_RENDERED_SECTIONS - 1}`)).toBeInTheDocument();
    expect(screen.queryByText(`notes for index ${MAX_RENDERED_SECTIONS}`)).not.toBeInTheDocument();
    expect(screen.getByText(new RegExp(`and ${overflow} earlier versions`))).toBeInTheDocument();
  });

  it('renders every section with no overflow line when the range fits the cap', () => {
    // The point of raising the cap: an ordinary two-week absence (~12 releases
    // at the measured 6.1/week) is shown in full rather than collapsed.
    const sections = Array.from({ length: MAX_RENDERED_SECTIONS }, (_, i) =>
      section(`0.2.${100 - i}`, `notes for index ${i}`)
    );
    render(<ChangelogModal currentVersion="0.2.100" sections={sections} onDismiss={() => {}} />);

    expect(screen.getByText(`notes for index ${MAX_RENDERED_SECTIONS - 1}`)).toBeInTheDocument();
    expect(screen.queryByText(/earlier version/)).not.toBeInTheDocument();
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

  it('keeps scrollable content reachable by keyboard (WCAG 2.1.1)', () => {
    // Overflow lives on the DIALOG, so the scroll container contains the focused
    // close button and arrow keys scroll it. A preamble carries no links, so
    // moving overflow onto the notes region would put the scroll container
    // outside the focus path — and making that region focusable would mean a
    // tabIndex on a non-interactive element. Vitest runs with css:false, so the
    // overflow itself is asserted by the stylesheet; this locks the DOM half.
    render(
      <ChangelogModal
        currentVersion="0.2.40"
        sections={[section('0.2.40', 'Detail.', 'Summary with no links at all.')]}
        onDismiss={() => {}}
      />
    );
    const notes = screen.getByRole('region', { name: /release notes/i });
    expect(notes).not.toHaveAttribute('tabindex');
    // The scroll container (the dialog) must own a focusable descendant.
    const dialog = screen.getByRole('dialog');
    expect(dialog).toContainElement(screen.getByRole('button', { name: /got it/i }));
  });

  it('renders the preamble and NOT the full detail body when a preamble exists', () => {
    render(
      <ChangelogModal
        currentVersion="0.2.40"
        sections={[section('0.2.40', 'Full STE detail bullet text.', 'Short brand summary.')]}
        onDismiss={() => {}}
      />
    );
    expect(screen.getByText('Short brand summary.')).toBeInTheDocument();
    expect(screen.queryByText('Full STE detail bullet text.')).not.toBeInTheDocument();
  });

  it('falls back to the body for entries predating the preamble convention', () => {
    render(
      <ChangelogModal
        currentVersion="0.2.21"
        sections={[section('0.2.21', 'Legacy body text.')]}
        onDismiss={() => {}}
      />
    );
    expect(screen.getByText('Legacy body text.')).toBeInTheDocument();
  });

  it('keeps a mixed range readable — preamble sections stay summarized alongside legacy ones', () => {
    // sectionsBetween() concatenates EVERY skipped version, so a user upgrading
    // across the convention boundary sees both shapes at once. The new sections
    // must not regress to full bodies just because an older one has none.
    render(
      <ChangelogModal
        currentVersion="0.2.41"
        sections={[
          section('0.2.41', 'Detail for 41.', 'Summary for 41.'),
          section('0.2.40', 'Detail for 40.', 'Summary for 40.'),
          section('0.2.39', 'Legacy detail for 39.'),
        ]}
        onDismiss={() => {}}
      />
    );
    expect(screen.getByText('Summary for 41.')).toBeInTheDocument();
    expect(screen.getByText('Summary for 40.')).toBeInTheDocument();
    expect(screen.getByText('Legacy detail for 39.')).toBeInTheDocument();
    expect(screen.queryByText('Detail for 41.')).not.toBeInTheDocument();
    expect(screen.queryByText('Detail for 40.')).not.toBeInTheDocument();
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
