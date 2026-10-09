import { vi } from 'vitest';
import React from 'react';

// ─── Mocks (before component imports) ───────────────────────────────────────

vi.mock('@/renderer/components/Auth/LoadingSpinner', () => ({ default: () => null }));
vi.mock('@/renderer/components/ui/ImageCropEditor', () => ({ default: () => null }));
vi.mock('@/renderer/components/ui/Modal', () => ({ default: () => null }));

// useImageUpload is stubbed so the real avatar/header file inputs are present
// and we can drive `handleChange` (the L9 size-check wraps it in the host).
const avatarHandleChange = vi.fn();
const headerHandleChange = vi.fn();
function makeImageUploadStub(handleChange: ReturnType<typeof vi.fn>) {
  return {
    preview: null,
    imageUrl: null,
    removed: false,
    pendingFile: null,
    showCrop: false,
    fileInputRef: { current: null },
    handleClick: vi.fn(),
    handleKeyDown: vi.fn(),
    handleChange,
    handleCropConfirm: vi.fn(),
    handleCropCancel: vi.fn(),
    handleRemove: vi.fn(),
    reset: vi.fn(),
  };
}
let imageUploadCalls = 0;
vi.mock('@/renderer/hooks/messaging/useImageUpload', () => ({
  useImageUpload: vi.fn(() => {
    // First call in the component is the avatar, second is the header.
    const stub =
      imageUploadCalls % 2 === 0
        ? makeImageUploadStub(avatarHandleChange)
        : makeImageUploadStub(headerHandleChange);
    imageUploadCalls += 1;
    return stub;
  }),
}));

// ─── Imports (after mocks) ──────────────────────────────────────────────────

import { render, screen, fireEvent, act } from '../../../test-utils';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import {
  FREE_ENTITLEMENT,
  useSubscriptionStore,
  type Entitlement,
} from '@/renderer/stores/auth/subscriptionStore';
import { mockUser } from '../../../mocks/fixtures';
import ProfileInfoForm from '@/renderer/components/Profile/ProfileInfoForm';

function setEntitlement(overrides: Partial<Entitlement>) {
  useSubscriptionStore.setState({ entitlement: { ...FREE_ENTITLEMENT, ...overrides } });
}

function makeFile(name: string, size: number): File {
  const f = new File(['x'], name, { type: 'image/png' });
  Object.defineProperty(f, 'size', { value: size });
  return f;
}

beforeEach(() => {
  vi.clearAllMocks();
  imageUploadCalls = 0;
  useSubscriptionStore.setState({ entitlement: FREE_ENTITLEMENT, hydrated: true });
  useUserStore.setState({ user: { ...mockUser }, isLoading: false });
});

// ─── L8: username cadence note ──────────────────────────────────────────────

describe('ProfileInfoForm — L8 username cadence', () => {
  it('free cadence: shows the "Premium: every 3 months" upsell note', () => {
    render(<ProfileInfoForm />);
    expect(screen.getByText(/Premium: every 3 months\./)).toBeInTheDocument();
    expect(screen.getByText(/You can change your username every 6 months/)).toBeInTheDocument();
  });

  it('keeps the free upsell when its interval changes', () => {
    setEntitlement({ usernameChangeIntervalMonths: 6 });
    render(<ProfileInfoForm />);
    expect(screen.getByText(/Premium: every 3 months\./)).toBeInTheDocument();
    expect(screen.queryByText(/once per year/)).not.toBeInTheDocument();
  });

  it('on cooldown: still shows the premium cadence upsell next to the date note', () => {
    useUserStore.setState({
      user: {
        ...mockUser,
        username_change_eligible_at: new Date(Date.now() + 86_400_000).toISOString(),
      },
      isLoading: false,
    });
    render(<ProfileInfoForm />);
    expect(screen.getByText(/Change again on/)).toBeInTheDocument();
    expect(screen.getByText(/Premium: every 3 months\./)).toBeInTheDocument();
  });

  it('premium cadence (3-month interval): hides the premium upsell note', () => {
    setEntitlement({ tier: 'premium', usernameChangeIntervalMonths: 3 });
    render(<ProfileInfoForm />);
    expect(screen.queryByText(/Premium: every 3 months\./)).not.toBeInTheDocument();
    expect(screen.getByText(/You can change your username every 3 months/)).toBeInTheDocument();
  });

  it('uses the server date and generic copy until the entitlement is hydrated', () => {
    useSubscriptionStore.setState({ hydrated: false });
    useUserStore.setState({
      user: {
        ...mockUser,
        username_changed_at: new Date(Date.now() - 120 * 86_400_000).toISOString(),
        username_change_eligible_at: new Date(Date.now() + 1 * 86_400_000).toISOString(),
      },
      isLoading: false,
    });
    render(<ProfileInfoForm />);
    expect(screen.getByLabelText('Username')).toBeDisabled();
    expect(
      screen.getByText(/Your plan sets the wait between username changes/)
    ).toBeInTheDocument();
    expect(screen.queryByText(/every 6 months/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Premium: every 3 months/)).not.toBeInTheDocument();
  });

  it('uses legacy seconds when an older server sends no calendar-month field', () => {
    const changedAt = new Date(Date.now() - 120 * 86_400_000).toISOString();
    useUserStore.setState({
      user: { ...mockUser, username_changed_at: changedAt },
      isLoading: false,
    });
    const legacyFree: Entitlement = { ...FREE_ENTITLEMENT };
    delete legacyFree.usernameChangeIntervalMonths;
    useSubscriptionStore.setState({
      entitlement: { ...legacyFree, usernameChangeIntervalSeconds: 365 * 86_400 },
    });
    render(<ProfileInfoForm />);
    expect(screen.getByLabelText('Username')).toBeDisabled();
    expect(screen.getByText(/every 365 days/)).toBeInTheDocument();
    expect(screen.queryByText(/Premium: every 3 months/)).not.toBeInTheDocument();

    act(() => {
      useSubscriptionStore.setState({
        entitlement: { ...legacyFree, tier: 'premium', usernameChangeIntervalSeconds: 91 * 86_400 },
      });
    });
    expect(screen.getByLabelText('Username')).not.toBeDisabled();
    expect(screen.getByText(/every 91 days/)).toBeInTheDocument();
  });

  it('recomputes the cooldown on a live tier change without refetching the profile', () => {
    const changedAt = new Date(Date.now() - 120 * 86_400_000).toISOString();
    useUserStore.setState({
      user: {
        ...mockUser,
        username_changed_at: changedAt,
        username_change_eligible_at: new Date(Date.now() + 60 * 86_400_000).toISOString(),
      },
      isLoading: false,
    });
    useSubscriptionStore.setState({ hydrated: true });
    render(<ProfileInfoForm />);
    expect(screen.getByLabelText('Username')).toBeDisabled();

    act(() => {
      setEntitlement({ tier: 'premium', usernameChangeIntervalMonths: 3 });
    });
    expect(screen.getByLabelText('Username')).not.toBeDisabled();

    act(() => {
      setEntitlement({ tier: 'free', usernameChangeIntervalMonths: 6 });
    });
    expect(screen.getByLabelText('Username')).toBeDisabled();
  });
});

// ─── L9: avatar / banner size upsell ────────────────────────────────────────

describe('ProfileInfoForm — L9 avatar/banner size upsell', () => {
  it('avatar hint shows the current free floor', () => {
    render(<ProfileInfoForm />);
    const hint = document.querySelector(
      '.profile-avatar-actions .profile-avatar-actions-label'
    ) as HTMLElement;
    expect(hint.textContent?.replace(/\s+/g, ' ')).toContain(
      'Click to upload an avatar (PNG, JPEG, GIF, WebP — max 5.0 MB)'
    );
  });

  it('avatar over 5 MB: shows the non-modal banner with sizes', () => {
    render(<ProfileInfoForm />);
    const inputs = document.querySelectorAll('input[type="file"]');
    const avatar = inputs[0] as HTMLInputElement;
    fireEvent.change(avatar, { target: { files: [makeFile('big.png', 6 * 1024 * 1024)] } });
    const banner = document.querySelector('.image-upsell-banner') as HTMLElement;
    expect(banner).toBeInTheDocument();
    expect(banner.textContent).toContain('This file is');
    expect(banner.textContent).toContain('Free limit');
    expect(banner.textContent).toContain('Premium raises it to');
  });

  it('avatar over limit: does NOT block — handleChange still runs', () => {
    render(<ProfileInfoForm />);
    const avatar = document.querySelectorAll('input[type="file"]')[0] as HTMLInputElement;
    fireEvent.change(avatar, { target: { files: [makeFile('big.png', 6 * 1024 * 1024)] } });
    expect(avatarHandleChange).toHaveBeenCalled();
  });

  it('banner over 5 MB: shows the upsell banner', () => {
    render(<ProfileInfoForm />);
    const header = document.querySelectorAll('input[type="file"]')[1] as HTMLInputElement;
    fireEvent.change(header, { target: { files: [makeFile('wide.png', 6 * 1024 * 1024)] } });
    expect(document.querySelector('.image-upsell-banner')).toBeInTheDocument();
    expect(headerHandleChange).toHaveBeenCalled();
  });

  it('within limit: no banner', () => {
    render(<ProfileInfoForm />);
    const avatar = document.querySelectorAll('input[type="file"]')[0] as HTMLInputElement;
    fireEvent.change(avatar, { target: { files: [makeFile('ok.png', 4 * 1024 * 1024)] } });
    expect(document.querySelector('.image-upsell-banner')).not.toBeInTheDocument();
  });

  it('the banner is dismissible', () => {
    render(<ProfileInfoForm />);
    const avatar = document.querySelectorAll('input[type="file"]')[0] as HTMLInputElement;
    fireEvent.change(avatar, { target: { files: [makeFile('big.png', 6 * 1024 * 1024)] } });
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }));
    expect(document.querySelector('.image-upsell-banner')).not.toBeInTheDocument();
  });

  it('entitled (premium avatar cap): no banner for a file within the higher cap', () => {
    setEntitlement({ maxAvatarBytes: 10 * 1024 * 1024 });
    render(<ProfileInfoForm />);
    const avatar = document.querySelectorAll('input[type="file"]')[0] as HTMLInputElement;
    fireEvent.change(avatar, { target: { files: [makeFile('big.png', 6 * 1024 * 1024)] } });
    expect(document.querySelector('.image-upsell-banner')).not.toBeInTheDocument();
  });
});
