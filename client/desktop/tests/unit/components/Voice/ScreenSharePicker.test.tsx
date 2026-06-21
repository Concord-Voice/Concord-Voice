import React from 'react';
import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { useVideoSettingsStore } from '@/renderer/stores/videoSettingsStore';
import { resetAllStores } from '../../../helpers/store-helpers';

vi.mock('@/renderer/components/Voice/ScreenSharePicker.css', () => ({}));

// Mock CustomSelect to simplify testing
vi.mock('@/renderer/components/ui/CustomSelect', () => ({
  default: ({
    value,
    onChange,
    options,
    id,
  }: {
    value: string;
    onChange: (v: string) => void;
    options: { value: string; label: string }[];
    id?: string;
  }) => (
    <select data-testid={id} value={value} onChange={(e) => onChange(e.target.value)}>
      {options.map((o) => (
        <option key={o.value} value={o.value}>
          {o.label}
        </option>
      ))}
    </select>
  ),
}));

const mockSources = [
  { id: 'screen:0', name: 'Entire Screen', thumbnail: 'thumb1', appIcon: null },
  { id: 'window:1', name: 'VS Code', thumbnail: 'thumb2', appIcon: 'icon1' },
  { id: 'window:2', name: 'Chrome', thumbnail: 'thumb3', appIcon: null },
];

import ScreenSharePicker from '@/renderer/components/Voice/ScreenSharePicker';

describe('ScreenSharePicker', () => {
  const mockOnSelect = vi.fn();
  const mockOnCancel = vi.fn();

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();

    // Mock electron.getDesktopSources — electron is already writable from setup.ts
    (globalThis as Record<string, unknown>).electron = {
      ...(globalThis.electron || {}),
      getDesktopSources: vi.fn().mockResolvedValue(mockSources),
    };
  });

  it('renders loading state initially', () => {
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    expect(screen.getByText('Loading sources...')).toBeInTheDocument();
  });

  it('renders screens and windows after loading', async () => {
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByText('Screens')).toBeInTheDocument();
      expect(screen.getByText('Windows')).toBeInTheDocument();
    });
    expect(screen.getByText('Entire Screen')).toBeInTheDocument();
    expect(screen.getByText('VS Code')).toBeInTheDocument();
    expect(screen.getByText('Chrome')).toBeInTheDocument();
  });

  it('renders title with Share Your Screen', async () => {
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByText('Share Your Screen')).toBeInTheDocument();
    });
  });

  it('selects a source on click', async () => {
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByText('Entire Screen')).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText('Entire Screen'));
    // Share button should now be enabled
    const shareBtn = screen.getByText('Share');
    expect(shareBtn).not.toBeDisabled();
  });

  it('Share button is disabled when no source is selected', async () => {
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByText('Share')).toBeInTheDocument();
    });
    expect(screen.getByText('Share')).toBeDisabled();
  });

  it('calls onSelect with source ID and options on confirm', async () => {
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByText('Entire Screen')).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText('Entire Screen'));
    fireEvent.click(screen.getByText('Share'));
    expect(mockOnSelect).toHaveBeenCalledWith('screen:0', {
      resolution: 'source',
      frameRate: 30,
      contentType: 'auto',
    });
  });

  it('calls onCancel when Cancel button is clicked', async () => {
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByText('Cancel')).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText('Cancel'));
    expect(mockOnCancel).toHaveBeenCalled();
  });

  it('calls onCancel when close button is clicked', async () => {
    const { container } = render(
      <ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />
    );
    await waitFor(() => {
      expect(screen.getByText('Share Your Screen')).toBeInTheDocument();
    });
    const closeBtn = container.querySelector('.screen-picker__close');
    expect(closeBtn).toBeInTheDocument();
    fireEvent.click(closeBtn!);
    expect(mockOnCancel).toHaveBeenCalled();
  });

  it('calls onCancel when Escape key is pressed', async () => {
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByText('Share Your Screen')).toBeInTheDocument();
    });
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(mockOnCancel).toHaveBeenCalled();
  });

  it('calls onCancel when overlay background is clicked', async () => {
    const { container } = render(
      <ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />
    );
    await waitFor(() => {
      expect(screen.getByText('Share Your Screen')).toBeInTheDocument();
    });
    const overlay = container.querySelector('.screen-picker-overlay');
    fireEvent.click(overlay!);
    expect(mockOnCancel).toHaveBeenCalled();
  });

  it('reads default settings from video settings store', async () => {
    useVideoSettingsStore.setState({
      screenResolution: '1080p',
      screenFrameRate: 60,
      screenContentType: 'motion',
    });

    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByText('Entire Screen')).toBeInTheDocument();
    });

    // Select a source and confirm to verify options use store defaults
    fireEvent.click(screen.getByText('Entire Screen'));
    fireEvent.click(screen.getByText('Share'));
    expect(mockOnSelect).toHaveBeenCalledWith('screen:0', {
      resolution: '1080p',
      frameRate: 60,
      contentType: 'motion',
    });
  });

  it('changes local resolution when user selects from dropdown', async () => {
    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    await waitFor(() => {
      expect(screen.getByText('Entire Screen')).toBeInTheDocument();
    });

    const resolutionSelect = screen.getByTestId('screen-resolution');
    fireEvent.change(resolutionSelect, { target: { value: '720p' } });

    fireEvent.click(screen.getByText('Entire Screen'));
    fireEvent.click(screen.getByText('Share'));
    expect(mockOnSelect).toHaveBeenCalledWith(
      'screen:0',
      expect.objectContaining({
        resolution: '720p',
      })
    );
  });

  it('handles missing electron.getDesktopSources gracefully', async () => {
    (globalThis as Record<string, unknown>).electron = {};

    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);
    // Should stop loading without crashing
    await waitFor(() => {
      expect(screen.queryByText('Loading sources...')).not.toBeInTheDocument();
    });
  });

  it('logs error when getDesktopSources throws', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    (globalThis as Record<string, unknown>).electron = {
      getDesktopSources: vi.fn().mockRejectedValue(new Error('IPC error')),
    };

    render(<ScreenSharePicker onSelect={mockOnSelect} onCancel={mockOnCancel} />);

    await waitFor(() => {
      expect(consoleSpy).toHaveBeenCalledWith('Failed to get desktop sources:', 'IPC error');
    });
    // Loading ends even on error
    expect(screen.queryByText('Loading sources...')).not.toBeInTheDocument();
    consoleSpy.mockRestore();
  });
});
