import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { ClientBehaviorSection } from '@/renderer/components/Settings/ClientBehaviorSection';
import { useSettingsStore } from '@/renderer/stores/settingsStore';

beforeEach(() => {
  vi.clearAllMocks();
  (window as unknown as { electron: unknown }).electron = {
    window: { setClientBehavior: vi.fn().mockResolvedValue(undefined) },
  };
  useSettingsStore.setState({
    clientBehavior: { toTray: 'close', toToolbar: 'minimize' },
  });
});

describe('ClientBehaviorSection', () => {
  it('renders both toggles and the explanation panel', () => {
    render(<ClientBehaviorSection />);
    expect(screen.getByText(/To Tray/)).toBeInTheDocument();
    expect(screen.getByText(/To Toolbar/)).toBeInTheDocument();
    expect(screen.getByText(/To Close Concord Voice/i)).toBeInTheDocument();
  });

  it('shows correct selected state for default config', () => {
    render(<ClientBehaviorSection />);
    const toTrayClose = screen.getByLabelText(/To Tray.*Close/i);
    expect(toTrayClose).toBeChecked();
  });

  it('grays out the mutex-conflicting option', () => {
    render(<ClientBehaviorSection />);
    // toToolbar is "minimize" by default; so "minimize" in toTray should be disabled
    const toTrayMinimize = screen.getByLabelText(/To Tray.*Minimize/i);
    expect(toTrayMinimize).toBeDisabled();
  });

  it('updates clientBehavior when user changes a toggle', () => {
    render(<ClientBehaviorSection />);
    const toTrayNone = screen.getByLabelText(/To Tray.*None/i);
    fireEvent.click(toTrayNone);
    expect(useSettingsStore.getState().clientBehavior.toTray).toBe('none');
  });

  // #1148: there is no "coverage" rule. Both derivers are total, so every button
  // always has a destination — these configs are valid and must be selectable.
  it('allows "close" in toToolbar when toTray is "none" (#1148 — both buttons → toolbar, quit via app-icon menu)', () => {
    useSettingsStore.setState({
      clientBehavior: { toTray: 'none', toToolbar: 'minimize' },
    });
    render(<ClientBehaviorSection />);
    const toToolbarClose = screen.getByLabelText(/To Toolbar.*Close/i);
    expect(toToolbarClose).not.toBeDisabled();
  });

  it('allows "none" on toTray when toToolbar is "close" (symmetric to #1148) with no coverage tooltip', () => {
    useSettingsStore.setState({
      clientBehavior: { toTray: 'minimize', toToolbar: 'close' },
    });
    render(<ClientBehaviorSection />);
    const toTrayNone = screen.getByLabelText(/To Tray.*None/i);
    expect(toTrayNone).not.toBeDisabled();
    // The removed coverage rule no longer attaches a "no destination" tooltip.
    const label = toTrayNone.closest('label');
    expect(label?.getAttribute('title') ?? '').not.toMatch(/Would leave the \[—\] button/);
  });

  it('updates clientBehavior when user changes toToolbar', () => {
    render(<ClientBehaviorSection />);
    const toToolbarClose = screen.getByLabelText(/To Toolbar.*Close/i);
    fireEvent.click(toToolbarClose);
    expect(useSettingsStore.getState().clientBehavior.toToolbar).toBe('close');
  });
});
