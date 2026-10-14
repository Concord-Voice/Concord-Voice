import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen } from '../../../test-utils';
import CollapsibleSection from '@/renderer/components/Settings/CollapsibleSection';
import { useSettingsOverlayStore } from '@/renderer/stores/ui/settingsOverlayStore';
import { useSettingsCollapsibleStore } from '@/renderer/stores/ui/settingsCollapsibleStore';
import { useDraftSettingsStore } from '@/renderer/stores/ui/draftSettingsStore';
import { resetAllStores } from '../../../helpers/store-helpers';

const THIRTY_MINUTES = 30 * 60 * 1000;
const START = new Date('2026-10-13T12:00:00Z');

// Render the actual disclosure consumer under the actual overlay state, so a store
// action that records a deadline but does not spend it cannot satisfy this suite.
function OverlayDisclosures() {
  const open = useSettingsOverlayStore((state) => state.open);
  return open === 'app' ? (
    <>
      <CollapsibleSection id="section-theme" title="Theme">
        Theme choices
      </CollapsibleSection>
      <CollapsibleSection id="client-behavior" title="Client Behavior">
        Client choices
      </CollapsibleSection>
    </>
  ) : null;
}

function themeDisclosure(): HTMLDetailsElement {
  const details = screen.getByText('Theme', { selector: 'h2' }).closest('details');
  if (!(details instanceof HTMLDetailsElement)) throw new Error('Theme disclosure is missing');
  return details;
}

function openSettings() {
  act(() => useSettingsOverlayStore.getState().openSettings('app'));
}

function closeSettings() {
  act(() => useSettingsOverlayStore.getState().close());
  expect(screen.queryByText('Theme choices')).not.toBeInTheDocument();
}

function expandTheme() {
  const details = themeDisclosure();
  const summary = details.querySelector('summary');
  if (!summary) throw new Error('Theme summary is missing');
  fireEvent.click(summary);
  // jsdom queues its native toggle. Dispatch the same event synchronously so
  // advancing the retention clock never also becomes a gate for that user action.
  fireEvent(details, new Event('toggle'));
  expect(details).toHaveAttribute('open');
  expect(useSettingsCollapsibleStore.getState().openSections['section-theme']).toBe(true);
}

function advance(ms: number) {
  act(() => vi.advanceTimersByTime(ms));
}

describe('Settings expansion retained for 30 minutes after leaving', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(START);
    resetAllStores();
    render(<OverlayDisclosures />);
  });

  afterEach(() => {
    vi.clearAllTimers();
    vi.useRealTimers();
  });

  it('opens a fresh visit with every disclosure collapsed', () => {
    openSettings();
    expect(screen.getByText('Theme choices')).toBeInTheDocument();
    expect(screen.getByText('Client choices')).toBeInTheDocument();
    expect(document.querySelectorAll('details[open]')).toHaveLength(0);
  });

  it('preserves expansion on return after 29 minutes and 59 seconds', () => {
    openSettings();
    expandTheme();
    closeSettings();
    advance(THIRTY_MINUTES - 1000);
    openSettings();

    expect(themeDisclosure()).toHaveAttribute('open');
    expect(document.getElementById('client-behavior')).not.toHaveAttribute('open');
  });

  it('returns to all-collapsed at exactly 30 minutes after close', () => {
    openSettings();
    expandTheme();
    closeSettings();
    advance(THIRTY_MINUTES);
    openSettings();

    expect(themeDisclosure()).not.toHaveAttribute('open');
    expect(document.querySelectorAll('details[open]')).toHaveLength(0);
  });

  it('purges remembered expansion while Settings stays closed at the deadline', () => {
    openSettings();
    expandTheme();
    closeSettings();
    advance(THIRTY_MINUTES);

    expect(useSettingsCollapsibleStore.getState().openSections).toEqual({});
    expect(useSettingsOverlayStore.getState().open).toBeNull();
  });

  it('checks the absolute deadline on return after sleep delays the timeout', () => {
    openSettings();
    expandTheme();
    closeSettings();
    // Jump the wall clock without running timers, as a suspended renderer can do.
    vi.setSystemTime(START.getTime() + THIRTY_MINUTES + 1);
    openSettings();

    expect(themeDisclosure()).not.toHaveAttribute('open');
    expect(useSettingsCollapsibleStore.getState().openSections).toEqual({});
  });

  it('keeps expansion throughout an active visit lasting over 30 minutes', () => {
    openSettings();
    expandTheme();
    advance(THIRTY_MINUTES * 2);

    expect(themeDisclosure()).toHaveAttribute('open');
    closeSettings();
    advance(THIRTY_MINUTES - 1000);
    openSettings();
    expect(themeDisclosure()).toHaveAttribute('open');
  });

  it('cancels an earlier close timer when the user returns before expiry', () => {
    openSettings();
    expandTheme();
    closeSettings();
    advance(5 * 60 * 1000);
    openSettings();
    advance(THIRTY_MINUTES);

    expect(themeDisclosure()).toHaveAttribute('open');
    closeSettings();
    advance(THIRTY_MINUTES);
    openSettings();
    expect(themeDisclosure()).not.toHaveAttribute('open');
  });

  it('does not restart the close deadline on repeated close requests', () => {
    openSettings();
    expandTheme();
    closeSettings();
    advance(10 * 60 * 1000);
    closeSettings();
    advance(20 * 60 * 1000);
    openSettings();

    expect(themeDisclosure()).not.toHaveAttribute('open');
  });

  it('treats an app-to-server Settings transition as leaving app Settings', () => {
    openSettings();
    expandTheme();
    act(() => useSettingsOverlayStore.getState().openSettings('server', { serverId: 'server-1' }));
    expect(screen.queryByText('Theme choices')).not.toBeInTheDocument();
    advance(THIRTY_MINUTES);
    openSettings();

    expect(themeDisclosure()).not.toHaveAttribute('open');
  });

  it('does not expire or replace memory when app Settings is already open', () => {
    openSettings();
    expandTheme();
    advance(THIRTY_MINUTES);
    openSettings();
    advance(THIRTY_MINUTES);

    expect(themeDisclosure()).toHaveAttribute('open');
  });

  it('keeps the visit active when screen-capture protection blocks closing', () => {
    openSettings();
    expandTheme();
    act(() => useDraftSettingsStore.setState({ contentProtectionApplying: true }));
    act(() => useSettingsOverlayStore.getState().close());
    expect(useSettingsOverlayStore.getState().open).toBe('app');
    advance(THIRTY_MINUTES);
    expect(themeDisclosure()).toHaveAttribute('open');

    act(() => useDraftSettingsStore.setState({ contentProtectionApplying: false }));
    closeSettings();
    advance(THIRTY_MINUTES);
    openSettings();
    expect(themeDisclosure()).not.toHaveAttribute('open');
  });
});
