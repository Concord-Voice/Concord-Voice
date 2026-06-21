import { render, screen, fireEvent } from '../../../test-utils';
import ChannelListContextMenu from '@/renderer/components/Channels/ChannelListContextMenu';

describe('ChannelListContextMenu', () => {
  const mockOnClose = vi.fn();
  const mockOnCreateChannel = vi.fn();
  const mockOnCreateCategory = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  const renderMenu = () =>
    render(
      <ChannelListContextMenu
        position={{ x: 100, y: 150 }}
        onClose={mockOnClose}
        onCreateChannel={mockOnCreateChannel}
        onCreateCategory={mockOnCreateCategory}
      />
    );

  it('renders Create Channel item', () => {
    renderMenu();
    expect(screen.getByText('Create Channel')).toBeInTheDocument();
  });

  it('renders Create Category item', () => {
    renderMenu();
    expect(screen.getByText('Create Category')).toBeInTheDocument();
  });

  it('calls onCreateChannel and onClose when Create Channel is clicked', () => {
    renderMenu();
    fireEvent.click(screen.getByText('Create Channel'));
    expect(mockOnCreateChannel).toHaveBeenCalled();
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('calls onCreateCategory and onClose when Create Category is clicked', () => {
    renderMenu();
    fireEvent.click(screen.getByText('Create Category'));
    expect(mockOnCreateCategory).toHaveBeenCalled();
    expect(mockOnClose).toHaveBeenCalled();
  });
});
