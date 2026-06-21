import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { vi } from 'vitest';
import { useFriendStore } from '@/renderer/stores/friendStore';

// Mock Shamir secret sharing
vi.mock('@/renderer/utils/shamir', () => ({
  split: vi.fn().mockImplementation((_secret: Uint8Array, n: number) => {
    return Array.from({ length: n }, (_, i) => ({
      index: i + 1,
      data: new Uint8Array([1, 2, 3]),
    }));
  }),
}));

// Mock crypto utilities
vi.mock('@/renderer/utils/crypto', () => ({
  base64ToArrayBuffer: vi.fn().mockReturnValue(new ArrayBuffer(32)),
  arrayBufferToBase64: vi.fn().mockReturnValue('bW9jay1iYXNlNjQ='),
}));

// Mock e2eeService
vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    getWrappingKey: vi.fn().mockReturnValue('mock-wrapping-key'),
    getWrappedPrivateKey: vi.fn().mockReturnValue('bW9jay1kYXRh'),
  },
}));

// Mock apiFetch
const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  API_BASE: 'http://localhost:8080',
}));

// Mock Web Crypto
Object.defineProperty(globalThis, 'crypto', {
  value: {
    subtle: {
      unwrapKey: vi.fn().mockResolvedValue('mock-private-key'),
      exportKey: vi.fn().mockResolvedValue(new ArrayBuffer(64)),
      importKey: vi.fn().mockResolvedValue('mock-imported-key'),
      generateKey: vi.fn().mockResolvedValue('mock-aes-key'),
      encrypt: vi.fn().mockResolvedValue(new ArrayBuffer(32)),
    },
    getRandomValues: (arr: Uint8Array) => arr,
  },
  writable: true,
  configurable: true,
});

import RecoveryCircle from '@/renderer/components/Settings/RecoveryCircle';
import { e2eeService } from '@/renderer/services/e2eeService';

const mockFriends = [
  {
    id: 'f1',
    userId: 'user-2',
    username: 'alice',
    displayName: 'Alice',
    status: 'online' as const,
  },
  { id: 'f2', userId: 'user-3', username: 'bob', displayName: 'Bob', status: 'offline' as const },
  {
    id: 'f3',
    userId: 'user-4',
    username: 'charlie',
    displayName: undefined,
    status: 'online' as const,
  },
  {
    id: 'f4',
    userId: 'user-5',
    username: 'diana',
    displayName: 'Diana',
    status: 'online' as const,
  },
];

describe('RecoveryCircle', () => {
  const onComplete = vi.fn();
  const onCancel = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
    // Set up friend store with mock friends
    useFriendStore.setState({
      friends: mockFriends,
      fetchFriends: vi.fn(),
    });
  });

  // ── Initial Rendering (Select Step) ────────────────────────────────────

  it('renders setup wizard title', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    expect(screen.getByText('Set Up Recovery Circle')).toBeInTheDocument();
  });

  it('renders explanatory text about Shamir Secret Sharing', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    expect(screen.getByText(/Shamir/)).toBeInTheDocument();
  });

  it('renders friend list with checkboxes', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    expect(screen.getByText('Alice')).toBeInTheDocument();
    expect(screen.getByText('Bob')).toBeInTheDocument();
    expect(screen.getByText('@charlie')).toBeInTheDocument();
  });

  it('shows display name when available, username otherwise', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    // charlie has no displayName
    expect(screen.getByText('charlie')).toBeInTheDocument();
  });

  it('renders threshold slider', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    expect(screen.getByLabelText(/Recovery threshold/)).toBeInTheDocument();
  });

  it('renders Continue and Cancel buttons', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    expect(screen.getByText('Continue')).toBeInTheDocument();
    expect(screen.getByText('Cancel')).toBeInTheDocument();
  });

  it('disables Continue until at least 2 contacts are selected', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    expect(screen.getByText('Continue')).toBeDisabled();
  });

  it('calls onCancel when Cancel is clicked', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    fireEvent.click(screen.getByText('Cancel'));
    expect(onCancel).toHaveBeenCalled();
  });

  it('calls fetchFriends on mount', () => {
    const fetchFriends = vi.fn();
    useFriendStore.setState({ fetchFriends });
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    expect(fetchFriends).toHaveBeenCalled();
  });

  // ── Contact Selection ──────────────────────────────────────────────────

  it('enables Continue when 3 contacts are selected (meeting default threshold)', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);

    const checkboxes = screen.getAllByRole('checkbox');
    fireEvent.click(checkboxes[0]); // Alice
    fireEvent.click(checkboxes[1]); // Bob
    fireEvent.click(checkboxes[2]); // Charlie

    expect(screen.getByText('Continue')).not.toBeDisabled();
  });

  it('toggles contact selection off when clicked again', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);

    const checkboxes = screen.getAllByRole('checkbox');
    fireEvent.click(checkboxes[0]); // Select Alice
    fireEvent.click(checkboxes[1]); // Select Bob
    fireEvent.click(checkboxes[2]); // Select Charlie (threshold=3, need 3 selected)
    expect(screen.getByText('Continue')).not.toBeDisabled();

    fireEvent.click(checkboxes[2]); // Deselect Charlie — now only 2 < threshold(3)
    expect(screen.getByText('Continue')).toBeDisabled();
  });

  it('limits selection to 7 contacts', () => {
    // Add more friends to test the limit
    const manyFriends = Array.from({ length: 10 }, (_, i) => ({
      id: `f${i}`,
      userId: `user-${i + 10}`,
      username: `friend${i}`,
      displayName: `Friend ${i}`,
      status: 'online' as const,
    }));
    useFriendStore.setState({ friends: manyFriends });

    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);

    const checkboxes = screen.getAllByRole('checkbox');
    // Select 7 contacts
    for (let i = 0; i < 7; i++) {
      fireEvent.click(checkboxes[i]);
    }

    // 8th selection should not add
    fireEvent.click(checkboxes[7]);

    // Count checked checkboxes
    const checked = checkboxes.filter((cb) => (cb as HTMLInputElement).checked);
    expect(checked.length).toBe(7);
  });

  it('shows "No friends found" when friend list is empty', () => {
    useFriendStore.setState({ friends: [] });
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    expect(screen.getByText(/No friends found/)).toBeInTheDocument();
  });

  // ── Threshold Slider ───────────────────────────────────────────────────

  it('adjusts threshold when slider is changed', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);

    // Select 3 contacts first
    const checkboxes = screen.getAllByRole('checkbox');
    fireEvent.click(checkboxes[0]);
    fireEvent.click(checkboxes[1]);
    fireEvent.click(checkboxes[2]);

    const slider = screen.getByRole('slider');
    fireEvent.change(slider, { target: { value: '2' } });

    expect(screen.getByText(/Recovery threshold: 2 of 3/)).toBeInTheDocument();
  });

  it('disables threshold slider when fewer than 2 contacts selected', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);
    const slider = screen.getByRole('slider');
    expect(slider).toBeDisabled();
  });

  // ── Confirm Step ───────────────────────────────────────────────────────

  it('transitions to confirm step when Continue is clicked', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);

    const checkboxes = screen.getAllByRole('checkbox');
    fireEvent.click(checkboxes[0]); // Alice
    fireEvent.click(checkboxes[1]); // Bob
    fireEvent.click(checkboxes[2]); // Charlie — 3 selected matches default threshold=3
    fireEvent.click(screen.getByText('Continue'));

    expect(screen.getByText('Confirm Recovery Circle')).toBeInTheDocument();
    expect(screen.getByText(/3 of 3/)).toBeInTheDocument();
    expect(screen.getByPlaceholderText('Your password')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('MFA code (if enabled)')).toBeInTheDocument();
  });

  it('shows Create Recovery Circle button on confirm step', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);

    const checkboxes = screen.getAllByRole('checkbox');
    fireEvent.click(checkboxes[0]);
    fireEvent.click(checkboxes[1]);
    fireEvent.click(checkboxes[2]); // need 3 to match default threshold
    fireEvent.click(screen.getByText('Continue'));

    expect(screen.getByText('Create Recovery Circle')).toBeInTheDocument();
  });

  it('disables Create Recovery Circle when password is empty', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);

    const checkboxes = screen.getAllByRole('checkbox');
    fireEvent.click(checkboxes[0]);
    fireEvent.click(checkboxes[1]);
    fireEvent.click(checkboxes[2]);
    fireEvent.click(screen.getByText('Continue'));

    expect(screen.getByText('Create Recovery Circle')).toBeDisabled();
  });

  it('goes back to select step when Back is clicked', () => {
    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);

    const checkboxes = screen.getAllByRole('checkbox');
    fireEvent.click(checkboxes[0]);
    fireEvent.click(checkboxes[1]);
    fireEvent.click(checkboxes[2]);
    fireEvent.click(screen.getByText('Continue'));

    expect(screen.getByText('Confirm Recovery Circle')).toBeInTheDocument();
    fireEvent.click(screen.getByText('Back'));
    expect(screen.getByText('Set Up Recovery Circle')).toBeInTheDocument();
  });

  // ── Setup Flow ─────────────────────────────────────────────────────────

  it('shows loading state during setup', async () => {
    // Public key fetch will never resolve
    mockApiFetch.mockReturnValue(new Promise(() => {}));

    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);

    const checkboxes = screen.getAllByRole('checkbox');
    fireEvent.click(checkboxes[0]);
    fireEvent.click(checkboxes[1]);
    // Lower threshold to 2 so Continue is enabled with 2 contacts
    const slider = screen.getByRole('slider');
    fireEvent.change(slider, { target: { value: '2' } });
    fireEvent.click(screen.getByText('Continue'));

    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'mypassword' },
    });
    fireEvent.click(screen.getByText('Create Recovery Circle'));

    await waitFor(() => {
      expect(screen.getByText('Setting up...')).toBeInTheDocument();
    });
  });

  it('shows done step after successful setup', async () => {
    // Mock public key fetch for each contact + final upload
    mockApiFetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ public_key: 'bW9jay1wdWJsaWMta2V5' }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ public_key: 'bW9jay1wdWJsaWMta2V5' }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({}),
      });

    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);

    const checkboxes = screen.getAllByRole('checkbox');
    fireEvent.click(checkboxes[0]);
    fireEvent.click(checkboxes[1]);
    const slider = screen.getByRole('slider');
    fireEvent.change(slider, { target: { value: '2' } });
    fireEvent.click(screen.getByText('Continue'));

    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'mypassword' },
    });
    fireEvent.click(screen.getByText('Create Recovery Circle'));

    await waitFor(() => {
      expect(screen.getByText('Recovery Circle Configured')).toBeInTheDocument();
      expect(screen.getByText('Recovery Circle Active')).toBeInTheDocument();
    });
  });

  it('calls onComplete when Done is clicked on success', async () => {
    mockApiFetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ public_key: 'bW9jay1wdWJsaWMta2V5' }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ public_key: 'bW9jay1wdWJsaWMta2V5' }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({}),
      });

    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);

    const checkboxes = screen.getAllByRole('checkbox');
    fireEvent.click(checkboxes[0]);
    fireEvent.click(checkboxes[1]);
    const slider = screen.getByRole('slider');
    fireEvent.change(slider, { target: { value: '2' } });
    fireEvent.click(screen.getByText('Continue'));

    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'mypassword' },
    });
    fireEvent.click(screen.getByText('Create Recovery Circle'));

    await waitFor(() => {
      expect(screen.getByText('Done')).toBeInTheDocument();
    });

    fireEvent.click(screen.getByText('Done'));
    expect(onComplete).toHaveBeenCalled();
  });

  // ── Error States ───────────────────────────────────────────────────────

  it('shows error when E2EE keys are not available', async () => {
    (e2eeService.getWrappingKey as ReturnType<typeof vi.fn>).mockReturnValueOnce(null);

    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);

    const checkboxes = screen.getAllByRole('checkbox');
    fireEvent.click(checkboxes[0]);
    fireEvent.click(checkboxes[1]);
    const slider = screen.getByRole('slider');
    fireEvent.change(slider, { target: { value: '2' } });
    fireEvent.click(screen.getByText('Continue'));

    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'mypassword' },
    });
    fireEvent.click(screen.getByText('Create Recovery Circle'));

    await waitFor(() => {
      expect(screen.getByText('E2EE keys not available')).toBeInTheDocument();
    });
  });

  it('shows error when public key fetch fails', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      json: async () => ({ error: 'User not found' }),
    });

    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);

    const checkboxes = screen.getAllByRole('checkbox');
    fireEvent.click(checkboxes[0]);
    fireEvent.click(checkboxes[1]);
    const slider = screen.getByRole('slider');
    fireEvent.change(slider, { target: { value: '2' } });
    fireEvent.click(screen.getByText('Continue'));

    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'mypassword' },
    });
    fireEvent.click(screen.getByText('Create Recovery Circle'));

    await waitFor(() => {
      expect(screen.getByText('Failed to fetch public key for contact')).toBeInTheDocument();
    });
  });

  it('shows error when final upload fails', async () => {
    mockApiFetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ public_key: 'bW9jay1wdWJsaWMta2V5' }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ public_key: 'bW9jay1wdWJsaWMta2V5' }),
      })
      .mockResolvedValueOnce({
        ok: false,
        json: async () => ({ error: 'Password incorrect' }),
      });

    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);

    const checkboxes = screen.getAllByRole('checkbox');
    fireEvent.click(checkboxes[0]);
    fireEvent.click(checkboxes[1]);
    const slider = screen.getByRole('slider');
    fireEvent.change(slider, { target: { value: '2' } });
    fireEvent.click(screen.getByText('Continue'));

    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'wrongpw' },
    });
    fireEvent.click(screen.getByText('Create Recovery Circle'));

    await waitFor(() => {
      expect(screen.getByText('Password incorrect')).toBeInTheDocument();
    });
  });

  it('displays threshold and contact count on done step', async () => {
    mockApiFetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ public_key: 'bW9jay1wdWJsaWMta2V5' }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ public_key: 'bW9jay1wdWJsaWMta2V5' }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ public_key: 'bW9jay1wdWJsaWMta2V5' }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({}),
      });

    render(<RecoveryCircle onComplete={onComplete} onCancel={onCancel} />);

    const checkboxes = screen.getAllByRole('checkbox');
    fireEvent.click(checkboxes[0]);
    fireEvent.click(checkboxes[1]);
    fireEvent.click(checkboxes[2]);

    // Set threshold to 2
    const slider = screen.getByRole('slider');
    fireEvent.change(slider, { target: { value: '2' } });

    fireEvent.click(screen.getByText('Continue'));
    fireEvent.change(screen.getByPlaceholderText('Your password'), {
      target: { value: 'pw' },
    });
    fireEvent.click(screen.getByText('Create Recovery Circle'));

    await waitFor(() => {
      expect(screen.getByText(/2 of 3/)).toBeInTheDocument();
    });
  });
});
