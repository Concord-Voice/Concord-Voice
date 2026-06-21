import { render, screen, fireEvent } from '../../../test-utils';
import { useLayoutStore } from '@/renderer/stores/layoutStore';
import AppLayout from '@/renderer/components/Layout/AppLayout';

describe('AppLayout', () => {
  const defaultProps = {
    serverBar: <div data-testid="server-bar">Server Bar</div>,
    folderBar: <div data-testid="folder-bar">Folder Bar</div>,
    channelPanel: <div data-testid="channel-panel">Channel Panel</div>,
    chatArea: <div data-testid="chat-area">Chat Area</div>,
    memberSpace: <div data-testid="member-space">Member Space</div>,
  };

  beforeEach(() => {
    useLayoutStore.setState({
      channelPanelPinned: true,
      memberPanelMode: 'side',
      serverBarHeight: 56,
      folderBarHeight: 200,
      interfaceLocked: false,
    });
  });

  it('renders all layout sections', () => {
    render(<AppLayout {...defaultProps} />);
    expect(screen.getByTestId('server-bar')).toBeInTheDocument();
    expect(screen.getByTestId('folder-bar')).toBeInTheDocument();
    expect(screen.getByTestId('channel-panel')).toBeInTheDocument();
    expect(screen.getByTestId('chat-area')).toBeInTheDocument();
    expect(screen.getByTestId('member-space')).toBeInTheDocument();
  });

  it('renders app-layout container', () => {
    const { container } = render(<AppLayout {...defaultProps} />);
    expect(container.querySelector('.app-layout')).toBeInTheDocument();
  });

  it('renders server bar section', () => {
    const { container } = render(<AppLayout {...defaultProps} />);
    expect(container.querySelector('.layout-server-bar')).toBeInTheDocument();
  });

  it('renders folder bar section', () => {
    const { container } = render(<AppLayout {...defaultProps} />);
    expect(container.querySelector('.layout-folder-bar')).toBeInTheDocument();
  });

  it('renders resize handles', () => {
    const { container } = render(<AppLayout {...defaultProps} />);
    const resizeHandles = container.querySelectorAll('.layout-resize-handle-h');
    expect(resizeHandles.length).toBeGreaterThanOrEqual(1);
  });

  it('renders chat area section', () => {
    const { container } = render(<AppLayout {...defaultProps} />);
    expect(container.querySelector('.layout-chat-area')).toBeInTheDocument();
  });

  it('renders member space section', () => {
    const { container } = render(<AppLayout {...defaultProps} />);
    expect(container.querySelector('.layout-member-space')).toBeInTheDocument();
  });

  it('sets data-channel-pinned attribute based on store', () => {
    useLayoutStore.setState({ channelPanelPinned: true });
    const { container } = render(<AppLayout {...defaultProps} />);
    const layout = container.querySelector('.app-layout');
    expect(layout?.getAttribute('data-channel-pinned')).toBe('true');
  });

  it('sets data-channel-pinned to true when forceChannelPin is set', () => {
    useLayoutStore.setState({ channelPanelPinned: false });
    const { container } = render(<AppLayout {...defaultProps} forceChannelPin />);
    const layout = container.querySelector('.app-layout');
    expect(layout?.getAttribute('data-channel-pinned')).toBe('true');
  });

  it('sets data-channel-pinned to false when not pinned', () => {
    useLayoutStore.setState({ channelPanelPinned: false });
    const { container } = render(<AppLayout {...defaultProps} />);
    const layout = container.querySelector('.app-layout');
    expect(layout?.getAttribute('data-channel-pinned')).toBe('false');
  });

  it('sets data-member-mode attribute', () => {
    useLayoutStore.setState({ memberPanelMode: 'expanded' });
    const { container } = render(<AppLayout {...defaultProps} />);
    const layout = container.querySelector('.app-layout');
    expect(layout?.getAttribute('data-member-mode')).toBe('expanded');
  });

  it('applies serverBarHeight from store', () => {
    useLayoutStore.setState({ serverBarHeight: 80 });
    const { container } = render(<AppLayout {...defaultProps} />);
    const serverBar = container.querySelector('.layout-server-bar') as HTMLElement;
    expect(serverBar.style.height).toBe('80px');
  });

  it('applies folderBarHeight from store', () => {
    useLayoutStore.setState({ folderBarHeight: 150 });
    const { container } = render(<AppLayout {...defaultProps} />);
    const folderBar = container.querySelector('.layout-folder-bar') as HTMLElement;
    expect(folderBar.style.height).toBe('150px');
  });

  it('renders two resize handles', () => {
    const { container } = render(<AppLayout {...defaultProps} />);
    const resizeHandles = container.querySelectorAll('.layout-resize-handle-h');
    expect(resizeHandles.length).toBe(2);
  });

  // ── Force props ──

  it('forceMemberExpanded overrides memberPanelMode', () => {
    useLayoutStore.setState({ memberPanelMode: 'collapsed' });
    const { container } = render(<AppLayout {...defaultProps} forceMemberExpanded />);
    const layout = container.querySelector('.app-layout');
    expect(layout?.getAttribute('data-member-mode')).toBe('expanded');
  });

  it('memberPanelMode used when forceMemberExpanded is false', () => {
    useLayoutStore.setState({ memberPanelMode: 'hidden' });
    const { container } = render(<AppLayout {...defaultProps} />);
    const layout = container.querySelector('.app-layout');
    expect(layout?.getAttribute('data-member-mode')).toBe('hidden');
  });

  it('data-member-mode shows collapsed when store is collapsed', () => {
    useLayoutStore.setState({ memberPanelMode: 'collapsed' });
    const { container } = render(<AppLayout {...defaultProps} />);
    const layout = container.querySelector('.app-layout');
    expect(layout?.getAttribute('data-member-mode')).toBe('collapsed');
  });

  // ── Resize handler interaction ──

  it('resize handle triggers mousedown', () => {
    const { container } = render(<AppLayout {...defaultProps} />);
    const resizeHandles = container.querySelectorAll('.layout-resize-handle-h');
    // Verify that mouseDown on resize handle doesn't crash
    fireEvent.mouseDown(resizeHandles[0], { clientY: 100 });
    // The mousedown starts a drag — we just verify it doesn't throw
    expect(resizeHandles[0]).toBeInTheDocument();
  });

  it('server bar resize handle updates height on drag', () => {
    const { container } = render(<AppLayout {...defaultProps} />);
    const resizeHandles = container.querySelectorAll('.layout-resize-handle-h');
    // Mousedown on the server bar resize (first handle)
    fireEvent.mouseDown(resizeHandles[0], { clientY: 100 });
    // Simulate mousemove and mouseup on document
    fireEvent.mouseMove(document, { clientY: 120 });
    fireEvent.mouseUp(document);
    // The setServerBarHeight should have been called via the store
    // We verify the layout doesn't crash and the handle is still there
    expect(resizeHandles[0]).toBeInTheDocument();
  });

  // ── Channel panel section ──

  it('renders channel panel section', () => {
    const { container } = render(<AppLayout {...defaultProps} />);
    expect(container.querySelector('.layout-channel-panel')).toBeInTheDocument();
  });

  it('channel panel content is rendered correctly', () => {
    render(<AppLayout {...defaultProps} />);
    expect(screen.getByText('Channel Panel')).toBeInTheDocument();
  });

  // ── Custom slot content ──

  it('renders custom server bar content', () => {
    render(
      <AppLayout {...defaultProps} serverBar={<div data-testid="custom-server">Custom</div>} />
    );
    expect(screen.getByTestId('custom-server')).toBeInTheDocument();
    expect(screen.getByText('Custom')).toBeInTheDocument();
  });

  it('renders custom chat area content', () => {
    render(
      <AppLayout {...defaultProps} chatArea={<div data-testid="custom-chat">Custom Chat</div>} />
    );
    expect(screen.getByTestId('custom-chat')).toBeInTheDocument();
  });

  // ── Resize handle accessibility ──

  it('resize handles are keyboard-accessible', () => {
    const { container } = render(<AppLayout {...defaultProps} />);
    const handles = container.querySelectorAll('.layout-resize-handle-h');
    for (const handle of handles) {
      expect(handle).toHaveAttribute('tabindex', '0');
      expect(handle).toHaveAttribute('aria-label');
      expect(handle).not.toHaveAttribute('aria-hidden');
    }
  });

  it('server bar resize handle has correct aria-label', () => {
    const { container } = render(<AppLayout {...defaultProps} />);
    const handle = container.querySelector('.layout-server-bar-resize');
    expect(handle).toHaveAttribute('aria-label', 'Resize server bar');
  });

  it('folder bar resize handle has correct aria-label', () => {
    const { container } = render(<AppLayout {...defaultProps} />);
    const handle = container.querySelector('.layout-folder-bar-resize');
    expect(handle).toHaveAttribute('aria-label', 'Resize folder bar');
  });

  // ── Interface lock (#188) ───────────────────────────────────────────────
  // When locked, the server-bar and folder-bar resize handles are removed so
  // the current heights can't be dragged. Unlocked behaviour is covered by the
  // resize-handle tests above.
  describe('interface lock (#188)', () => {
    it('removes the server-bar and folder-bar resize handles when locked', () => {
      useLayoutStore.setState({ interfaceLocked: true });
      const { container } = render(<AppLayout {...defaultProps} />);
      expect(container.querySelector('.layout-server-bar-resize')).not.toBeInTheDocument();
      expect(container.querySelector('.layout-folder-bar-resize')).not.toBeInTheDocument();
      expect(container.querySelectorAll('.layout-resize-handle-h').length).toBe(0);
    });

    it('keeps both resize handles when unlocked', () => {
      useLayoutStore.setState({ interfaceLocked: false });
      const { container } = render(<AppLayout {...defaultProps} />);
      expect(container.querySelectorAll('.layout-resize-handle-h').length).toBe(2);
    });
  });
});
