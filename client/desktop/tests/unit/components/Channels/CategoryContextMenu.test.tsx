import { render, screen, fireEvent } from '../../../test-utils';
import CategoryContextMenu from '@/renderer/components/Channels/CategoryContextMenu';
import type { ChannelGroup } from '@/renderer/types/chat';

const mockGroup: ChannelGroup = {
  id: 'group-1',
  server_id: 'server-1',
  name: 'Voice Channels',
  position: 0,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

describe('CategoryContextMenu', () => {
  const mockOnClose = vi.fn();
  const mockOnEditCategory = vi.fn();
  const mockOnDeleteCategory = vi.fn();
  const mockOnCategoryPermissions = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  const renderMenu = (overrides?: { onCategoryPermissions?: (group: ChannelGroup) => void }) =>
    render(
      <CategoryContextMenu
        group={mockGroup}
        position={{ x: 200, y: 300 }}
        onClose={mockOnClose}
        onEditCategory={mockOnEditCategory}
        onDeleteCategory={mockOnDeleteCategory}
        onCategoryPermissions={overrides?.onCategoryPermissions ?? mockOnCategoryPermissions}
      />
    );

  it('renders category name in header', () => {
    renderMenu();
    expect(screen.getByText('Voice Channels')).toBeInTheDocument();
  });

  it('renders Category Permissions item', () => {
    renderMenu();
    expect(screen.getByText('Category Permissions')).toBeInTheDocument();
  });

  it('renders Edit Category item', () => {
    renderMenu();
    expect(screen.getByText('Edit Category')).toBeInTheDocument();
  });

  it('renders Delete Category item', () => {
    renderMenu();
    expect(screen.getByText('Delete Category')).toBeInTheDocument();
  });

  it('calls onCategoryPermissions and onClose when Category Permissions is clicked', () => {
    renderMenu();
    fireEvent.click(screen.getByText('Category Permissions'));
    expect(mockOnClose).toHaveBeenCalled();
    expect(mockOnCategoryPermissions).toHaveBeenCalledWith(mockGroup);
  });

  it('calls onEditCategory and onClose when Edit Category is clicked', () => {
    renderMenu();
    fireEvent.click(screen.getByText('Edit Category'));
    expect(mockOnEditCategory).toHaveBeenCalledWith(mockGroup);
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('calls onDeleteCategory and onClose when Delete Category is clicked', () => {
    renderMenu();
    fireEvent.click(screen.getByText('Delete Category'));
    expect(mockOnDeleteCategory).toHaveBeenCalledWith(mockGroup);
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('does not crash when onCategoryPermissions is undefined', () => {
    render(
      <CategoryContextMenu
        group={mockGroup}
        position={{ x: 200, y: 300 }}
        onClose={mockOnClose}
        onEditCategory={mockOnEditCategory}
        onDeleteCategory={mockOnDeleteCategory}
      />
    );
    // Should not throw when clicking
    fireEvent.click(screen.getByText('Category Permissions'));
    expect(mockOnClose).toHaveBeenCalled();
  });
});
