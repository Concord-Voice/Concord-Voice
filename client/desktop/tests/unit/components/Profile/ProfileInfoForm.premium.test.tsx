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
vi.mock('@/renderer/hooks/useImageUpload', () => ({
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

// Entitlement: FREE floor.
const entitlementOverrides: Record<string, unknown> = {};
function freeEntitlement() {
  return {
    usernameChangeIntervalSeconds: 31_536_000,
    maxAvatarBytes: 5_242_880,
    maxBannerBytes: 5_242_880,
    ...entitlementOverrides,
  };
}
vi.mock('@/renderer/hooks/useEntitlement', () => ({
  useEntitlement: vi.fn((selector: (e: Record<string, unknown>) => unknown) =>
    selector(freeEntitlement())
  ),
}));

// ─── Imports (after mocks) ──────────────────────────────────────────────────

import { render, screen, fireEvent } from '../../../test-utils';
import { useUserStore } from '@/renderer/stores/userStore';
import { mockUser } from '../../../mocks/fixtures';
import ProfileInfoForm from '@/renderer/components/Profile/ProfileInfoForm';

function setEntitlement(overrides: Record<string, unknown>) {
  for (const k of Object.keys(entitlementOverrides)) delete entitlementOverrides[k];
  Object.assign(entitlementOverrides, overrides);
}

function makeFile(name: string, size: number): File {
  const f = new File(['x'], name, { type: 'image/png' });
  Object.defineProperty(f, 'size', { value: size });
  return f;
}

beforeEach(() => {
  vi.clearAllMocks();
  imageUploadCalls = 0;
  setEntitlement({});
  useUserStore.setState({ user: { ...mockUser }, isLoading: false });
});

// ─── L8: username cadence note ──────────────────────────────────────────────

describe('ProfileInfoForm — L8 username cadence', () => {
  it('free cadence: shows the "Premium: every 3 months" upsell note', () => {
    render(<ProfileInfoForm />);
    expect(screen.getByText(/Premium: every 3 months\./)).toBeInTheDocument();
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
    setEntitlement({ usernameChangeIntervalSeconds: 7_776_000 }); // ~90 days
    render(<ProfileInfoForm />);
    expect(screen.queryByText(/Premium: every 3 months\./)).not.toBeInTheDocument();
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
