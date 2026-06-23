// @vitest-environment node
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { revealLoadFailure } from '../../../src/main/loadFailureVisibility';
import { closeSplash } from '../../../src/main/splashWindow';

const { mockShowErrorBox } = vi.hoisted(() => ({
  mockShowErrorBox: vi.fn(),
}));

vi.mock('electron', () => ({
  dialog: {
    showErrorBox: mockShowErrorBox,
  },
}));

vi.mock('../../../src/main/splashWindow', () => ({
  closeSplash: vi.fn(),
}));

describe('revealLoadFailure', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('closes splash, shows a live window, and displays a native error', () => {
    const window = {
      isDestroyed: vi.fn(() => false),
      show: vi.fn(),
    };

    revealLoadFailure(window as never, 'Could not load application');

    expect(closeSplash).toHaveBeenCalled();
    expect(window.show).toHaveBeenCalled();
    expect(mockShowErrorBox).toHaveBeenCalledWith('Concord Voice', 'Could not load application');
  });

  it('does not show a destroyed window but still displays a native error', () => {
    const window = {
      isDestroyed: vi.fn(() => true),
      show: vi.fn(),
    };

    revealLoadFailure(window as never, 'Could not load application');

    expect(closeSplash).toHaveBeenCalled();
    expect(window.show).not.toHaveBeenCalled();
    expect(mockShowErrorBox).toHaveBeenCalledWith('Concord Voice', 'Could not load application');
  });
});
