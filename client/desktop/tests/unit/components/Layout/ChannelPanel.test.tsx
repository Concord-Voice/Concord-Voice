import { render, screen, fireEvent } from '../../../test-utils';
import { useLayoutStore } from '@/renderer/stores/layoutStore';
import ChannelPanel from '@/renderer/components/Layout/ChannelPanel';

// Mock UserPanel to prevent complex rendering
vi.mock('@/renderer/components/User/UserPanel', () => ({
  default: () => <div data-testid="user-panel">User Panel</div>,
}));

// Mock useResizablePanel
vi.mock('@/renderer/hooks/useResizablePanel', () => ({
  useResizablePanel: () => ({
    width: 240,
    onMouseDown: vi.fn(),
    onKeyDown: vi.fn(),
  }),
}));

describe('ChannelPanel', () => {
  beforeEach(() => {
    useLayoutStore.setState({
      channelPanelPinned: true,
      channelPanelHoverVisible: false,
      interfaceLocked: false,
    });
  });

  it('renders header and children in pinned mode', () => {
    render(
      <ChannelPanel header={<div>Test Header</div>}>
        <div>Test Content</div>
      </ChannelPanel>
    );
    expect(screen.getByText('Test Header')).toBeInTheDocument();
    expect(screen.getByText('Test Content')).toBeInTheDocument();
  });

  it('renders UserPanel in pinned mode', () => {
    render(
      <ChannelPanel header={<div>Header</div>}>
        <div>Content</div>
      </ChannelPanel>
    );
    expect(screen.getByTestId('user-panel')).toBeInTheDocument();
  });

  it('renders channels-sidebar class in pinned mode', () => {
    const { container } = render(
      <ChannelPanel header={<div>Header</div>}>
        <div>Content</div>
      </ChannelPanel>
    );
    expect(container.querySelector('.channels-sidebar')).toBeInTheDocument();
  });

  it('shows unpin button when pinned', () => {
    render(
      <ChannelPanel header={<div>Header</div>}>
        <div>Content</div>
      </ChannelPanel>
    );
    expect(screen.getByLabelText('Unpin panel')).toBeInTheDocument();
  });

  it('hides unpin button when forcePin is true', () => {
    render(
      <ChannelPanel header={<div>Header</div>} forcePin>
        <div>Content</div>
      </ChannelPanel>
    );
    expect(screen.queryByLabelText('Unpin panel')).not.toBeInTheDocument();
  });

  it('renders overlay in unpinned mode when hover visible', () => {
    useLayoutStore.setState({
      channelPanelPinned: false,
      channelPanelHoverVisible: true,
    });
    const { container } = render(
      <ChannelPanel header={<div>Header</div>}>
        <div>Content</div>
      </ChannelPanel>
    );
    expect(container.querySelector('.channel-panel-overlay')).toBeInTheDocument();
  });

  it('shows pin button in unpinned mode', () => {
    useLayoutStore.setState({
      channelPanelPinned: false,
      channelPanelHoverVisible: true,
    });
    render(
      <ChannelPanel header={<div>Header</div>}>
        <div>Content</div>
      </ChannelPanel>
    );
    expect(screen.getByLabelText('Pin panel open')).toBeInTheDocument();
  });

  it('toggles channel pin when unpin button clicked', () => {
    useLayoutStore.setState({ channelPanelPinned: true });
    render(
      <ChannelPanel header={<div>Header</div>}>
        <div>Content</div>
      </ChannelPanel>
    );
    fireEvent.click(screen.getByLabelText('Unpin panel'));
    expect(useLayoutStore.getState().channelPanelPinned).toBe(false);
  });

  it('toggles channel pin when pin button clicked in unpinned mode', () => {
    useLayoutStore.setState({ channelPanelPinned: false, channelPanelHoverVisible: true });
    render(
      <ChannelPanel header={<div>Header</div>}>
        <div>Content</div>
      </ChannelPanel>
    );
    fireEvent.click(screen.getByLabelText('Pin panel open'));
    expect(useLayoutStore.getState().channelPanelPinned).toBe(true);
  });

  it('renders overlay with slide-in class when visible', () => {
    useLayoutStore.setState({ channelPanelPinned: false, channelPanelHoverVisible: true });
    const { container } = render(
      <ChannelPanel header={<div>Header</div>}>
        <div>Content</div>
      </ChannelPanel>
    );
    const overlay = container.querySelector('.channel-panel-overlay');
    expect(overlay?.classList.contains('slide-in')).toBe(true);
  });

  it('renders overlay with slide-out class when not visible', () => {
    useLayoutStore.setState({ channelPanelPinned: false, channelPanelHoverVisible: false });
    const { container } = render(
      <ChannelPanel header={<div>Header</div>}>
        <div>Content</div>
      </ChannelPanel>
    );
    const overlay = container.querySelector('.channel-panel-overlay');
    expect(overlay?.classList.contains('slide-out')).toBe(true);
  });

  it('renders resize handle in pinned mode', () => {
    useLayoutStore.setState({ channelPanelPinned: true });
    const { container } = render(
      <ChannelPanel header={<div>Header</div>}>
        <div>Content</div>
      </ChannelPanel>
    );
    expect(container.querySelector('.layout-resize-handle')).toBeInTheDocument();
  });

  it('renders channels-sidebar-header in pinned mode', () => {
    useLayoutStore.setState({ channelPanelPinned: true });
    const { container } = render(
      <ChannelPanel header={<div>Header</div>}>
        <div>Content</div>
      </ChannelPanel>
    );
    expect(container.querySelector('.channels-sidebar-header')).toBeInTheDocument();
  });

  it('resize handle is keyboard-accessible with aria-label', () => {
    useLayoutStore.setState({ channelPanelPinned: true });
    render(
      <ChannelPanel header={<div>Header</div>}>
        <div>Content</div>
      </ChannelPanel>
    );
    const handle = screen.getByLabelText('Resize channel panel');
    expect(handle).toBeInTheDocument();
    expect(handle).toHaveAttribute('tabindex', '0');
  });

  // ── Edge lip handle (#188) ──────────────────────────────────────────────

  describe('edge lip handle (#188)', () => {
    it('renders the edge lip when unpinned and the panel is hidden', () => {
      useLayoutStore.setState({ channelPanelPinned: false, channelPanelHoverVisible: false });
      const { container } = render(
        <ChannelPanel header={<div>Header</div>}>
          <div>Content</div>
        </ChannelPanel>
      );
      const lip = screen.getByLabelText('Reveal channel panel');
      expect(lip).toBeInTheDocument();
      // Contains the right-pointing chevron cue.
      expect(container.querySelector('.channel-panel-lip svg')).toBeInTheDocument();
    });

    it('hides the edge lip once the overlay is revealed', () => {
      useLayoutStore.setState({ channelPanelPinned: false, channelPanelHoverVisible: true });
      render(
        <ChannelPanel header={<div>Header</div>}>
          <div>Content</div>
        </ChannelPanel>
      );
      expect(screen.queryByLabelText('Reveal channel panel')).not.toBeInTheDocument();
    });

    it('reveals the panel on lip hover (reuses the hover machinery)', () => {
      useLayoutStore.setState({ channelPanelPinned: false, channelPanelHoverVisible: false });
      render(
        <ChannelPanel header={<div>Header</div>}>
          <div>Content</div>
        </ChannelPanel>
      );
      fireEvent.mouseEnter(screen.getByLabelText('Reveal channel panel'));
      expect(useLayoutStore.getState().channelPanelHoverVisible).toBe(true);
    });

    it('re-pins the panel when the lip is clicked', () => {
      useLayoutStore.setState({ channelPanelPinned: false, channelPanelHoverVisible: false });
      render(
        <ChannelPanel header={<div>Header</div>}>
          <div>Content</div>
        </ChannelPanel>
      );
      fireEvent.click(screen.getByLabelText('Reveal channel panel'));
      expect(useLayoutStore.getState().channelPanelPinned).toBe(true);
    });
  });

  // ── Pin button state clarity (#188) ─────────────────────────────────────

  describe('pin button state clarity (#188)', () => {
    it('marks the pin button pressed and pinned-styled when pinned', () => {
      useLayoutStore.setState({ channelPanelPinned: true });
      render(
        <ChannelPanel header={<div>Header</div>}>
          <div>Content</div>
        </ChannelPanel>
      );
      const pin = screen.getByLabelText('Unpin panel');
      expect(pin).toHaveAttribute('aria-pressed', 'true');
      expect(pin).toHaveClass('pinned');
    });

    it('marks the pin button unpressed and not pinned-styled when unpinned', () => {
      useLayoutStore.setState({ channelPanelPinned: false, channelPanelHoverVisible: true });
      render(
        <ChannelPanel header={<div>Header</div>}>
          <div>Content</div>
        </ChannelPanel>
      );
      const pin = screen.getByLabelText('Pin panel open');
      expect(pin).toHaveAttribute('aria-pressed', 'false');
      expect(pin).not.toHaveClass('pinned');
    });
  });

  // ── Interface lock (#188) ───────────────────────────────────────────────

  describe('interface lock (#188)', () => {
    it('hides the pin button and resize handle in pinned mode when locked', () => {
      useLayoutStore.setState({ channelPanelPinned: true, interfaceLocked: true });
      render(
        <ChannelPanel header={<div>Header</div>}>
          <div>Content</div>
        </ChannelPanel>
      );
      expect(screen.queryByLabelText('Unpin panel')).not.toBeInTheDocument();
      expect(screen.queryByLabelText('Resize channel panel')).not.toBeInTheDocument();
    });

    it('keeps the edge lip visible in unpinned mode when locked (peek access)', () => {
      useLayoutStore.setState({
        channelPanelPinned: false,
        channelPanelHoverVisible: false,
        interfaceLocked: true,
      });
      render(
        <ChannelPanel header={<div>Header</div>}>
          <div>Content</div>
        </ChannelPanel>
      );
      // A closed panel still needs a peek affordance even when locked.
      expect(screen.getByLabelText('Reveal channel panel')).toBeInTheDocument();
    });

    it('lip click only peeks (does not re-pin) when locked', () => {
      useLayoutStore.setState({
        channelPanelPinned: false,
        channelPanelHoverVisible: false,
        interfaceLocked: true,
      });
      render(
        <ChannelPanel header={<div>Header</div>}>
          <div>Content</div>
        </ChannelPanel>
      );
      fireEvent.click(screen.getByLabelText('Reveal channel panel'));
      // Reveals (peeks) the panel...
      expect(useLayoutStore.getState().channelPanelHoverVisible).toBe(true);
      // ...but the pin state stays frozen while locked.
      expect(useLayoutStore.getState().channelPanelPinned).toBe(false);
    });

    it('hides the pin button in the revealed overlay when locked', () => {
      useLayoutStore.setState({
        channelPanelPinned: false,
        channelPanelHoverVisible: true,
        interfaceLocked: true,
      });
      render(
        <ChannelPanel header={<div>Header</div>}>
          <div>Content</div>
        </ChannelPanel>
      );
      expect(screen.queryByLabelText('Pin panel open')).not.toBeInTheDocument();
    });
  });
});
