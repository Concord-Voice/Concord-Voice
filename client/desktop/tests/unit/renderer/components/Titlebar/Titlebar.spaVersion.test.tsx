import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';

vi.mock('@/renderer/config', () => ({
  SPA_VERSION: 'a'.repeat(40),
}));

import { Titlebar } from '@/renderer/components/Titlebar/Titlebar';

const mockGet = vi.fn();
const mockOnChange = vi.fn();

beforeEach(() => {
  vi.clearAllMocks();
  (window as unknown as { electron: unknown }).electron = {
    version: {
      get: mockGet,
      onChange: mockOnChange,
    },
  };
});

describe('Titlebar SPA version display', () => {
  it('uses the renderer build SPA version when the main flat-host provider has no URL hash', async () => {
    mockGet.mockResolvedValue({ appVersion: '0.2.20', spaHash: null });
    mockOnChange.mockReturnValue(() => {});

    render(<Titlebar />);

    await waitFor(() => {
      expect(screen.getByText('v0.2.20-aaaaaaa')).toBeInTheDocument();
    });
  });
});
