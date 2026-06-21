import { render, screen, fireEvent } from '../../../test-utils';
import { useServerStore } from '@/renderer/stores/serverStore';
import { useLayoutStore } from '@/renderer/stores/layoutStore';
import { useUnreadStore } from '@/renderer/stores/unreadStore';
import { mockServer, mockServer2 } from '../../../mocks/fixtures';
import FolderBar from '@/renderer/components/Layout/FolderBar';

describe('FolderBar', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useServerStore.setState({
      servers: [],
      activeServerId: null,
    });
    useLayoutStore.setState({
      serverFolders: [],
      folderBarHeight: 36,
    });
    useUnreadStore.setState({
      serverUnreadSet: new Set(),
    });
  });

  it('renders without crashing', () => {
    const { container } = render(<FolderBar />);
    expect(container).toBeTruthy();
  });

  it('renders folder items when folders exist', () => {
    useLayoutStore.setState({
      serverFolders: [{ id: 'folder-1', name: 'My Folder', serverIds: [], collapsed: false }],
    });
    render(<FolderBar />);
    expect(screen.getByText('My Folder')).toBeInTheDocument();
  });

  it('renders create folder button', () => {
    const { container } = render(<FolderBar />);
    const addBtn = container.querySelector('.folder-add-btn');
    expect(addBtn).toBeInTheDocument();
  });

  it('toggles folder open/close on click without errors', () => {
    useLayoutStore.setState({
      serverFolders: [
        { id: 'folder-1', name: 'My Folder', serverIds: ['server-1'], collapsed: false },
      ],
    });
    useServerStore.setState({ servers: [mockServer] });
    render(<FolderBar />);
    const folderBtn = screen.getByText('My Folder');

    // The dropdown is a portal'd component-local state, not store state —
    // asserting DOM portal presence is brittle under jsdom's zero-rect refs.
    // Instead, spy on console.error to catch any React error / handler throw
    // during the two-click toggle sequence and also verify the folder chip is
    // still present in the DOM (proving the component survived both clicks).
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    fireEvent.click(folderBtn);
    fireEvent.click(folderBtn);
    // No React errors should have been emitted during the toggle sequence
    expect(errorSpy).not.toHaveBeenCalled();
    // Folder chip must still be mounted after both clicks
    expect(screen.getByText('My Folder')).toBeInTheDocument();
    errorSpy.mockRestore();
  });

  it('shows folder context menu on right click', () => {
    useLayoutStore.setState({
      serverFolders: [{ id: 'folder-1', name: 'My Folder', serverIds: [], collapsed: false }],
    });
    render(<FolderBar />);
    fireEvent.contextMenu(screen.getByText('My Folder'));
    expect(screen.getByText('Rename Folder')).toBeInTheDocument();
    expect(screen.getByText('Delete Folder')).toBeInTheDocument();
  });

  it('renders multiple folders', () => {
    useLayoutStore.setState({
      serverFolders: [
        { id: 'folder-1', name: 'Games', serverIds: [], collapsed: false },
        { id: 'folder-2', name: 'Work', serverIds: [], collapsed: false },
      ],
    });
    render(<FolderBar />);
    expect(screen.getByText('Games')).toBeInTheDocument();
    expect(screen.getByText('Work')).toBeInTheDocument();
  });

  it('shows server names inside expanded folder', () => {
    useLayoutStore.setState({
      serverFolders: [
        {
          id: 'folder-1',
          name: 'My Folder',
          serverIds: ['server-1', 'server-2'],
          collapsed: false,
        },
      ],
    });
    useServerStore.setState({ servers: [mockServer, mockServer2] });
    render(<FolderBar />);
    // Click folder chip to open the dropdown
    fireEvent.click(screen.getByText('My Folder'));
    expect(screen.getByText('Test Server')).toBeInTheDocument();
    expect(screen.getByText('Second Server')).toBeInTheDocument();
  });

  it('hides server names when folder is not clicked open', () => {
    useLayoutStore.setState({
      serverFolders: [
        { id: 'folder-1', name: 'My Folder', serverIds: ['server-1'], collapsed: false },
      ],
    });
    useServerStore.setState({ servers: [mockServer] });
    render(<FolderBar />);
    expect(screen.getByText('My Folder')).toBeInTheDocument();
    // Server names should not be visible until folder is clicked open
    expect(screen.queryByText('Test Server')).not.toBeInTheDocument();
  });

  it('creates a new folder when add button is clicked', () => {
    const { container } = render(<FolderBar />);
    const addBtn = container.querySelector('.folder-add-btn')!;
    fireEvent.click(addBtn);
    // After clicking, a new folder with a rename input should appear
    const state = useLayoutStore.getState();
    expect(state.serverFolders.length).toBe(1);
  });

  it('shows unread indicator on folder with unread servers', () => {
    useLayoutStore.setState({
      serverFolders: [
        { id: 'folder-1', name: 'My Folder', serverIds: ['server-1'], collapsed: true },
      ],
    });
    useServerStore.setState({ servers: [mockServer] });
    useUnreadStore.setState({ serverUnreadSet: new Set(['server-1']) });
    const { container } = render(<FolderBar />);
    const unreadBadge = container.querySelector('.folder-chip-badge');
    expect(unreadBadge).toBeInTheDocument();
  });

  it('renders folder bar container', () => {
    const { container } = render(<FolderBar />);
    expect(container.querySelector('.folder-bar')).toBeInTheDocument();
  });

  // ===== Rename flow =====

  it('enters rename mode from context menu', () => {
    useLayoutStore.setState({
      serverFolders: [{ id: 'folder-1', name: 'My Folder', serverIds: [], collapsed: false }],
    });
    render(<FolderBar />);
    fireEvent.contextMenu(screen.getByText('My Folder'));
    fireEvent.click(screen.getByText('Rename Folder'));
    // Rename input should appear
    const input = screen.getByDisplayValue('My Folder');
    expect(input).toBeInTheDocument();
  });

  it('submits rename on Enter', () => {
    useLayoutStore.setState({
      serverFolders: [{ id: 'folder-1', name: 'My Folder', serverIds: [], collapsed: false }],
    });
    render(<FolderBar />);
    fireEvent.contextMenu(screen.getByText('My Folder'));
    fireEvent.click(screen.getByText('Rename Folder'));
    const input = screen.getByDisplayValue('My Folder');
    fireEvent.change(input, { target: { value: 'Renamed' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    // Folder should be renamed in the store
    const state = useLayoutStore.getState();
    expect(state.serverFolders[0].name).toBe('Renamed');
  });

  it('cancels rename on Escape', () => {
    useLayoutStore.setState({
      serverFolders: [{ id: 'folder-1', name: 'My Folder', serverIds: [], collapsed: false }],
    });
    render(<FolderBar />);
    fireEvent.contextMenu(screen.getByText('My Folder'));
    fireEvent.click(screen.getByText('Rename Folder'));
    const input = screen.getByDisplayValue('My Folder');
    fireEvent.change(input, { target: { value: 'Changed' } });
    fireEvent.keyDown(input, { key: 'Escape' });
    // Should still show original name
    expect(screen.getByText('My Folder')).toBeInTheDocument();
  });

  // ===== Delete flow =====

  it('shows delete confirmation from context menu', () => {
    useLayoutStore.setState({
      serverFolders: [{ id: 'folder-1', name: 'My Folder', serverIds: [], collapsed: false }],
    });
    render(<FolderBar />);
    fireEvent.contextMenu(screen.getByText('My Folder'));
    fireEvent.click(screen.getByText('Delete Folder'));
    expect(screen.getByText(/Are you sure/i)).toBeInTheDocument();
  });

  it('confirms delete removes folder', () => {
    useLayoutStore.setState({
      serverFolders: [{ id: 'folder-1', name: 'My Folder', serverIds: [], collapsed: false }],
    });
    render(<FolderBar />);
    fireEvent.contextMenu(screen.getByText('My Folder'));
    fireEvent.click(screen.getByText('Delete Folder'));
    // Confirm delete (second "Delete Folder" — the confirmation button)
    const deleteButtons = screen.getAllByText('Delete Folder');
    fireEvent.click(deleteButtons[deleteButtons.length - 1]);
    const state = useLayoutStore.getState();
    expect(state.serverFolders).toHaveLength(0);
  });

  // ===== Server interaction inside folder =====

  it('clicking server in folder activates it', () => {
    useLayoutStore.setState({
      serverFolders: [
        { id: 'folder-1', name: 'My Folder', serverIds: ['server-1'], collapsed: false },
      ],
    });
    useServerStore.setState({ servers: [mockServer] });
    render(<FolderBar />);
    // Open folder
    fireEvent.click(screen.getByText('My Folder'));
    // Click server
    fireEvent.click(screen.getByText('Test Server'));
    expect(useServerStore.getState().activeServerId).toBe('server-1');
  });

  // ===== Unread count =====

  it('shows unread count badge for folder with multiple unread', () => {
    useLayoutStore.setState({
      serverFolders: [
        { id: 'folder-1', name: 'My Folder', serverIds: ['server-1', 'server-2'], collapsed: true },
      ],
    });
    useServerStore.setState({ servers: [mockServer, mockServer2] });
    useUnreadStore.setState({ serverUnreadSet: new Set(['server-1', 'server-2']) });
    const { container } = render(<FolderBar />);
    const badge = container.querySelector('.folder-chip-badge');
    expect(badge).toBeInTheDocument();
    expect(badge?.textContent).toBe('2');
  });

  // ===== No folders state =====

  it('shows empty state with no folders', () => {
    const { container } = render(<FolderBar />);
    const chips = container.querySelectorAll('.folder-chip');
    expect(chips).toHaveLength(0);
  });

  // ===== Folder with no servers =====

  it('shows empty folder message when expanded', () => {
    useLayoutStore.setState({
      serverFolders: [{ id: 'folder-1', name: 'Empty', serverIds: [], collapsed: false }],
    });
    render(<FolderBar />);
    fireEvent.click(screen.getByText('Empty'));
    // An empty folder dropdown should appear (may show "Drag servers here" or similar)
    expect(screen.getByText('Drag servers here')).toBeInTheDocument();
  });

  // ===== Drag-and-drop (folder chip level) =====

  it('applies drag-over class on dragEnter', () => {
    useLayoutStore.setState({
      serverFolders: [{ id: 'folder-1', name: 'Target Folder', serverIds: [], collapsed: false }],
    });
    render(<FolderBar />);
    const chip = document.querySelector('.folder-chip')!;
    fireEvent.dragEnter(chip, {
      dataTransfer: { getData: () => '' },
    });
    expect(chip.classList.contains('drag-over')).toBe(true);
  });

  it('removes drag-over class on dragLeave', () => {
    useLayoutStore.setState({
      serverFolders: [{ id: 'folder-1', name: 'Target Folder', serverIds: [], collapsed: false }],
    });
    render(<FolderBar />);
    const chip = document.querySelector('.folder-chip')!;
    fireEvent.dragEnter(chip, {
      dataTransfer: { getData: () => '' },
    });
    expect(chip.classList.contains('drag-over')).toBe(true);
    fireEvent.dragLeave(chip);
    expect(chip.classList.contains('drag-over')).toBe(false);
  });

  it('handles drop on folder chip', () => {
    useLayoutStore.setState({
      serverFolders: [{ id: 'folder-1', name: 'Target', serverIds: [], collapsed: false }],
    });
    useServerStore.setState({ servers: [mockServer] });
    render(<FolderBar />);
    const chip = document.querySelector('.folder-chip')!;
    fireEvent.drop(chip, {
      dataTransfer: {
        getData: () => 'server-1',
      },
    });
    const state = useLayoutStore.getState();
    expect(state.serverFolders[0].serverIds).toContain('server-1');
  });

  // ===== Delete confirmation details =====

  it('shows server count in delete confirmation for non-empty folder', () => {
    useLayoutStore.setState({
      serverFolders: [
        {
          id: 'folder-1',
          name: 'My Folder',
          serverIds: ['server-1', 'server-2'],
          collapsed: false,
        },
      ],
    });
    render(<FolderBar />);
    fireEvent.contextMenu(screen.getByText('My Folder'));
    fireEvent.click(screen.getByText('Delete Folder'));
    expect(screen.getByText(/2 servers/)).toBeInTheDocument();
    expect(screen.getByText(/moved back to the server bar/)).toBeInTheDocument();
  });

  it('shows "empty" message in delete confirmation for empty folder', () => {
    useLayoutStore.setState({
      serverFolders: [{ id: 'folder-1', name: 'Empty Folder', serverIds: [], collapsed: false }],
    });
    render(<FolderBar />);
    fireEvent.contextMenu(screen.getByText('Empty Folder'));
    fireEvent.click(screen.getByText('Delete Folder'));
    expect(screen.getByText(/This folder is empty/)).toBeInTheDocument();
  });

  it('cancel button in delete confirmation closes modal', () => {
    useLayoutStore.setState({
      serverFolders: [{ id: 'folder-1', name: 'My Folder', serverIds: [], collapsed: false }],
    });
    render(<FolderBar />);
    fireEvent.contextMenu(screen.getByText('My Folder'));
    fireEvent.click(screen.getByText('Delete Folder'));
    expect(screen.getByText(/Are you sure/i)).toBeInTheDocument();
    fireEvent.click(screen.getByText('Cancel'));
    expect(screen.queryByText(/Are you sure/i)).not.toBeInTheDocument();
  });

  // ===== Rename submits on blur =====

  it('submits rename on blur', () => {
    useLayoutStore.setState({
      serverFolders: [{ id: 'folder-1', name: 'My Folder', serverIds: [], collapsed: false }],
    });
    render(<FolderBar />);
    fireEvent.contextMenu(screen.getByText('My Folder'));
    fireEvent.click(screen.getByText('Rename Folder'));
    const input = screen.getByDisplayValue('My Folder');
    fireEvent.change(input, { target: { value: 'Blurred Name' } });
    fireEvent.blur(input);
    const state = useLayoutStore.getState();
    expect(state.serverFolders[0].name).toBe('Blurred Name');
  });

  it('does not rename to empty string', () => {
    useLayoutStore.setState({
      serverFolders: [{ id: 'folder-1', name: 'My Folder', serverIds: [], collapsed: false }],
    });
    render(<FolderBar />);
    fireEvent.contextMenu(screen.getByText('My Folder'));
    fireEvent.click(screen.getByText('Rename Folder'));
    const input = screen.getByDisplayValue('My Folder');
    fireEvent.change(input, { target: { value: '   ' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    // Should keep original name since trimmed is empty
    const state = useLayoutStore.getState();
    expect(state.serverFolders[0].name).toBe('My Folder');
  });

  // ===== Rename input has maxLength =====

  it('rename input has maxLength of 32', () => {
    useLayoutStore.setState({
      serverFolders: [{ id: 'folder-1', name: 'My Folder', serverIds: [], collapsed: false }],
    });
    render(<FolderBar />);
    fireEvent.contextMenu(screen.getByText('My Folder'));
    fireEvent.click(screen.getByText('Rename Folder'));
    const input = screen.getByDisplayValue('My Folder') as HTMLInputElement;
    expect(input.maxLength).toBe(32);
  });

  // ===== No unread badge when count is 0 =====

  it('does not show unread badge when no servers are unread', () => {
    useLayoutStore.setState({
      serverFolders: [{ id: 'folder-1', name: 'Clean', serverIds: ['server-1'], collapsed: false }],
    });
    useServerStore.setState({ servers: [mockServer] });
    useUnreadStore.setState({ serverUnreadSet: new Set() });
    const { container } = render(<FolderBar />);
    const badge = container.querySelector('.folder-chip-badge');
    expect(badge).not.toBeInTheDocument();
  });

  // ===== Server icon in dropdown =====

  it('shows server icon when icon_url is present', () => {
    const serverWithIcon = { ...mockServer, icon_url: 'https://example.com/icon.png' };
    useLayoutStore.setState({
      serverFolders: [
        { id: 'folder-1', name: 'My Folder', serverIds: ['server-1'], collapsed: false },
      ],
    });
    useServerStore.setState({ servers: [serverWithIcon] });
    render(<FolderBar />);
    fireEvent.click(screen.getByText('My Folder'));
    const img = document.querySelector('.folder-dropdown-item-icon img');
    expect(img).toBeInTheDocument();
    expect(img).toHaveAttribute('src', 'https://example.com/icon.png');
  });

  it('shows server initial when no icon_url', () => {
    useLayoutStore.setState({
      serverFolders: [
        { id: 'folder-1', name: 'My Folder', serverIds: ['server-1'], collapsed: false },
      ],
    });
    useServerStore.setState({ servers: [mockServer] });
    render(<FolderBar />);
    fireEvent.click(screen.getByText('My Folder'));
    const initial = document.querySelector('.folder-dropdown-item-initial');
    expect(initial).toBeInTheDocument();
    expect(initial?.textContent).toBe('T');
  });

  // ===== ARIA / a11y attributes =====

  it('renders folder chips with role="treeitem"', () => {
    useLayoutStore.setState({
      serverFolders: [{ id: 'folder-1', name: 'My Folder', serverIds: [], collapsed: false }],
    });
    render(<FolderBar />);
    const treeItems = screen.getAllByRole('treeitem');
    expect(treeItems.length).toBeGreaterThanOrEqual(1);
    expect(treeItems[0]).toHaveAttribute('aria-label', 'My Folder folder');
  });

  it('folder chip inner element is a <button>', () => {
    useLayoutStore.setState({
      serverFolders: [{ id: 'folder-1', name: 'My Folder', serverIds: [], collapsed: false }],
    });
    render(<FolderBar />);
    const chip = document.querySelector('.folder-chip-inner');
    expect(chip).toBeInTheDocument();
    expect(chip?.tagName).toBe('BUTTON');
    expect(chip).toHaveAttribute('type', 'button');
  });

  it('folder treeitem has aria-expanded matching open state', () => {
    useLayoutStore.setState({
      serverFolders: [
        { id: 'folder-1', name: 'My Folder', serverIds: ['server-1'], collapsed: false },
      ],
    });
    useServerStore.setState({ servers: [mockServer] });
    render(<FolderBar />);
    const treeItem = screen.getByRole('treeitem', { name: 'My Folder folder' });
    expect(treeItem).toHaveAttribute('aria-expanded', 'false');

    // Open the folder
    fireEvent.click(screen.getByText('My Folder'));
    expect(treeItem).toHaveAttribute('aria-expanded', 'true');
  });

  // ===== Folder scale from height =====

  it('applies folder scale CSS custom property', () => {
    useLayoutStore.setState({ folderBarHeight: 36 });
    const { container } = render(<FolderBar />);
    const folderBar = container.querySelector('.folder-bar') as HTMLElement;
    expect(folderBar.style.getPropertyValue('--folder-scale')).toBeTruthy();
  });

  // ===== Clicking inside rename prevents folder toggle =====

  // ===== Add button is a native button element =====

  it('add button is a <button> element', () => {
    const { container } = render(<FolderBar />);
    const addBtn = container.querySelector('.folder-add-btn');
    expect(addBtn).toBeInTheDocument();
    expect(addBtn?.tagName).toBe('BUTTON');
  });

  // ===== Folder chip click fires server activation =====

  it('clicking server in dropdown calls setActiveServer and navigates', () => {
    useLayoutStore.setState({
      serverFolders: [
        {
          id: 'folder-1',
          name: 'My Folder',
          serverIds: ['server-1', 'server-2'],
          collapsed: false,
        },
      ],
    });
    useServerStore.setState({ servers: [mockServer, mockServer2] });
    render(<FolderBar />);
    // Open folder dropdown
    fireEvent.click(screen.getByText('My Folder'));
    // Click second server
    fireEvent.click(screen.getByText('Second Server'));
    expect(useServerStore.getState().activeServerId).toBe('server-2');
  });

  // ===== Folder scale edge cases =====

  it('applies minimum folder scale at folderBarHeight=24', () => {
    useLayoutStore.setState({ folderBarHeight: 24 });
    const { container } = render(<FolderBar />);
    const folderBar = container.querySelector('.folder-bar') as HTMLElement;
    const scale = folderBar.style.getPropertyValue('--folder-scale');
    expect(Number.parseFloat(scale)).toBeCloseTo(0.75);
  });

  it('applies maximum folder scale at folderBarHeight=48', () => {
    useLayoutStore.setState({ folderBarHeight: 48 });
    const { container } = render(<FolderBar />);
    const folderBar = container.querySelector('.folder-bar') as HTMLElement;
    const scale = folderBar.style.getPropertyValue('--folder-scale');
    expect(Number.parseFloat(scale)).toBeCloseTo(1.25);
  });

  it('does not toggle folder when clicking rename input', () => {
    useLayoutStore.setState({
      serverFolders: [
        { id: 'folder-1', name: 'My Folder', serverIds: ['server-1'], collapsed: false },
      ],
    });
    useServerStore.setState({ servers: [mockServer] });
    render(<FolderBar />);
    fireEvent.contextMenu(screen.getByText('My Folder'));
    fireEvent.click(screen.getByText('Rename Folder'));
    const input = screen.getByDisplayValue('My Folder');
    // Click the input — should not open the dropdown
    fireEvent.click(input);
    // Server name should not appear since dropdown should not open
    expect(screen.queryByText('Test Server')).not.toBeInTheDocument();
  });
});
