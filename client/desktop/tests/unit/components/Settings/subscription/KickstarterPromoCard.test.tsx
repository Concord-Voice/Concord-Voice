import { describe, it, expect, afterEach, vi } from 'vitest';
import { render, screen, cleanup } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import KickstarterPromoCard from '@/renderer/components/Settings/subscription/KickstarterPromoCard';
import { KICKSTARTER_URL } from '@/renderer/components/Settings/subscription/subscriptionCopy';

// tests/setup.ts defines a shared window.electron bridge (writable, no
// openExternal). We add/remove ONLY openExternal per test so we exercise both
// the bridged and bridge-absent paths without clobbering the shared bridge.
const bridge = (globalThis as unknown as { electron: Record<string, unknown> }).electron;

afterEach(() => {
  cleanup();
  delete bridge.openExternal;
  vi.restoreAllMocks();
});

describe('KickstarterPromoCard (#1304)', () => {
  it('routes the campaign link through window.electron.openExternal on click', async () => {
    const openExternal = vi.fn().mockResolvedValue({ ok: true });
    bridge.openExternal = openExternal;

    render(<KickstarterPromoCard />);
    await userEvent.click(screen.getByRole('link', { name: /View the Kickstarter campaign/i }));

    expect(openExternal).toHaveBeenCalledTimes(1);
    expect(openExternal).toHaveBeenCalledWith(KICKSTARTER_URL);
    // The campaign URL is an https URL.
    expect(KICKSTARTER_URL.startsWith('https://')).toBe(true);
  });

  it('carries target=_blank and rel="noopener noreferrer" on the anchor', () => {
    render(<KickstarterPromoCard />);
    const link = screen.getByRole('link', { name: /View the Kickstarter campaign/i });
    expect(link).toHaveAttribute('href', KICKSTARTER_URL);
    expect(link).toHaveAttribute('target', '_blank');
    expect(link).toHaveAttribute('rel', 'noopener noreferrer');
  });

  it('does not throw when openExternal is absent (default anchor activation fallback)', async () => {
    // No openExternal on the bridge — the click must not throw; default anchor
    // activation is the fallback (main-process setWindowOpenHandler re-validates).
    render(<KickstarterPromoCard />);
    await expect(
      userEvent.click(screen.getByRole('link', { name: /View the Kickstarter campaign/i }))
    ).resolves.not.toThrow();
  });
});
