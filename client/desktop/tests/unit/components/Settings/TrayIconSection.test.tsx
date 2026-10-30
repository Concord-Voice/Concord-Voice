import { render, screen, fireEvent, userEvent, waitFor } from '../../../test-utils';
import { vi } from 'vitest';

const mockSetDraftAppearanceSetting = vi.fn();
let mockTrayIconStyle: unknown = 'auto';

vi.mock('@/renderer/hooks/ui/useDraftSettings', () => ({
  useDraftAppearance: vi.fn(() => ({ trayIconStyle: mockTrayIconStyle })),
  setDraftAppearanceSetting: (...args: unknown[]) => mockSetDraftAppearanceSetting(...args),
}));

import TrayIconSection from '@/renderer/components/Settings/TrayIconSection';

function setPlatform(impl: (() => Promise<string>) | undefined) {
  (globalThis as { electron?: unknown }).electron = impl ? { getPlatform: impl } : undefined;
}

beforeEach(() => {
  mockTrayIconStyle = 'auto';
  mockSetDraftAppearanceSetting.mockClear();
  setPlatform(() => Promise.resolve('linux'));
});

afterEach(() => {
  setPlatform(undefined);
});

describe('TrayIconSection (Appearance ▸ Tray Icon)', () => {
  it('titles the section "Tray Icon" off macOS', async () => {
    render(<TrayIconSection />);
    expect(await screen.findByText('Tray Icon')).toBeInTheDocument();
    expect(
      screen.getByText('Choose how the Concord Voice icon looks in your system tray.')
    ).toBeInTheDocument();
    expect(screen.getByText('Full colour.')).toBeInTheDocument();
  });

  it('defaults to the non-mac title while the platform is pending', () => {
    setPlatform(() => new Promise<string>(() => {}));
    render(<TrayIconSection />);
    expect(screen.getByText('Tray Icon')).toBeInTheDocument();
  });

  it('titles the section "Menu Bar Icon" on macOS', async () => {
    setPlatform(() => Promise.resolve('darwin'));
    render(<TrayIconSection />);
    expect(await screen.findByText('Menu Bar Icon')).toBeInTheDocument();
    expect(
      screen.getByText('Choose how the Concord Voice icon looks in your menu bar.')
    ).toBeInTheDocument();
    expect(screen.getByText('Matches your menu bar and shows status colours.')).toBeInTheDocument();
  });

  it('stays on the non-mac title when getPlatform rejects or is absent', async () => {
    setPlatform(() => Promise.reject(new Error('x')));
    const { unmount } = render(<TrayIconSection />);
    await waitFor(() => expect(screen.getByText('Tray Icon')).toBeInTheDocument());
    unmount();
    setPlatform(undefined);
    render(<TrayIconSection />);
    expect(screen.getByText('Tray Icon')).toBeInTheDocument();
  });

  it('renders four radios with Automatic checked by default', () => {
    render(<TrayIconSection />);
    const radios = screen.getAllByRole('radio');
    expect(radios).toHaveLength(4);
    expect(screen.getByRole('radio', { name: 'Automatic' })).toBeChecked();
    for (const name of ['Light', 'Dark', 'Colour']) {
      expect(screen.getByRole('radio', { name })).not.toBeChecked();
    }
  });

  it('shows a decorative preview image per option', () => {
    const { container } = render(<TrayIconSection />);
    const imgs = container.querySelectorAll('img');
    expect(imgs).toHaveLength(4);
    for (const img of imgs) expect(img.getAttribute('alt')).toBe('');
  });

  it('clicking Dark writes trayIconStyle to the draft store', () => {
    render(<TrayIconSection />);
    fireEvent.click(screen.getByRole('radio', { name: 'Dark' }));
    expect(mockSetDraftAppearanceSetting).toHaveBeenCalledWith('trayIconStyle', 'dark');
  });

  it('reflects the draft value as the checked radio', () => {
    mockTrayIconStyle = 'colour';
    render(<TrayIconSection />);
    expect(screen.getByRole('radio', { name: 'Colour' })).toBeChecked();
  });

  it('falls back to Automatic for an unknown draft value', () => {
    mockTrayIconStyle = 'neon';
    render(<TrayIconSection />);
    expect(screen.getByRole('radio', { name: 'Automatic' })).toBeChecked();
  });

  it('arrow keys move the selection through the native radio group', async () => {
    const user = userEvent.setup();
    render(<TrayIconSection />);
    screen.getByRole('radio', { name: 'Automatic' }).focus();
    await user.keyboard('{ArrowDown}');
    expect(mockSetDraftAppearanceSetting).toHaveBeenCalledWith('trayIconStyle', 'light');
  });
});
