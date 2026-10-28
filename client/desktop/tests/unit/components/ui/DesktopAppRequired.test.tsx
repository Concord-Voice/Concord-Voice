import React from 'react';
import { render, screen } from '../../../test-utils';

vi.mock('@/renderer/components/ui/ForceUpdateOverlay.css', () => ({}));

import DesktopAppRequired, {
  isUnsupportedBrowser,
} from '@/renderer/components/ui/DesktopAppRequired';

describe('DesktopAppRequired', () => {
  const savedElectron = globalThis.electron;
  afterEach(() => {
    globalThis.electron = savedElectron;
  });

  it('flags a production build without the preload bridge', () => {
    globalThis.electron = undefined as unknown as typeof globalThis.electron;
    expect(isUnsupportedBrowser(true)).toBe(true);
  });

  it('allows the desktop client and dev builds', () => {
    expect(isUnsupportedBrowser(true)).toBe(false);
    globalThis.electron = undefined as unknown as typeof globalThis.electron;
    expect(isUnsupportedBrowser(false)).toBe(false);
  });

  it('links to the desktop download', () => {
    render(<DesktopAppRequired />);
    expect(screen.getByRole('link', { name: /download concord/i })).toHaveAttribute(
      'href',
      'https://concordvoice.com/download'
    );
  });
});
