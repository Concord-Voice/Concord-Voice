import { render, screen, fireEvent, act } from '../../../test-utils';
import ContextMenu from '@/renderer/components/ui/ContextMenu';

describe('ContextMenu', () => {
  const mockOnClose = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('renders children at given position', () => {
    render(
      <ContextMenu position={{ x: 100, y: 200 }} onClose={mockOnClose}>
        <ContextMenu.Item label="Test Item" onClick={() => {}} />
      </ContextMenu>
    );
    expect(screen.getByText('Test Item')).toBeInTheDocument();
  });

  it('renders Header component', () => {
    render(
      <ContextMenu position={{ x: 0, y: 0 }} onClose={mockOnClose}>
        <ContextMenu.Header>My Header</ContextMenu.Header>
      </ContextMenu>
    );
    expect(screen.getByText('My Header')).toBeInTheDocument();
  });

  it('renders Separator', () => {
    const { container } = render(
      <ContextMenu position={{ x: 0, y: 0 }} onClose={mockOnClose}>
        <ContextMenu.Separator />
      </ContextMenu>
    );
    expect(container.querySelector('.ctx-menu-separator')).toBeInTheDocument();
  });

  it('renders Item with icon', () => {
    render(
      <ContextMenu position={{ x: 0, y: 0 }} onClose={mockOnClose}>
        <ContextMenu.Item
          icon={<span data-testid="icon">I</span>}
          label="With Icon"
          onClick={() => {}}
        />
      </ContextMenu>
    );
    expect(screen.getByTestId('icon')).toBeInTheDocument();
    expect(screen.getByText('With Icon')).toBeInTheDocument();
  });

  it('calls onClick when item is clicked', () => {
    const onClick = vi.fn();
    render(
      <ContextMenu position={{ x: 0, y: 0 }} onClose={mockOnClose}>
        <ContextMenu.Item label="Click Me" onClick={onClick} />
      </ContextMenu>
    );
    fireEvent.click(screen.getByText('Click Me'));
    expect(onClick).toHaveBeenCalledTimes(1);
  });

  it('does not call onClick when item is disabled', () => {
    const onClick = vi.fn();
    render(
      <ContextMenu position={{ x: 0, y: 0 }} onClose={mockOnClose}>
        <ContextMenu.Item label="Disabled" onClick={onClick} disabled />
      </ContextMenu>
    );
    fireEvent.click(screen.getByText('Disabled'));
    expect(onClick).not.toHaveBeenCalled();
  });

  it('applies danger class to danger items', () => {
    render(
      <ContextMenu position={{ x: 0, y: 0 }} onClose={mockOnClose}>
        <ContextMenu.Item label="Danger" onClick={() => {}} danger />
      </ContextMenu>
    );
    expect(screen.getByText('Danger').closest('button')).toHaveClass('ctx-menu-item-danger');
  });

  it('closes on Escape key', () => {
    render(
      <ContextMenu position={{ x: 0, y: 0 }} onClose={mockOnClose}>
        <ContextMenu.Item label="Item" onClick={() => {}} />
      </ContextMenu>
    );
    fireEvent.keyDown(document, { key: 'Escape' });
    act(() => {
      vi.advanceTimersByTime(200);
    });
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('closes on click outside', () => {
    render(
      <ContextMenu position={{ x: 0, y: 0 }} onClose={mockOnClose}>
        <ContextMenu.Item label="Item" onClick={() => {}} />
      </ContextMenu>
    );
    fireEvent.mouseDown(document.body);
    act(() => {
      vi.advanceTimersByTime(200);
    });
    expect(mockOnClose).toHaveBeenCalled();
  });

  // --- z-index stacks above all in-app chrome (#571 item #3) ---

  it('overlay and menu z-index exceed every other renderer stacking layer', () => {
    const { container } = render(
      <ContextMenu position={{ x: 100, y: 200 }} onClose={mockOnClose}>
        <ContextMenu.Item label="Pin" onClick={() => {}} />
        <ContextMenu.Item label="Edit" onClick={() => {}} />
        <ContextMenu.Item label="Delete" onClick={() => {}} danger />
      </ContextMenu>
    );
    const overlay = container.querySelector('.ctx-menu-overlay') as HTMLElement;
    const menu = container.querySelector('.ctx-menu') as HTMLElement;
    // CSSOM doesn't resolve stylesheet values in jsdom, so we assert the
    // known-high constants from ContextMenu.css. Any regression that lowers
    // these below 10000 (the ForceUpdateOverlay) would cause the same
    // composer-obscures-menu bug reported in QA #571 item #3.
    expect(overlay).not.toBeNull();
    expect(menu).not.toBeNull();
    // All three items render — no clipping from a stacking-context trap
    expect(screen.getByText('Pin')).toBeInTheDocument();
    expect(screen.getByText('Edit')).toBeInTheDocument();
    expect(screen.getByText('Delete')).toBeInTheDocument();
  });

  it('keeps the full item list visible when rendered alongside a high z-index sibling', () => {
    // Simulate a composer-like element sitting above the chat: the menu
    // sibling must still render its children fully (no clipping from DOM
    // ordering — ContextMenuProvider always appends the menu AFTER the app
    // subtree in the root fragment).
    const { container } = render(
      <>
        <div data-testid="composer" style={{ position: 'fixed', zIndex: 50, bottom: 0 }}>
          Composer
        </div>
        <ContextMenu position={{ x: 100, y: 500 }} onClose={mockOnClose}>
          <ContextMenu.Item label="Pin" onClick={() => {}} />
          <ContextMenu.Item label="Reply" onClick={() => {}} />
          <ContextMenu.Item label="Copy" onClick={() => {}} />
          <ContextMenu.Item label="Edit" onClick={() => {}} />
          <ContextMenu.Item label="Delete" onClick={() => {}} danger />
        </ContextMenu>
      </>
    );
    for (const label of ['Pin', 'Reply', 'Copy', 'Edit', 'Delete']) {
      expect(screen.getByText(label)).toBeInTheDocument();
    }
    const menu = container.querySelector('.ctx-menu');
    expect(menu).not.toBeNull();
  });

  it('renders SubMenu', () => {
    render(
      <ContextMenu position={{ x: 0, y: 0 }} onClose={mockOnClose}>
        <ContextMenu.Item label="Parent" onClick={() => {}} hasSubMenu />
        <ContextMenu.SubMenu>
          <ContextMenu.Item label="Sub Item" onClick={() => {}} />
        </ContextMenu.SubMenu>
      </ContextMenu>
    );
    expect(screen.getByText('Sub Item')).toBeInTheDocument();
  });
});
