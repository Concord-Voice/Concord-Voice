// @vitest-environment-options { "url": "https://spa.example.com/" }
//
// Flat Cloudflare Pages host (post-#976, ADR-0015): the SPA is served at the
// origin root on a dedicated host. Verifies the renderer self-heal listeners
// fire for flat-host chunk failures (/assets/<hash>.js, no /spa/<sha>/ prefix)
// — exactly the URLs the stale SPA_CHUNK_URL_PATTERN silently stopped matching,
// which disabled self-heal on the production host (the founding bug).
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

declare global {
  interface Window {
    electron?: {
      spa?: {
        requestSelfHeal: (payload: {
          reason: 'chunk-load' | 'chunk-import-rejected';
          url?: string;
        }) => Promise<void>;
      };
    };
  }
}

import { installSelfHealHandlers } from '@/renderer/spaSelfHealClient';

describe('spaSelfHealClient — flat Pages host (#976)', () => {
  let mockRequestSelfHeal: ReturnType<typeof vi.fn>;
  let originalElectron: Window['electron'];

  beforeEach(() => {
    originalElectron = window.electron;
    mockRequestSelfHeal = vi.fn().mockResolvedValue(undefined);
    window.electron = { spa: { requestSelfHeal: mockRequestSelfHeal } };
    installSelfHealHandlers();
  });

  afterEach(() => {
    window.electron = originalElectron;
  });

  it('IPCs on a flat-host chunk error (/assets/<hash>.js, no /spa/<sha>/ prefix)', () => {
    const script = document.createElement('script');
    script.src = 'https://spa.example.com/assets/recoveryService-40XchBlv.js';
    document.body.appendChild(script);

    const event = new Event('error', { bubbles: false });
    Object.defineProperty(event, 'target', { value: script, writable: false });
    window.dispatchEvent(event);

    expect(mockRequestSelfHeal).toHaveBeenCalledWith({
      reason: 'chunk-load',
      url: 'https://spa.example.com/assets/recoveryService-40XchBlv.js',
    });
  });

  it('IPCs on a flat-host dynamic-import rejection', () => {
    const event = Object.assign(new Event('unhandledrejection'), {
      reason: new Error(
        'Failed to fetch dynamically imported module: https://spa.example.com/assets/resetService-YoryeXlC.js'
      ),
    });
    window.dispatchEvent(event);

    expect(mockRequestSelfHeal).toHaveBeenCalledWith({ reason: 'chunk-import-rejected' });
  });
});
