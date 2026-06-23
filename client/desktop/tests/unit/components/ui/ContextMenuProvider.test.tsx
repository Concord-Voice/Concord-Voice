import { render, screen, fireEvent, act } from '../../../test-utils';
import ContextMenuProvider, { resolveTarget } from '@/renderer/components/ui/ContextMenuProvider';

describe('resolveTarget', () => {
  afterEach(() => {
    globalThis.getSelection()?.removeAllRanges();
  });

  it('returns text-selection when text is selected', () => {
    const div = document.createElement('div');
    div.textContent = 'hello world';
    document.body.appendChild(div);

    const range = document.createRange();
    range.selectNodeContents(div);
    const sel = globalThis.getSelection()!;
    sel.removeAllRanges();
    sel.addRange(range);

    const result = resolveTarget(div);
    expect(result.kind).toBe('text-selection');
    if (result.kind === 'text-selection') {
      expect(result.text).toBe('hello world');
    }

    div.remove();
  });

  it('ignores text selection when it does not intersect the clicked element', () => {
    const selected = document.createElement('p');
    selected.textContent = 'selected text';
    document.body.appendChild(selected);

    const link = document.createElement('a');
    link.href = 'https://example.com';
    link.textContent = 'A link';
    document.body.appendChild(link);

    // Select text in the paragraph
    const range = document.createRange();
    range.selectNodeContents(selected);
    const sel = globalThis.getSelection()!;
    sel.removeAllRanges();
    sel.addRange(range);

    // Right-click the link — should resolve as link, not text-selection
    const result = resolveTarget(link);
    expect(result.kind).toBe('link');

    selected.remove();
    link.remove();
  });

  it('returns link for anchor elements with href', () => {
    const a = document.createElement('a');
    a.href = 'https://example.com';
    a.textContent = 'Example';
    document.body.appendChild(a);

    const result = resolveTarget(a);
    expect(result.kind).toBe('link');
    if (result.kind === 'link') {
      expect(result.href).toContain('example.com');
      expect(result.text).toBe('Example');
    }

    a.remove();
  });

  it('returns image for img elements', () => {
    const img = document.createElement('img');
    img.src = 'https://example.com/photo.jpg';
    document.body.appendChild(img);

    const result = resolveTarget(img);
    expect(result.kind).toBe('image');
    if (result.kind === 'image') {
      expect(result.src).toContain('photo.jpg');
    }

    img.remove();
  });

  it('returns text-input for input elements', () => {
    const input = document.createElement('input');
    input.type = 'text';
    document.body.appendChild(input);

    const result = resolveTarget(input);
    expect(result.kind).toBe('text-input');

    input.remove();
  });

  it('returns text-input for textarea elements', () => {
    const textarea = document.createElement('textarea');
    document.body.appendChild(textarea);

    const result = resolveTarget(textarea);
    expect(result.kind).toBe('text-input');

    textarea.remove();
  });

  it('returns contenteditable for contenteditable elements', () => {
    const div = document.createElement('div');
    div.setAttribute('contenteditable', 'true');
    document.body.appendChild(div);

    const result = resolveTarget(div);
    expect(result.kind).toBe('contenteditable');

    div.remove();
  });

  it('returns area when data-context-area is set', () => {
    const div = document.createElement('div');
    div.dataset.contextArea = 'chat';
    document.body.appendChild(div);

    const result = resolveTarget(div);
    expect(result.kind).toBe('area');
    if (result.kind === 'area') {
      expect(result.area).toBe('chat');
    }

    div.remove();
  });

  it('walks up DOM to find context area on parent', () => {
    const parent = document.createElement('div');
    parent.dataset.contextArea = 'members';
    const child = document.createElement('span');
    child.textContent = 'inner';
    parent.appendChild(child);
    document.body.appendChild(parent);

    const result = resolveTarget(child);
    expect(result.kind).toBe('area');
    if (result.kind === 'area') {
      expect(result.area).toBe('members');
    }

    parent.remove();
  });

  it('returns generic when no target matches', () => {
    const div = document.createElement('div');
    document.body.appendChild(div);

    const result = resolveTarget(div);
    expect(result.kind).toBe('generic');

    div.remove();
  });

  it('returns generic for null element', () => {
    expect(resolveTarget(null).kind).toBe('generic');
  });

  it('ignores non-text input types (checkbox, radio)', () => {
    const input = document.createElement('input');
    input.type = 'checkbox';
    document.body.appendChild(input);

    const result = resolveTarget(input);
    expect(result.kind).toBe('generic');

    input.remove();
  });
});

describe('ContextMenuProvider', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
    globalThis.getSelection()?.removeAllRanges();
  });

  it('renders children', () => {
    render(
      <ContextMenuProvider>
        <div data-testid="child">Hello</div>
      </ContextMenuProvider>
    );
    expect(screen.getByTestId('child')).toBeInTheDocument();
  });

  it('prevents default on contextmenu event', () => {
    render(
      <ContextMenuProvider>
        <div data-testid="target">Content</div>
      </ContextMenuProvider>
    );

    const target = screen.getByTestId('target');
    const event = new MouseEvent('contextmenu', {
      bubbles: true,
      clientX: 100,
      clientY: 200,
    });
    const preventSpy = vi.spyOn(event, 'preventDefault');
    target.dispatchEvent(event);

    expect(preventSpy).toHaveBeenCalled();
  });

  it('does not show a global menu when a component already handled contextmenu', () => {
    render(
      <ContextMenuProvider>
        <div data-context-area="members">
          <button
            type="button"
            data-testid="friend-row"
            onContextMenu={(event) => {
              event.preventDefault();
            }}
          >
            Friend
          </button>
        </div>
      </ContextMenuProvider>
    );

    fireEvent.contextMenu(screen.getByTestId('friend-row'));

    expect(screen.queryByText('Copy Server ID')).not.toBeInTheDocument();
    expect(document.querySelector('.ctx-menu')).not.toBeInTheDocument();
  });

  it('shows link context menu for anchor elements', () => {
    render(
      <ContextMenuProvider>
        <a href="https://example.com">Click me</a>
      </ContextMenuProvider>
    );

    fireEvent.contextMenu(screen.getByText('Click me'));

    expect(screen.getByText('Copy Link')).toBeInTheDocument();
    expect(screen.getByText('Open in Browser')).toBeInTheDocument();
  });

  it('shows text input context menu for input elements', () => {
    render(
      <ContextMenuProvider>
        <input type="text" data-testid="input" defaultValue="hello" />
      </ContextMenuProvider>
    );

    fireEvent.contextMenu(screen.getByTestId('input'));

    expect(screen.getByText('Cut')).toBeInTheDocument();
    expect(screen.getByText('Copy')).toBeInTheDocument();
    expect(screen.getByText('Paste')).toBeInTheDocument();
    expect(screen.getByText('Select All')).toBeInTheDocument();
  });

  it('shows area fallback for data-context-area elements', () => {
    render(
      <ContextMenuProvider>
        <div data-context-area="chat" data-testid="chat-area">
          <span>Empty space</span>
        </div>
      </ContextMenuProvider>
    );

    fireEvent.contextMenu(screen.getByText('Empty space'));

    expect(screen.getByText('Chat')).toBeInTheDocument();
    expect(screen.getByText('Mark Channel as Read')).toBeInTheDocument();
  });

  it('shows members area fallback', () => {
    render(
      <ContextMenuProvider>
        <div data-context-area="members">
          <span data-testid="members-area">Members</span>
        </div>
      </ContextMenuProvider>
    );

    fireEvent.contextMenu(screen.getByTestId('members-area'));
    expect(screen.getByText('Copy Server ID')).toBeInTheDocument();
  });

  it('shows channels area fallback', () => {
    render(
      <ContextMenuProvider>
        <div data-context-area="channels">
          <span data-testid="channels-area">Channels</span>
        </div>
      </ContextMenuProvider>
    );

    fireEvent.contextMenu(screen.getByTestId('channels-area'));
    expect(screen.getByText('Create Channel')).toBeInTheDocument();
    expect(screen.getByText('Create Category')).toBeInTheDocument();
  });

  it('shows no menu for generic targets', () => {
    const { container } = render(
      <ContextMenuProvider>
        <div data-testid="generic">Nothing here</div>
      </ContextMenuProvider>
    );

    fireEvent.contextMenu(screen.getByTestId('generic'));
    expect(container.querySelector('.ctx-menu')).not.toBeInTheDocument();
  });

  it('closes menu when Escape is pressed', () => {
    render(
      <ContextMenuProvider>
        <a href="https://example.com">Link</a>
      </ContextMenuProvider>
    );

    fireEvent.contextMenu(screen.getByText('Link'));
    expect(screen.getByText('Copy Link')).toBeInTheDocument();

    fireEvent.keyDown(document, { key: 'Escape' });
    act(() => {
      vi.advanceTimersByTime(200);
    });

    expect(screen.queryByText('Copy Link')).not.toBeInTheDocument();
  });

  it('handles Copy Link click', async () => {
    render(
      <ContextMenuProvider>
        <a href="https://example.com">Link</a>
      </ContextMenuProvider>
    );

    fireEvent.contextMenu(screen.getByText('Link'));

    await act(async () => {
      fireEvent.click(screen.getByText('Copy Link'));
    });

    expect(navigator.clipboard.writeText).toHaveBeenCalledWith(
      expect.stringContaining('example.com')
    );
  });

  it('handles Open in Browser click for links', () => {
    const openSpy = vi.spyOn(globalThis, 'open').mockImplementation(() => null);

    render(
      <ContextMenuProvider>
        <a href="https://example.com">Link</a>
      </ContextMenuProvider>
    );

    fireEvent.contextMenu(screen.getByText('Link'));
    fireEvent.click(screen.getByText('Open in Browser'));

    expect(openSpy).toHaveBeenCalledWith(expect.stringContaining('example.com'), '_blank');
    openSpy.mockRestore();
  });

  it('handles Paste in text input', async () => {
    vi.mocked(navigator.clipboard.readText).mockResolvedValueOnce('pasted text');

    render(
      <ContextMenuProvider>
        <input type="text" data-testid="input" defaultValue="" />
      </ContextMenuProvider>
    );

    fireEvent.contextMenu(screen.getByTestId('input'));

    await act(async () => {
      fireEvent.click(screen.getByText('Paste'));
    });

    expect(navigator.clipboard.readText).toHaveBeenCalled();
  });

  it('handles keyboard accessibility (Shift+F10)', () => {
    render(
      <ContextMenuProvider>
        <a href="https://example.com">Focusable Link</a>
      </ContextMenuProvider>
    );

    const link = screen.getByText('Focusable Link');
    link.focus();

    fireEvent.keyDown(document, { key: 'F10', shiftKey: true });

    expect(screen.getByText('Copy Link')).toBeInTheDocument();
    expect(screen.getByText('Open in Browser')).toBeInTheDocument();
  });

  it('handles ContextMenu key', () => {
    render(
      <ContextMenuProvider>
        <input type="text" data-testid="input" defaultValue="test" />
      </ContextMenuProvider>
    );

    screen.getByTestId('input').focus();
    fireEvent.keyDown(document, { key: 'ContextMenu' });

    expect(screen.getByText('Cut')).toBeInTheDocument();
    expect(screen.getByText('Paste')).toBeInTheDocument();
  });

  it('calls specific handler without preventing global menu', () => {
    const specificHandler = vi.fn((e: React.MouseEvent) => {
      e.preventDefault();
      e.stopPropagation();
    });

    render(
      <ContextMenuProvider>
        <div data-context-area="chat">
          <button onContextMenu={specificHandler}>Specific Target</button>
        </div>
      </ContextMenuProvider>
    );

    fireEvent.contextMenu(screen.getByText('Specific Target'));
    expect(specificHandler).toHaveBeenCalled();
  });

  it('shows image context menu for img elements', () => {
    render(
      <ContextMenuProvider>
        <img src="https://example.com/photo.jpg" alt="Test" data-testid="img" />
      </ContextMenuProvider>
    );

    fireEvent.contextMenu(screen.getByTestId('img'));

    expect(screen.getByText('Copy Image Link')).toBeInTheDocument();
    expect(screen.getByText('Open in Browser')).toBeInTheDocument();
  });

  it('shows contenteditable context menu', () => {
    const { container } = render(
      <ContextMenuProvider>
        <div data-testid="editable">Editable content</div>
      </ContextMenuProvider>
    );

    // Set contenteditable via setAttribute to ensure jsdom picks it up
    const editable = container.querySelector('[data-testid="editable"]')!;
    editable.setAttribute('contenteditable', 'true');

    fireEvent.contextMenu(editable);

    expect(screen.getByText('Copy')).toBeInTheDocument();
    expect(screen.getByText('Paste')).toBeInTheDocument();
    expect(screen.getByText('Select All')).toBeInTheDocument();
  });

  it('replaces previous menu when right-clicking a new target', () => {
    render(
      <ContextMenuProvider>
        <a href="https://example.com">Link</a>
        <input type="text" data-testid="input" defaultValue="text" />
      </ContextMenuProvider>
    );

    fireEvent.contextMenu(screen.getByText('Link'));
    expect(screen.getByText('Copy Link')).toBeInTheDocument();

    fireEvent.contextMenu(screen.getByTestId('input'));
    expect(screen.queryByText('Copy Link')).not.toBeInTheDocument();
    expect(screen.getByText('Paste')).toBeInTheDocument();
  });

  it('handles Select All in text input', () => {
    render(
      <ContextMenuProvider>
        <input type="text" data-testid="input" defaultValue="hello world" />
      </ContextMenuProvider>
    );

    fireEvent.contextMenu(screen.getByTestId('input'));
    fireEvent.click(screen.getByText('Select All'));

    // Menu closes after Select All
    expect(screen.queryByText('Select All')).not.toBeInTheDocument();
  });

  it('shows text-selection menu when text is selected', () => {
    render(
      <ContextMenuProvider>
        <p data-testid="paragraph">Some selectable text</p>
      </ContextMenuProvider>
    );

    const p = screen.getByTestId('paragraph');
    const range = document.createRange();
    range.selectNodeContents(p);
    const sel = globalThis.getSelection()!;
    sel.removeAllRanges();
    sel.addRange(range);

    fireEvent.contextMenu(p);

    expect(screen.getByText('Copy')).toBeInTheDocument();
    expect(screen.queryByText('Paste')).not.toBeInTheDocument();
  });

  it('handles Copy for text selection', async () => {
    render(
      <ContextMenuProvider>
        <p data-testid="paragraph">Selected text here</p>
      </ContextMenuProvider>
    );

    const p = screen.getByTestId('paragraph');
    const range = document.createRange();
    range.selectNodeContents(p);
    const sel = globalThis.getSelection()!;
    sel.removeAllRanges();
    sel.addRange(range);

    fireEvent.contextMenu(p);

    await act(async () => {
      fireEvent.click(screen.getByText('Copy'));
    });

    expect(navigator.clipboard.writeText).toHaveBeenCalledWith('Selected text here');
  });

  it('handles Cut in text input', async () => {
    render(
      <ContextMenuProvider>
        <input type="text" data-testid="input" defaultValue="hello world" />
      </ContextMenuProvider>
    );

    const input = screen.getByTestId('input') as HTMLInputElement;
    // Select "hello"
    input.setSelectionRange(0, 5);
    fireEvent.contextMenu(input);

    await act(async () => {
      fireEvent.click(screen.getByText('Cut'));
    });

    expect(navigator.clipboard.writeText).toHaveBeenCalledWith('hello');
  });

  it('handles Copy in text input with selection', async () => {
    render(
      <ContextMenuProvider>
        <input type="text" data-testid="input" defaultValue="hello world" />
      </ContextMenuProvider>
    );

    const input = screen.getByTestId('input') as HTMLInputElement;
    input.setSelectionRange(0, 5);
    fireEvent.contextMenu(input);

    await act(async () => {
      fireEvent.click(screen.getByText('Copy'));
    });

    expect(navigator.clipboard.writeText).toHaveBeenCalledWith('hello');
  });

  it('handles Paste in contenteditable', async () => {
    vi.mocked(navigator.clipboard.readText).mockResolvedValueOnce('pasted');

    const { container } = render(
      <ContextMenuProvider>
        <div data-testid="editable">Editable</div>
      </ContextMenuProvider>
    );

    const editable = container.querySelector('[data-testid="editable"]')!;
    editable.setAttribute('contenteditable', 'true');
    fireEvent.contextMenu(editable);

    await act(async () => {
      fireEvent.click(screen.getByText('Paste'));
    });

    expect(navigator.clipboard.readText).toHaveBeenCalled();
  });

  it('handles Select All in contenteditable', () => {
    const { container } = render(
      <ContextMenuProvider>
        <div data-testid="editable">Editable</div>
      </ContextMenuProvider>
    );

    const editable = container.querySelector('[data-testid="editable"]')!;
    editable.setAttribute('contenteditable', 'true');
    fireEvent.contextMenu(editable);

    fireEvent.click(screen.getByText('Select All'));
    // Menu closes
    expect(screen.queryByText('Select All')).not.toBeInTheDocument();
  });

  it('handles Copy in contenteditable with selection', async () => {
    const { container } = render(
      <ContextMenuProvider>
        <div data-testid="editable">Editable text</div>
      </ContextMenuProvider>
    );

    const editable = container.querySelector('[data-testid="editable"]')!;
    editable.setAttribute('contenteditable', 'true');

    // Select the text
    const range = document.createRange();
    range.selectNodeContents(editable);
    const sel = globalThis.getSelection()!;
    sel.removeAllRanges();
    sel.addRange(range);

    fireEvent.contextMenu(editable);

    await act(async () => {
      fireEvent.click(screen.getByText('Copy'));
    });

    expect(navigator.clipboard.writeText).toHaveBeenCalled();
  });

  it('handles Copy in contenteditable without selection', async () => {
    const { container } = render(
      <ContextMenuProvider>
        <div data-testid="editable">Text</div>
      </ContextMenuProvider>
    );

    const editable = container.querySelector('[data-testid="editable"]')!;
    editable.setAttribute('contenteditable', 'true');

    // No selection
    globalThis.getSelection()?.removeAllRanges();
    fireEvent.contextMenu(editable);

    await act(async () => {
      fireEvent.click(screen.getByText('Copy'));
    });

    // Should call onClose since no selection, not clipboard write
    expect(navigator.clipboard.writeText).not.toHaveBeenCalled();
  });

  it('shows servers area fallback', () => {
    render(
      <ContextMenuProvider>
        <div data-context-area="servers">
          <span data-testid="servers-area">Servers</span>
        </div>
      </ContextMenuProvider>
    );

    fireEvent.contextMenu(screen.getByTestId('servers-area'));
    // Header renders "Servers" — verify the context menu appeared via the header element
    expect(screen.getAllByText('Servers')).toHaveLength(2); // span + menu header
  });

  it('shows empty fallback for unknown area', () => {
    const { container } = render(
      <ContextMenuProvider>
        <div data-context-area="unknown">
          <span data-testid="unknown-area">Unknown</span>
        </div>
      </ContextMenuProvider>
    );

    fireEvent.contextMenu(screen.getByTestId('unknown-area'));
    // Menu still renders (area case), but default areaItems returns null
    expect(container.querySelector('.ctx-menu')).toBeInTheDocument();
  });
});
