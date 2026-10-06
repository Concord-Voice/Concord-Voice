import { act, fireEvent, render, screen } from '../../../test-utils';
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';
import { klipyClient } from '@/renderer/services/messaging/gifProvider/klipyClient';
import { usePrivacyStore } from '@/renderer/stores/ui/privacyStore';
import ContentSafetyControls from '@/renderer/components/Settings/ContentSafetyControls';

const gifIdMock = vi.hoisted(() => ({
  currentId: 'working-id' as string | null,
  listeners: new Set<() => void>(),
}));

vi.mock('@/renderer/services/messaging/gifProvider/klipyClient', () => ({
  klipyClient: {
    getCurrentCustomerId: vi.fn(() => gifIdMock.currentId),
    getCustomerID: vi.fn(() => Promise.resolve(gifIdMock.currentId)),
    setPersonalizationEnabled: vi.fn(),
    subscribeCustomerId: vi.fn((listener: () => void) => {
      gifIdMock.listeners.add(listener);
      return () => gifIdMock.listeners.delete(listener);
    }),
    rotateCustomerId: vi.fn(async () => {
      gifIdMock.currentId = 'rotated-id';
      for (const listener of gifIdMock.listeners) listener();
      return gifIdMock.currentId;
    }),
  },
}));

describe('ContentSafetyControls', () => {
  beforeEach(() => {
    resetAllStores();
    usePrivacyStore.setState({ loaded: true });
    gifIdMock.currentId = 'working-id';
    gifIdMock.listeners.clear();
    vi.clearAllMocks();
    vi.mocked(klipyClient.getCustomerID).mockImplementation(() =>
      Promise.resolve(gifIdMock.currentId)
    );
    vi.mocked(klipyClient.rotateCustomerId).mockImplementation(async () => {
      gifIdMock.currentId = 'rotated-id';
      for (const listener of gifIdMock.listeners) listener();
      return gifIdMock.currentId;
    });
  });

  it('renders the content safety subsection title', () => {
    render(<ContentSafetyControls />);
    expect(screen.getByText(/content safety/i)).toBeInTheDocument();
  });

  it('does not mint an ID from the placeholder privacy preference', async () => {
    usePrivacyStore.setState({ loaded: false });
    gifIdMock.currentId = null;
    render(<ContentSafetyControls />);

    expect(screen.getByRole('status')).toHaveTextContent('Loading privacy settings…');
    expect(screen.queryByRole('button', { name: 'Rotate' })).not.toBeInTheDocument();
    expect(klipyClient.getCustomerID).not.toHaveBeenCalled();
    expect(klipyClient.setPersonalizationEnabled).toHaveBeenCalledWith(false);

    act(() => {
      usePrivacyStore.setState((state) => ({
        loaded: true,
        settings: { ...state.settings, sharePersonalizationWithGifProvider: false },
      }));
    });

    await vi.waitFor(() => expect(klipyClient.getCustomerID).toHaveBeenCalledOnce());
    expect(klipyClient.setPersonalizationEnabled).not.toHaveBeenCalledWith(true);
  });

  it('shows a lazily loaded ID without remounting Settings', () => {
    gifIdMock.currentId = null;
    render(<ContentSafetyControls />);
    expect(screen.queryByText('new-active-id')).not.toBeInTheDocument();

    act(() => {
      gifIdMock.currentId = 'new-active-id';
      for (const listener of gifIdMock.listeners) listener();
    });

    expect(screen.getByText('new-active-id')).toBeInTheDocument();
  });

  it('updates the shown ID when the client rotates it automatically', () => {
    render(<ContentSafetyControls />);
    expect(screen.getByText('working-id')).toBeInTheDocument();

    act(() => {
      gifIdMock.currentId = 'new-active-id';
      for (const listener of gifIdMock.listeners) listener();
    });

    expect(screen.getByText('new-active-id')).toBeInTheDocument();
    expect(screen.queryByText('working-id')).not.toBeInTheDocument();
  });

  it('does not display an unused ID when rotation fails', async () => {
    vi.mocked(klipyClient.rotateCustomerId).mockRejectedValueOnce(new Error('server unavailable'));
    render(<ContentSafetyControls />);

    fireEvent.click(screen.getByRole('button', { name: 'Rotate' }));

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Could not rotate the Personalization ID'
    );
    expect(screen.getByText('working-id')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Rotate' })).toBeEnabled();
  });

  it('reports success only after the active ID changes', async () => {
    render(<ContentSafetyControls />);

    fireEvent.click(screen.getByRole('button', { name: 'Rotate' }));

    expect(await screen.findByText('rotated-id')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Rotated' })).toBeDisabled();
  });

  it('ignores a rotation result after the privacy mode changes', async () => {
    let finishRotation!: (id: string) => void;
    vi.mocked(klipyClient.rotateCustomerId).mockImplementationOnce(
      () =>
        new Promise<string>((resolve) => {
          finishRotation = resolve;
        })
    );
    render(<ContentSafetyControls />);
    fireEvent.click(screen.getByRole('button', { name: 'Rotate' }));
    expect(screen.getByRole('button', { name: 'Rotating…' })).toBeDisabled();

    act(() => {
      usePrivacyStore.setState((state) => ({
        settings: { ...state.settings, sharePersonalizationWithGifProvider: false },
      }));
    });
    await act(async () => finishRotation('stale-id'));

    expect(screen.getByRole('button', { name: 'Rotate' })).toBeEnabled();
    expect(screen.queryByText('Rotated')).not.toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });
});
