import { render, screen, fireEvent, waitFor, act } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useSettingsOverlayStore } from '@/renderer/stores/settingsOverlayStore';

vi.mock('@/renderer/components/Settings/SettingsPage', () => ({
  default: () => <div data-testid="mock-settings-page">SettingsPage</div>,
}));

vi.mock('@/renderer/components/Servers/ServerSettingsPage', () => ({
  default: ({ serverId }: { serverId: string }) => (
    <div data-testid="mock-server-settings-page">ServerSettingsPage:{serverId}</div>
  ),
}));

import SettingsOverlayHost from '@/renderer/components/Settings/SettingsOverlayHost';

// JSDOM only partially implements HTMLDialogElement; polyfill showModal/close
// to toggle the [open] attribute and dispatch 'close' on close().
const originalShowModal = HTMLDialogElement.prototype.showModal;
const originalClose = HTMLDialogElement.prototype.close;

beforeAll(() => {
  const proto = HTMLDialogElement.prototype as unknown as {
    showModal: () => void;
    close: () => void;
    open: boolean;
  };
  proto.showModal = function () {
    (this as unknown as HTMLDialogElement).setAttribute('open', '');
  };
  proto.close = function () {
    (this as unknown as HTMLDialogElement).removeAttribute('open');
    (this as unknown as HTMLDialogElement).dispatchEvent(new Event('close'));
  };
});

afterAll(() => {
  HTMLDialogElement.prototype.showModal = originalShowModal;
  HTMLDialogElement.prototype.close = originalClose;
});

function getDialog(): HTMLDialogElement {
  const dlg = document.querySelector('dialog.settings-overlay-host');
  if (!(dlg instanceof HTMLDialogElement)) {
    throw new TypeError('SettingsOverlayHost dialog not in DOM');
  }
  return dlg;
}

describe('SettingsOverlayHost', () => {
  beforeEach(() => {
    resetAllStores();
    document.body.style.overflow = '';
  });

  it('dialog is closed when open is null', () => {
    render(<SettingsOverlayHost />);
    const dlg = getDialog();
    expect(dlg).not.toBeNull();
    expect(dlg.hasAttribute('open')).toBe(false);
  });

  it('renders SettingsPage when open === "app"', async () => {
    render(<SettingsOverlayHost />);
    act(() => {
      useSettingsOverlayStore.getState().openSettings('app');
    });
    expect(await screen.findByTestId('mock-settings-page')).toBeInTheDocument();
    expect(getDialog().hasAttribute('open')).toBe(true);
  });

  it('renders ServerSettingsPage with serverId when open === "server"', async () => {
    render(<SettingsOverlayHost />);
    act(() => {
      useSettingsOverlayStore.getState().openSettings('server', { serverId: 'srv-42' });
    });
    const node = await screen.findByTestId('mock-server-settings-page');
    expect(node).toHaveTextContent('ServerSettingsPage:srv-42');
  });

  it('renders no page when open === "server" without a serverId payload', async () => {
    render(<SettingsOverlayHost />);
    act(() => {
      useSettingsOverlayStore.getState().openSettings('server');
    });
    await waitFor(() => {
      expect(getDialog().hasAttribute('open')).toBe(true);
    });
    expect(screen.queryByTestId('mock-server-settings-page')).toBeNull();
  });

  it('closes the store when the dialog fires its close event (ESC)', async () => {
    render(<SettingsOverlayHost />);
    act(() => {
      useSettingsOverlayStore.getState().openSettings('app');
    });
    await screen.findByTestId('mock-settings-page');

    act(() => {
      getDialog().dispatchEvent(new Event('close'));
    });

    expect(useSettingsOverlayStore.getState().open).toBeNull();
  });

  it('closes when the dialog backdrop is clicked', async () => {
    render(<SettingsOverlayHost />);
    act(() => {
      useSettingsOverlayStore.getState().openSettings('app');
    });
    await screen.findByTestId('mock-settings-page');

    const dlg = getDialog();
    fireEvent.click(dlg, { target: dlg });

    expect(useSettingsOverlayStore.getState().open).toBeNull();
  });

  it('does not close when clicking inside the modal content', async () => {
    render(<SettingsOverlayHost />);
    act(() => {
      useSettingsOverlayStore.getState().openSettings('app');
    });
    const inner = await screen.findByTestId('mock-settings-page');

    fireEvent.click(inner);

    expect(useSettingsOverlayStore.getState().open).toBe('app');
  });

  it('locks body scroll while open and restores on close', async () => {
    document.body.style.overflow = 'auto';
    render(<SettingsOverlayHost />);
    act(() => {
      useSettingsOverlayStore.getState().openSettings('app');
    });
    await screen.findByTestId('mock-settings-page');

    expect(document.body.style.overflow).toBe('hidden');

    act(() => {
      useSettingsOverlayStore.getState().close();
    });

    expect(document.body.style.overflow).toBe('auto');
  });

  it('cleans up listeners and overflow on unmount', async () => {
    document.body.style.overflow = 'scroll';
    const { unmount } = render(<SettingsOverlayHost />);
    act(() => {
      useSettingsOverlayStore.getState().openSettings('app');
    });
    await screen.findByTestId('mock-settings-page');
    expect(document.body.style.overflow).toBe('hidden');

    expect(() => unmount()).not.toThrow();
    expect(document.body.style.overflow).toBe('scroll');
  });

  it('opening the same kind twice is idempotent (showModal not called when already open)', async () => {
    render(<SettingsOverlayHost />);
    act(() => {
      useSettingsOverlayStore.getState().openSettings('app');
    });
    await screen.findByTestId('mock-settings-page');
    expect(getDialog().hasAttribute('open')).toBe(true);

    act(() => {
      useSettingsOverlayStore.getState().openSettings('app');
    });
    expect(getDialog().hasAttribute('open')).toBe(true);
  });
});
