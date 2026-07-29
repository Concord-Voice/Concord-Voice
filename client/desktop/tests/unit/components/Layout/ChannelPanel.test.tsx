import { render, screen } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useLayoutStore } from '@/renderer/stores/layoutStore';
import ChannelPanel from '@/renderer/components/Layout/ChannelPanel';
import { DockOverlayProvider } from '@/renderer/components/Layout/DockShell';

vi.mock('@/renderer/components/User/UserPanel', () => ({
  default: ({ compact = false }: { compact?: boolean }) => (
    <div data-testid="user-panel" data-compact={String(compact)}>
      User Panel
    </div>
  ),
}));

const renderPanel = (props: Partial<React.ComponentProps<typeof ChannelPanel>> = {}) =>
  render(
    <DockOverlayProvider>
      <ChannelPanel
        context="dm"
        header={<div>Test Header</div>}
        renderContent={(compact) => <div>{compact ? 'compact content' : 'standard content'}</div>}
        {...props}
      />
    </DockOverlayProvider>
  );

describe('ChannelPanel', () => {
  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
    useLayoutStore.setState({
      sidebarLayoutsDecoupled: true,
      sidebarProfiles: {
        dm: {
          left: { width: 212, pinned: true },
          right: { width: 260, pinned: true },
        },
        server: {
          left: { width: 120, pinned: true },
          right: { width: 306, pinned: true },
        },
      },
      interfaceLocked: false,
    });
  });

  it('adapts the DM left dock with the Threads label and footer', () => {
    renderPanel();

    expect(screen.getByText('Test Header')).toBeInTheDocument();
    expect(screen.getByText('standard content')).toBeInTheDocument();
    expect(screen.getByTestId('user-panel')).toHaveAttribute('data-compact', 'false');
    expect(screen.getByRole('button', { name: 'Unpin Threads sidebar' })).toBeInTheDocument();
  });

  it('passes the compact presentation through for a Server channel dock', () => {
    renderPanel({ context: 'server' });

    expect(screen.getByText('compact content')).toBeInTheDocument();
    expect(screen.getByTestId('user-panel')).toHaveAttribute('data-compact', 'true');
    expect(screen.getByRole('button', { name: 'Unpin Channels sidebar' })).toBeInTheDocument();
  });

  it('force-pins without exposing the pin control', () => {
    useLayoutStore.getState().setSidebarPinned('dm', 'left', false);
    const { container } = renderPanel({ forcePin: true });

    expect(container.querySelector('.dock-shell__surface')).toHaveAttribute('data-mode', 'pinned');
    expect(screen.queryByRole('button', { name: /Threads sidebar/ })).not.toBeInTheDocument();
  });
});
