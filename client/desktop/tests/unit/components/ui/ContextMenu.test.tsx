import { render, screen, fireEvent, act } from '../../../test-utils';
import ContextMenu from '@/renderer/components/ui/ContextMenu';
import { ModalPortalHostContext } from '@/renderer/components/ui/ModalContext';
import { openForeignModal } from '../../../helpers/foreignModal';

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
    render(
      <ContextMenu position={{ x: 0, y: 0 }} onClose={mockOnClose}>
        <ContextMenu.Separator />
      </ContextMenu>
    );
    expect(document.querySelector('.ctx-menu-separator')).toBeInTheDocument();
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

  it('leaves Escape to a modal dialog open in front of it', () => {
    render(
      <ContextMenu position={{ x: 0, y: 0 }} onClose={mockOnClose}>
        <ContextMenu.Item label="Item" onClick={() => {}} />
      </ContextMenu>
    );
    const { input } = openForeignModal();
    fireEvent.keyDown(input, { key: 'Escape' });
    act(() => {
      vi.advanceTimersByTime(200);
    });
    expect(mockOnClose).not.toHaveBeenCalled();
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
    render(
      <ContextMenu position={{ x: 100, y: 200 }} onClose={mockOnClose}>
        <ContextMenu.Item label="Pin" onClick={() => {}} />
        <ContextMenu.Item label="Edit" onClick={() => {}} />
        <ContextMenu.Item label="Delete" onClick={() => {}} danger />
      </ContextMenu>
    );
    const overlay = document.querySelector('.ctx-menu-overlay') as HTMLElement;
    const menu = document.querySelector('.ctx-menu') as HTMLElement;
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
    render(
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
    const menu = document.querySelector('.ctx-menu');
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
  // --- Viewport clamping (#2367 part 2) ---------------------------------
  //
  // jsdom lays nothing out, so geometry is stubbed per class. The root menu is
  // sized from offsetWidth/offsetHeight (the layout box, immune to the
  // entrance animation's scale); getBoundingClientRect is stubbed to the same
  // box so the pre-fix, rect-based code would also see a real menu.
  describe('viewport clamping', () => {
    const originalInnerHeight = globalThis.innerHeight;
    const originalInnerWidth = globalThis.innerWidth;
    let boxes: Record<string, { width: number; height: number; top?: number; left?: number }>;

    const boxFor = (el: Element) => {
      for (const [cls, box] of Object.entries(boxes)) {
        if (el.classList.contains(cls)) return box;
      }
      return null;
    };
    const setViewport = (width: number, height: number) => {
      Object.defineProperty(globalThis, 'innerWidth', { configurable: true, value: width });
      Object.defineProperty(globalThis, 'innerHeight', { configurable: true, value: height });
    };

    beforeEach(() => {
      boxes = {};
      vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockImplementation(function (
        this: HTMLElement
      ) {
        return boxFor(this)?.width ?? 0;
      });
      vi.spyOn(HTMLElement.prototype, 'offsetHeight', 'get').mockImplementation(function (
        this: HTMLElement
      ) {
        return boxFor(this)?.height ?? 0;
      });
      // A submenu is positioned against its trigger wrapper; jsdom has no layout,
      // so its `offsetParent` is otherwise always null.
      vi.spyOn(HTMLElement.prototype, 'offsetParent', 'get').mockImplementation(function (
        this: HTMLElement
      ) {
        return this.classList.contains('ctx-submenu') ? this.parentElement : null;
      });
      // A submenu's CSS `top` is -4px relative to its trigger wrapper.
      vi.spyOn(HTMLElement.prototype, 'offsetTop', 'get').mockImplementation(function (
        this: HTMLElement
      ) {
        return this.classList.contains('ctx-submenu') ? -4 : 0;
      });
      vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function (
        this: Element
      ) {
        const box = boxFor(this);
        const el = this as HTMLElement;
        const top = box?.top ?? (Number.parseFloat(el.style.top) || 0);
        const left = box?.left ?? (Number.parseFloat(el.style.left) || 0);
        const width = box?.width ?? 0;
        const height = box?.rectHeight ?? box?.height ?? 0;
        return {
          top,
          left,
          width,
          height,
          bottom: top + height,
          right: left + width,
          x: left,
          y: top,
          toJSON: () => ({}),
        } as DOMRect;
      });
    });

    afterEach(() => {
      vi.restoreAllMocks();
      setViewport(originalInnerWidth, originalInnerHeight);
    });

    it('keeps a tall menu flipped above a low click inside the top margin', () => {
      // The measured #2367 case: a click at y=383 with a menu taller than the
      // 383px above it. Flipping alone put the top at 383 − 500 = −117.
      setViewport(1024, 600);
      boxes = { 'ctx-menu': { width: 200, height: 500 } };
      render(
        <ContextMenu position={{ x: 100, y: 383 }} onClose={mockOnClose}>
          <ContextMenu.Item label="Pin" onClick={() => {}} />
        </ContextMenu>
      );
      const menu = document.querySelector('.ctx-menu') as HTMLElement;
      const top = Number.parseFloat(menu.style.top);
      expect(top).toBeGreaterThanOrEqual(8);
      expect(top + 500).toBeLessThanOrEqual(600 - 8);
    });

    it('flips a 200px menu clicked at y=500 in a 600px viewport to exactly top:300px (flip, not mere clamp)', () => {
      // Clamping alone (no flip) would pin this at 600 − 200 − 8 = 392px. Only the
      // flip (position.y − height = 500 − 200 = 300) lands here, and 300 already
      // sits inside the clamp band, so this isolates the flip from the clamp.
      setViewport(1024, 600);
      boxes = { 'ctx-menu': { width: 200, height: 200 } };
      render(
        <ContextMenu position={{ x: 100, y: 500 }} onClose={mockOnClose}>
          <ContextMenu.Item label="Pin" onClick={() => {}} />
        </ContextMenu>
      );
      const menu = document.querySelector('.ctx-menu') as HTMLElement;
      expect(menu.style.top).toBe('300px');
    });

    it('keeps a wide menu flipped left of a click inside the left margin', () => {
      setViewport(400, 768);
      boxes = { 'ctx-menu': { width: 300, height: 100 } };
      render(
        <ContextMenu position={{ x: 150, y: 100 }} onClose={mockOnClose}>
          <ContextMenu.Item label="Pin" onClick={() => {}} />
        </ContextMenu>
      );
      const menu = document.querySelector('.ctx-menu') as HTMLElement;
      expect(Number.parseFloat(menu.style.left)).toBeGreaterThanOrEqual(8);
    });

    it('caps a menu taller than the viewport and lets it scroll', () => {
      setViewport(1024, 600);
      boxes = { 'ctx-menu': { width: 200, height: 700 } };
      render(
        <ContextMenu position={{ x: 100, y: 300 }} onClose={mockOnClose}>
          <ContextMenu.Item label="Pin" onClick={() => {}} />
        </ContextMenu>
      );
      const menu = document.querySelector('.ctx-menu') as HTMLElement;
      expect(menu.style.top).toBe('8px');
      expect(menu.style.maxHeight).toBe('584px');
      expect(menu.style.overflowY).toBe('auto');
    });

    it('leaves a menu that fits unscrollable, so its submenu flyouts are not clipped', () => {
      setViewport(1024, 600);
      boxes = { 'ctx-menu': { width: 200, height: 300 } };
      render(
        <ContextMenu position={{ x: 100, y: 100 }} onClose={mockOnClose}>
          <ContextMenu.Item label="Pin" onClick={() => {}} />
        </ContextMenu>
      );
      const menu = document.querySelector('.ctx-menu') as HTMLElement;
      expect(menu.style.overflowY).toBe('');
      expect(menu.style.maxHeight).toBe('');
      // And one that fits where it was clicked is not moved at all.
      expect(menu.style.top).toBe('100px');
    });

    it('never nudges a tall submenu past the top of the viewport', () => {
      setViewport(1024, 600);
      boxes = {
        'ctx-menu-item-wrapper': { width: 200, height: 30, top: 304, left: 150 },
        'ctx-submenu': { width: 150, height: 700, top: 300, left: 300 },
      };
      render(
        <ContextMenu position={{ x: 100, y: 100 }} onClose={mockOnClose}>
          <div className="ctx-menu-item-wrapper">
            <ContextMenu.Item label="Move to" onClick={() => {}} hasSubMenu />
            <ContextMenu.SubMenu>
              <ContextMenu.Item label="Sub Item" onClick={() => {}} />
            </ContextMenu.SubMenu>
          </div>
        </ContextMenu>
      );
      const sub = document.querySelector('.ctx-submenu') as HTMLElement;
      // Viewport top = the wrapper's top + the submenu's (nudged) offset in it.
      const viewportTop = 304 + Number.parseFloat(sub.style.top);
      expect(viewportTop).toBeGreaterThanOrEqual(8);
      expect(sub.style.maxHeight).toBe('584px');
      expect(sub.style.overflowY).toBe('auto');
    });

    it('sizes a submenu from its layout box, not its mid-animation rect', () => {
      // `ctxSubMenuIn` starts at scale(0.96), so a rect read on mount reports 576
      // for a 600px submenu. 576 fits the 584px the viewport allows; 600 does not.
      setViewport(1024, 600);
      boxes = {
        'ctx-menu-item-wrapper': { width: 200, height: 30, top: 104, left: 150 },
        'ctx-submenu': { width: 150, height: 600, rectHeight: 576, top: 100, left: 300 },
      };
      render(
        <ContextMenu position={{ x: 100, y: 100 }} onClose={mockOnClose}>
          <div className="ctx-menu-item-wrapper">
            <ContextMenu.Item label="Move to" onClick={() => {}} hasSubMenu />
            <ContextMenu.SubMenu>
              <ContextMenu.Item label="Sub Item" onClick={() => {}} />
            </ContextMenu.SubMenu>
          </div>
        </ContextMenu>
      );
      const sub = document.querySelector('.ctx-submenu') as HTMLElement;
      expect(sub.style.maxHeight).toBe('584px');
      expect(sub.style.overflowY).toBe('auto');
      expect(104 + Number.parseFloat(sub.style.top)).toBeGreaterThanOrEqual(8);
    });

    describe('horizontal', () => {
      // Unflipped, a submenu sits its 6px margin past the right of its trigger.
      const renderSubMenu = (wrapper: { left: number; width: number }, subWidth: number) => {
        boxes = {
          'ctx-menu-item-wrapper': { ...wrapper, height: 30, top: 100 },
          'ctx-submenu': { width: subWidth, height: 100 },
        };
        vi.spyOn(HTMLElement.prototype, 'offsetLeft', 'get').mockImplementation(function (
          this: HTMLElement
        ) {
          return this.classList.contains('ctx-submenu') ? wrapper.width + 6 : 0;
        });
        render(
          <ContextMenu position={{ x: 100, y: 100 }} onClose={mockOnClose}>
            <div className="ctx-menu-item-wrapper">
              <ContextMenu.Item label="Roles" onClick={() => {}} hasSubMenu />
              <ContextMenu.SubMenu>
                <ContextMenu.Item label="Sub Item" onClick={() => {}} />
              </ContextMenu.SubMenu>
            </div>
          </ContextMenu>
        );
        return document.querySelector('.ctx-submenu') as HTMLElement;
      };

      it('flips a submenu that overflows right, leaving it beside its trigger when it fits', () => {
        setViewport(1024, 600);
        // 700 + 206 + 150 = 1056 > 1024; flipped, its left is 700 − 6 − 150 = 544.
        const sub = renderSubMenu({ left: 700, width: 200 }, 150);
        expect(sub).toHaveClass('ctx-submenu-flip');
        expect(sub.style.left).toBe('');
      });

      it('clamps a flipped submenu that would cross the left edge', () => {
        // A 560px root menu pinned right in an 800px layout: flipped, the 300px
        // submenu's left is 200 − 6 − 300 = −106, off-screen.
        setViewport(800, 600);
        const sub = renderSubMenu({ left: 200, width: 560 }, 300);
        expect(sub).toHaveClass('ctx-submenu-flip');
        expect(200 + Number.parseFloat(sub.style.left)).toBe(8);
        expect(sub.style.right).toBe('auto');
        expect(sub.style.width).toBe('300px');
      });
    });
  });

  // #2366: rendered in place, the overlay inherited the font of the area that
  // opened it (Navigation or Messages). It must portal out like ui/Modal.
  describe('portal', () => {
    it('renders outside the region that opened it', () => {
      render(
        <div className="message-list">
          <ContextMenu position={{ x: 10, y: 10 }} onClose={mockOnClose}>
            <ContextMenu.Item label="Pin" onClick={() => {}} />
          </ContextMenu>
        </div>
      );
      const root = document.querySelector('.ctx-menu-overlay');
      expect(root?.parentElement).toBe(document.body);
      expect(root?.closest('.message-list')).toBeNull();
    });

    it('renders into a modal portal host when one is provided', () => {
      const host = document.createElement('div');
      document.body.appendChild(host);
      render(
        <ModalPortalHostContext.Provider value={host}>
          <ContextMenu position={{ x: 10, y: 10 }} onClose={mockOnClose}>
            <ContextMenu.Item label="Pin" onClick={() => {}} />
          </ContextMenu>
        </ModalPortalHostContext.Provider>
      );
      expect(host.querySelector('.ctx-menu-overlay')).not.toBeNull();
      host.remove();
    });
  });
});
