import React from 'react';
import { render, screen, fireEvent, waitFor, createEvent } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';

vi.mock('@/renderer/components/Members/UserProfileModal.css', () => ({}));
vi.mock('@/renderer/hooks/useUserThemeScope', () => ({
  useUserThemeScope: () => ({ scopeProps: { style: {} } }),
}));

// Mock apiFetch
const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
}));

import UserProfileModal from '@/renderer/components/Members/UserProfileModal';
import type { ServerMember } from '@/renderer/stores/memberStore';

// Match SafeLink.test.tsx's pattern: tests/setup.ts installs a base
// `window.electron` mock; this helper attaches an openExternal vi.fn so
// individual tests can assert IPC invocation. The cast layers narrow
// down to the openExternal slot without leaning on `any`.
const installOpenExternalMock = (openExternal: ReturnType<typeof vi.fn>): void => {
  const electron = (window as unknown as { electron?: Record<string, unknown> }).electron;
  if (!electron) {
    throw new Error('window.electron must be pre-installed by tests/setup.ts');
  }
  electron.openExternal = openExternal;
};

const mockMember: ServerMember = {
  user_id: 'user-1',
  server_id: 'server-1',
  username: 'testuser',
  display_name: 'Test User',
  avatar_url: null,
  role: 'member',
  joined_at: '2025-01-15T12:00:00Z',
};

describe('UserProfileModal', () => {
  const mockOnClose = vi.fn();

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: () =>
        Promise.resolve({
          user: {
            links: ['https://github.com/test'],
            created_at: '2024-06-01T12:00:00Z',
            header_image_url: null,
          },
        }),
    });
    installOpenExternalMock(vi.fn());
  });

  it('renders nothing when isOpen is false', () => {
    const { container } = render(
      <UserProfileModal
        isOpen={false}
        onClose={mockOnClose}
        member={mockMember}
        presenceStatus="online"
      />
    );
    expect(container.firstChild).toBeNull();
  });

  it('renders modal content when isOpen is true', async () => {
    render(
      <UserProfileModal
        isOpen={true}
        onClose={mockOnClose}
        member={mockMember}
        presenceStatus="online"
      />
    );
    expect(screen.getByText('Test User')).toBeInTheDocument();
    expect(screen.getByText('@testuser')).toBeInTheDocument();
    expect(screen.getByText('Member')).toBeInTheDocument();
  });

  it('shows Online status text', () => {
    render(
      <UserProfileModal
        isOpen={true}
        onClose={mockOnClose}
        member={mockMember}
        presenceStatus="online"
      />
    );
    expect(screen.getByText('Online')).toBeInTheDocument();
  });

  it('shows Do Not Disturb status', () => {
    render(
      <UserProfileModal
        isOpen={true}
        onClose={mockOnClose}
        member={mockMember}
        presenceStatus="dnd"
      />
    );
    expect(screen.getByText('Do Not Disturb')).toBeInTheDocument();
  });

  it('shows Offline for invisible status', () => {
    render(
      <UserProfileModal
        isOpen={true}
        onClose={mockOnClose}
        member={mockMember}
        presenceStatus="invisible"
      />
    );
    expect(screen.getByText('Offline')).toBeInTheDocument();
  });

  it('shows Last seen text for offline status with lastSeen', () => {
    // Set lastSeen to 5 minutes ago
    const fiveMinutesAgo = Math.floor(Date.now() / 1000) - 300;
    render(
      <UserProfileModal
        isOpen={true}
        onClose={mockOnClose}
        member={mockMember}
        presenceStatus="offline"
        lastSeen={fiveMinutesAgo}
      />
    );
    expect(screen.getByText(/Last seen 5m ago/)).toBeInTheDocument();
  });

  it('shows Offline for offline status without lastSeen', () => {
    render(
      <UserProfileModal
        isOpen={true}
        onClose={mockOnClose}
        member={mockMember}
        presenceStatus="offline"
      />
    );
    expect(screen.getByText('Offline')).toBeInTheDocument();
  });

  it('shows avatar initial when no avatar URL', () => {
    render(
      <UserProfileModal
        isOpen={true}
        onClose={mockOnClose}
        member={mockMember}
        presenceStatus="online"
      />
    );
    expect(screen.getByText('T')).toBeInTheDocument();
  });

  it('shows avatar image when avatar URL is present', () => {
    const memberWithAvatar = { ...mockMember, avatar_url: 'https://example.com/avatar.png' };
    render(
      <UserProfileModal
        isOpen={true}
        onClose={mockOnClose}
        member={memberWithAvatar}
        presenceStatus="online"
      />
    );
    expect(screen.getByAltText('testuser')).toBeInTheDocument();
  });

  it('capitalizes role label', () => {
    const adminMember = { ...mockMember, role: 'admin' as const };
    render(
      <UserProfileModal
        isOpen={true}
        onClose={mockOnClose}
        member={adminMember}
        presenceStatus="online"
      />
    );
    expect(screen.getByText('Admin')).toBeInTheDocument();
  });

  it('shows bio when member has one', () => {
    const memberWithBio = { ...mockMember, bio: 'Hello world!' };
    render(
      <UserProfileModal
        isOpen={true}
        onClose={mockOnClose}
        member={memberWithBio}
        presenceStatus="online"
      />
    );
    expect(screen.getByText('About Me')).toBeInTheDocument();
    expect(screen.getByText('Hello world!')).toBeInTheDocument();
  });

  it('does not show About Me section when bio is empty', () => {
    render(
      <UserProfileModal
        isOpen={true}
        onClose={mockOnClose}
        member={mockMember}
        presenceStatus="online"
      />
    );
    expect(screen.queryByText('About Me')).not.toBeInTheDocument();
  });

  it('fetches extended profile on open', async () => {
    render(
      <UserProfileModal
        isOpen={true}
        onClose={mockOnClose}
        member={mockMember}
        presenceStatus="online"
      />
    );
    await waitFor(() => {
      expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/users/user-1/profile');
    });
  });

  it('displays links from profile data', async () => {
    render(
      <UserProfileModal
        isOpen={true}
        onClose={mockOnClose}
        member={mockMember}
        presenceStatus="online"
      />
    );
    await waitFor(() => {
      expect(screen.getByText('Links')).toBeInTheDocument();
      expect(screen.getByText('github.com/test')).toBeInTheDocument();
    });

    // Regression marker for #800: the global a[target='_blank'] CSS rule in
    // index.css depends on this attribute being present. If a future refactor
    // drops it, profile links go back to the invisible UA-default blue.
    const link = screen.getByText('github.com/test').closest('a');
    expect(link).toHaveAttribute('target', '_blank');
    expect(link).toHaveAttribute('rel', 'noopener noreferrer');
  });

  it('displays Member Since date from profile', async () => {
    render(
      <UserProfileModal
        isOpen={true}
        onClose={mockOnClose}
        member={mockMember}
        presenceStatus="online"
      />
    );
    await waitFor(() => {
      expect(screen.getByText('Member Since')).toBeInTheDocument();
      expect(screen.getByText('Jun 1, 2024')).toBeInTheDocument();
    });
  });

  it('displays Server Joined date', () => {
    render(
      <UserProfileModal
        isOpen={true}
        onClose={mockOnClose}
        member={mockMember}
        presenceStatus="online"
      />
    );
    expect(screen.getByText('Server Joined')).toBeInTheDocument();
    expect(screen.getByText('Jan 15, 2025')).toBeInTheDocument();
  });

  it('closes on Escape key', () => {
    render(
      <UserProfileModal
        isOpen={true}
        onClose={mockOnClose}
        member={mockMember}
        presenceStatus="online"
      />
    );
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('closes when clicking overlay background', () => {
    const { container } = render(
      <UserProfileModal
        isOpen={true}
        onClose={mockOnClose}
        member={mockMember}
        presenceStatus="online"
      />
    );
    const overlay = container.querySelector('.user-profile-overlay');
    fireEvent.click(overlay!);
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('does not close when clicking inside the modal', () => {
    render(
      <UserProfileModal
        isOpen={true}
        onClose={mockOnClose}
        member={mockMember}
        presenceStatus="online"
      />
    );
    fireEvent.click(screen.getByText('Test User'));
    expect(mockOnClose).not.toHaveBeenCalled();
  });

  it('closes when close button is clicked', () => {
    render(
      <UserProfileModal
        isOpen={true}
        onClose={mockOnClose}
        member={mockMember}
        presenceStatus="online"
      />
    );
    const closeBtn = screen.getByLabelText('Close');
    fireEvent.click(closeBtn);
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('handles profile fetch failure gracefully', async () => {
    mockApiFetch.mockResolvedValue({
      ok: false,
      json: () => Promise.resolve({ error: 'Not found' }),
    });
    render(
      <UserProfileModal
        isOpen={true}
        onClose={mockOnClose}
        member={mockMember}
        presenceStatus="online"
      />
    );
    await waitFor(() => {
      // Should not crash; links section should not appear
      expect(screen.queryByText('Links')).not.toBeInTheDocument();
    });
  });

  it('shows loading indicator while fetching profile', async () => {
    let resolvePromise!: (value: unknown) => void;
    mockApiFetch.mockReturnValue(
      new Promise((resolve) => {
        resolvePromise = resolve;
      })
    );
    render(
      <UserProfileModal
        isOpen={true}
        onClose={mockOnClose}
        member={mockMember}
        presenceStatus="online"
      />
    );
    expect(screen.getByText('Loading profile...')).toBeInTheDocument();
    resolvePromise({
      ok: true,
      json: () => Promise.resolve({ user: { links: [], created_at: '' } }),
    });
    await waitFor(() => {
      expect(screen.queryByText('Loading profile...')).not.toBeInTheDocument();
    });
  });

  it('formats last seen as "Just now" for recent times', () => {
    const justNow = Math.floor(Date.now() / 1000);
    render(
      <UserProfileModal
        isOpen={true}
        onClose={mockOnClose}
        member={mockMember}
        presenceStatus="offline"
        lastSeen={justNow}
      />
    );
    expect(screen.getByText(/Just now/)).toBeInTheDocument();
  });

  it('formats last seen in hours', () => {
    const twoHoursAgo = Math.floor(Date.now() / 1000) - 7200;
    render(
      <UserProfileModal
        isOpen={true}
        onClose={mockOnClose}
        member={mockMember}
        presenceStatus="offline"
        lastSeen={twoHoursAgo}
      />
    );
    expect(screen.getByText(/2h ago/)).toBeInTheDocument();
  });

  it('formats last seen in days', () => {
    const threeDaysAgo = Math.floor(Date.now() / 1000) - 259200;
    render(
      <UserProfileModal
        isOpen={true}
        onClose={mockOnClose}
        member={mockMember}
        presenceStatus="offline"
        lastSeen={threeDaysAgo}
      />
    );
    expect(screen.getByText(/3d ago/)).toBeInTheDocument();
  });

  describe('external link routing through open-external IPC (#775)', () => {
    it('calls window.electron.openExternal with the href and preventDefaults the click', async () => {
      const openExternal = vi.fn();
      installOpenExternalMock(openExternal);
      mockApiFetch.mockResolvedValue({
        ok: true,
        json: () =>
          Promise.resolve({
            user: {
              links: ['https://example.com/portfolio'],
              created_at: '2024-06-01T12:00:00Z',
              header_image_url: null,
            },
          }),
      });
      render(
        <UserProfileModal
          isOpen={true}
          onClose={mockOnClose}
          member={mockMember}
          presenceStatus="online"
        />
      );
      await waitFor(() => {
        expect(screen.getByText('example.com/portfolio')).toBeInTheDocument();
      });
      const link = screen.getByText('example.com/portfolio').closest('a');
      if (link === null) throw new Error('link anchor not rendered');
      // Construct the click event directly so we can assert defaultPrevented —
      // a regression that drops the e.preventDefault() line would leave BOTH
      // IPC routing AND default anchor activation firing simultaneously, which
      // toHaveBeenCalledWith alone cannot detect.
      const event = createEvent.click(link);
      fireEvent(link, event);
      expect(openExternal).toHaveBeenCalledWith('https://example.com/portfolio');
      expect(event.defaultPrevented).toBe(true);
    });

    it('calls window.electron.openExternal with the href for http:// profile links (the #775 regression fix)', async () => {
      const openExternal = vi.fn();
      installOpenExternalMock(openExternal);
      mockApiFetch.mockResolvedValue({
        ok: true,
        json: () =>
          Promise.resolve({
            user: {
              links: ['http://legacy.example.com/portfolio'],
              created_at: '2024-06-01T12:00:00Z',
              header_image_url: null,
            },
          }),
      });
      render(
        <UserProfileModal
          isOpen={true}
          onClose={mockOnClose}
          member={mockMember}
          presenceStatus="online"
        />
      );
      await waitFor(() => {
        expect(screen.getByText('legacy.example.com/portfolio')).toBeInTheDocument();
      });
      fireEvent.click(screen.getByText('legacy.example.com/portfolio'));
      expect(openExternal).toHaveBeenCalledWith('http://legacy.example.com/portfolio');
    });

    it('does not call openExternal when the link is not clicked', async () => {
      const openExternal = vi.fn();
      installOpenExternalMock(openExternal);
      render(
        <UserProfileModal
          isOpen={true}
          onClose={mockOnClose}
          member={mockMember}
          presenceStatus="online"
        />
      );
      await waitFor(() => {
        expect(screen.getByText('github.com/test')).toBeInTheDocument();
      });
      expect(openExternal).not.toHaveBeenCalled();
    });

    it('attaches a catch handler when openExternal returns a Promise', async () => {
      const openExternal = vi.fn().mockResolvedValue({ ok: true });
      installOpenExternalMock(openExternal);
      mockApiFetch.mockResolvedValue({
        ok: true,
        json: () =>
          Promise.resolve({
            user: {
              links: ['https://promise.example.com/'],
              created_at: '2024-06-01T12:00:00Z',
              header_image_url: null,
            },
          }),
      });
      render(
        <UserProfileModal
          isOpen={true}
          onClose={mockOnClose}
          member={mockMember}
          presenceStatus="online"
        />
      );
      await waitFor(() => {
        expect(screen.getByText('promise.example.com')).toBeInTheDocument();
      });
      fireEvent.click(screen.getByText('promise.example.com'));
      expect(openExternal).toHaveBeenCalledWith('https://promise.example.com/');
      // Returned Promise resolves successfully — no rejection, no thrown error.
      // The test asserts that exercising the isPromiseLike branch doesn't raise.
    });

    it('silently swallows openExternal rejection (renderer treats as no-op)', async () => {
      const openExternal = vi.fn().mockRejectedValue(new Error('main-process IPC denied'));
      installOpenExternalMock(openExternal);
      mockApiFetch.mockResolvedValue({
        ok: true,
        json: () =>
          Promise.resolve({
            user: {
              links: ['https://denied.example.com/'],
              created_at: '2024-06-01T12:00:00Z',
              header_image_url: null,
            },
          }),
      });
      render(
        <UserProfileModal
          isOpen={true}
          onClose={mockOnClose}
          member={mockMember}
          presenceStatus="online"
        />
      );
      await waitFor(() => {
        expect(screen.getByText('denied.example.com')).toBeInTheDocument();
      });
      fireEvent.click(screen.getByText('denied.example.com'));
      // Let the rejected promise settle so the .catch handler runs.
      await new Promise((resolve) => setTimeout(resolve, 0));
      // The renderer must NOT crash and the unhandled-rejection listener must
      // NOT fire. Reaching this point without an unhandled-rejection error
      // proves the .catch attached and consumed the rejection.
      expect(openExternal).toHaveBeenCalledWith('https://denied.example.com/');
    });

    it('handles openExternal returning undefined (preload bridge present but synchronous)', async () => {
      const openExternal = vi.fn();
      // vi.fn() defaults to returning undefined. isPromiseLike(undefined) is
      // false, so no .catch is attached; the click is still preventDefault'd
      // (no anchor activation happens in jsdom).
      installOpenExternalMock(openExternal);
      render(
        <UserProfileModal
          isOpen={true}
          onClose={mockOnClose}
          member={mockMember}
          presenceStatus="online"
        />
      );
      await waitFor(() => {
        expect(screen.getByText('github.com/test')).toBeInTheDocument();
      });
      fireEvent.click(screen.getByText('github.com/test'));
      expect(openExternal).toHaveBeenCalledWith('https://github.com/test');
    });

    it('falls through to default anchor when window.electron.openExternal is absent', async () => {
      const electron = (window as unknown as { electron?: Record<string, unknown> }).electron;
      const saved = electron?.openExternal;
      if (electron) delete electron.openExternal;
      try {
        render(
          <UserProfileModal
            isOpen={true}
            onClose={mockOnClose}
            member={mockMember}
            presenceStatus="online"
          />
        );
        await waitFor(() => {
          expect(screen.getByText('github.com/test')).toBeInTheDocument();
        });
        // No openExternal → click should not throw. jsdom default-anchor
        // activation is a no-op; setWindowOpenHandler is a main-process
        // surface that doesn't exist in the renderer test environment.
        fireEvent.click(screen.getByText('github.com/test'));
      } finally {
        if (electron && saved !== undefined) electron.openExternal = saved;
      }
    });

    it('preserves the target="_blank" + rel attributes (regression marker for #800 CSS rule)', async () => {
      render(
        <UserProfileModal
          isOpen={true}
          onClose={mockOnClose}
          member={mockMember}
          presenceStatus="online"
        />
      );
      await waitFor(() => {
        expect(screen.getByText('github.com/test')).toBeInTheDocument();
      });
      const link = screen.getByText('github.com/test').closest('a');
      expect(link).toHaveAttribute('target', '_blank');
      expect(link).toHaveAttribute('rel', 'noopener noreferrer');
    });
  });
});
