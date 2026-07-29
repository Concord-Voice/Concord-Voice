import { fireEvent, render, screen } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { selectSidebarDock, useLayoutStore } from '@/renderer/stores/layoutStore';
import AppLayout from '@/renderer/components/Layout/AppLayout';
import { DockOverlayProvider, DockShell } from '@/renderer/components/Layout/DockShell';

describe('AppLayout', () => {
  const defaultProps = {
    context: 'dm' as const,
    serverBar: <div data-testid="server-bar">Server Bar</div>,
    folderBar: <div data-testid="folder-bar">Folder Bar</div>,
    channelPanel: <div data-testid="channel-panel">Channel Panel</div>,
    chatArea: <div data-testid="chat-area">Chat Area</div>,
    memberSpace: <div data-testid="member-space">Member Space</div>,
  };

  beforeEach(() => {
    resetAllStores();
    useLayoutStore.setState({
      sidebarLayoutsDecoupled: true,
      sidebarProfiles: {
        dm: {
          left: { width: 240, pinned: true },
          right: { width: 260, pinned: false },
        },
        server: {
          left: { width: 300, pinned: false },
          right: { width: 320, pinned: true },
        },
      },
      serverBarHeight: 56,
      folderBarHeight: 40,
      interfaceLocked: false,
    });
  });

  it('renders all layout slots', () => {
    render(<AppLayout {...defaultProps} />);
    expect(screen.getByTestId('server-bar')).toBeInTheDocument();
    expect(screen.getByTestId('folder-bar')).toBeInTheDocument();
    expect(screen.getByTestId('channel-panel')).toBeInTheDocument();
    expect(screen.getByTestId('chat-area')).toBeInTheDocument();
    expect(screen.getByTestId('member-space')).toBeInTheDocument();
  });

  it.each([
    ['dm', 'true', 'false'],
    ['server', 'false', 'true'],
  ] as const)('resolves %s left and right pin attributes', (context, left, right) => {
    const { container } = render(<AppLayout {...defaultProps} context={context} />);
    const layout = container.querySelector('.app-layout');

    expect(layout).toHaveAttribute('data-left-pinned', left);
    expect(layout).toHaveAttribute('data-right-pinned', right);
    expect(layout).not.toHaveAttribute('data-channel-pinned');
    expect(layout).not.toHaveAttribute('data-member-mode');
  });

  it.each([
    ['dm', 'left', 56, true, 'compact'],
    ['dm', 'right', 179, false, 'compact'],
    ['server', 'left', 180, true, 'standard'],
    ['server', 'right', 340, true, 'standard'],
  ] as const)(
    '%s %s resolves %ipx pinned=%s as %s',
    (context, side, width, pinned, presentation) => {
      useLayoutStore.setState({
        sidebarLayoutsDecoupled: true,
        sidebarProfiles: {
          dm: {
            left: { width: context === 'dm' && side === 'left' ? width : 240, pinned: true },
            right: { width: context === 'dm' && side === 'right' ? width : 260, pinned: true },
          },
          server: {
            left: { width: context === 'server' && side === 'left' ? width : 240, pinned: true },
            right: {
              width: context === 'server' && side === 'right' ? width : 260,
              pinned: true,
            },
          },
        },
      });
      useLayoutStore.getState().setSidebarPinned(context, side, pinned);

      render(
        <DockOverlayProvider>
          <DockShell
            context={context}
            side={side}
            label="Test"
            header={<span>Test</span>}
            renderBody={(compact) => <span>{compact ? 'compact' : 'standard'}</span>}
          />
        </DockOverlayProvider>
      );

      if (!pinned) {
        fireEvent.focus(screen.getByRole('button', { name: 'Open Test sidebar' }));
      }
      expect(screen.getByText(presentation)).toBeVisible();
      expect(selectSidebarDock(useLayoutStore.getState(), context, side)).toEqual({
        width,
        pinned,
      });
    }
  );

  it('forceChannelPin affects only the resolved left column', () => {
    const { container } = render(<AppLayout {...defaultProps} context="server" forceChannelPin />);
    const layout = container.querySelector('.app-layout');

    expect(layout).toHaveAttribute('data-left-pinned', 'true');
    expect(layout).toHaveAttribute('data-right-pinned', 'true');
    expect(useLayoutStore.getState().sidebarProfiles.server.left.pinned).toBe(false);
  });

  it('applies retained bar heights', () => {
    const { container } = render(<AppLayout {...defaultProps} />);
    expect(container.querySelector('.layout-server-bar')).toHaveStyle({ height: '56px' });
    expect(container.querySelector('.layout-folder-bar')).toHaveStyle({ height: '40px' });
  });

  it('commits server bar pointer resizing', () => {
    const { container } = render(<AppLayout {...defaultProps} />);
    const handle = screen.getByRole('button', { name: 'Resize server bar' });
    const serverBar = container.querySelector('.layout-server-bar');
    vi.spyOn(serverBar as Element, 'getBoundingClientRect').mockReturnValue({
      width: 0,
      height: 56,
      top: 0,
      right: 0,
      bottom: 56,
      left: 0,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    });

    fireEvent.mouseDown(handle, { clientY: 100 });
    fireEvent.mouseMove(document, { clientY: 105 });
    fireEvent.mouseUp(document);

    expect(useLayoutStore.getState().serverBarHeight).toBe(61);
  });

  it('keeps both bar resize controls keyboard accessible', () => {
    render(<AppLayout {...defaultProps} />);
    expect(screen.getByRole('button', { name: 'Resize server bar' })).toHaveAttribute(
      'tabindex',
      '0'
    );
    expect(screen.getByRole('button', { name: 'Resize folder bar' })).toHaveAttribute(
      'tabindex',
      '0'
    );
  });

  it('removes both bar resize controls under Interface Lock', () => {
    useLayoutStore.setState({ interfaceLocked: true });
    render(<AppLayout {...defaultProps} />);
    expect(screen.queryByRole('button', { name: 'Resize server bar' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Resize folder bar' })).not.toBeInTheDocument();
  });
});
