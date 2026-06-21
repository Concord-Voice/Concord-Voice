// HTMLDialogElement polyfill — jsdom 29 does not implement showModal/close.
// Components using the native <dialog> element call these imperatively; in
// the browser they work, but in jsdom we need a minimal shim that toggles
// the `open` attribute and dispatches the close event so React listeners fire.
// Each method is guarded individually so a future jsdom version that lands
// `close()` before `showModal()` (or vice versa) doesn't get its native
// implementation masked by the polyfill.
{
  const proto = (
    globalThis as typeof globalThis & { HTMLDialogElement?: { prototype: HTMLDialogElement } }
  ).HTMLDialogElement?.prototype;
  if (proto) {
    if (typeof proto.showModal !== 'function') {
      proto.showModal = function showModal(this: HTMLDialogElement) {
        this.setAttribute('open', '');
      };
    }
    if (typeof proto.close !== 'function') {
      proto.close = function close(this: HTMLDialogElement) {
        this.removeAttribute('open');
        this.dispatchEvent(new Event('close'));
      };
    }
  }
}

// Fix localStorage: modern Node versions expose `globalThis.localStorage` as a stub
// without working methods (setItem/getItem). Zustand persist middleware references
// the bare `localStorage` global. Replace it with a proper in-memory implementation.
{
  const store = new Map<string, string>();
  const storage = {
    getItem: (key: string) => store.get(key) ?? null,
    setItem: (key: string, value: string) => {
      store.set(key, value);
    },
    removeItem: (key: string) => {
      store.delete(key);
    },
    clear: () => {
      store.clear();
    },
    get length() {
      return store.size;
    },
    key: (index: number) => [...store.keys()][index] ?? null,
  };
  Object.defineProperty(globalThis, 'localStorage', {
    value: storage,
    writable: true,
    configurable: true,
  });
}

import '@testing-library/jest-dom/vitest';
import { cleanup } from '@testing-library/react';
import { afterEach, vi } from 'vitest';

// Auto-cleanup after each test
afterEach(() => {
  cleanup();
});

// Provide Web Crypto API from Node's built-in webcrypto (jsdom lacks crypto.subtle)
if (!globalThis.crypto?.subtle) {
  const { webcrypto } = require('node:crypto');
  Object.defineProperty(globalThis, 'crypto', { value: webcrypto });
}

// Browser-specific mocks — only applied in jsdom environment (skipped for @vitest-environment node)
if (typeof window !== 'undefined') {
  // Mock window.electron (Electron preload bridge)
  Object.defineProperty(window, 'electron', {
    value: {
      getVersion: vi.fn().mockResolvedValue('0.1.0-test'),
      getPlatform: vi.fn().mockResolvedValue('darwin'),
      getClientId: vi.fn().mockResolvedValue('test-client-id'),
      // OS permission management (#197)
      checkAllPermissions: vi.fn().mockResolvedValue({
        microphone: 'granted',
        camera: 'granted',
        screen: 'granted',
        secureStorage: 'granted',
        notifications: 'granted',
      }),
      checkPermission: vi.fn().mockResolvedValue('granted'),
      requestPermission: vi.fn().mockResolvedValue('granted'),
      openPermissionSettings: vi.fn().mockResolvedValue(undefined),
      onPermissionChanged: vi.fn().mockReturnValue(() => {}),
    },
    writable: true,
  });

  // Mock IntersectionObserver (jsdom doesn't support it)
  class MockIntersectionObserver {
    observe = vi.fn();
    unobserve = vi.fn();
    disconnect = vi.fn();
  }
  Object.defineProperty(window, 'IntersectionObserver', {
    value: MockIntersectionObserver,
    writable: true,
  });

  // Mock ResizeObserver (jsdom doesn't support it)
  class MockResizeObserver {
    observe = vi.fn();
    unobserve = vi.fn();
    disconnect = vi.fn();
  }
  Object.defineProperty(window, 'ResizeObserver', {
    value: MockResizeObserver,
    writable: true,
  });

  // Mock HTMLCanvasElement — jsdom doesn't support canvas rendering context
  const noop = () => {};
  HTMLCanvasElement.prototype.getContext = vi.fn(() => ({
    clearRect: noop,
    drawImage: noop,
    fillRect: noop,
    strokeRect: noop,
    beginPath: noop,
    arc: noop,
    fill: noop,
    stroke: noop,
    save: noop,
    restore: noop,
    scale: noop,
    setTransform: noop,
    globalCompositeOperation: 'source-over',
    fillStyle: '',
    strokeStyle: '',
    lineWidth: 1,
  })) as unknown as typeof HTMLCanvasElement.prototype.getContext;
  HTMLCanvasElement.prototype.toBlob = vi.fn(function (_cb: BlobCallback) {
    _cb(new Blob(['mock'], { type: 'image/png' }));
  }) as unknown as typeof HTMLCanvasElement.prototype.toBlob;
  HTMLCanvasElement.prototype.toDataURL = vi.fn(() => 'data:image/png;base64,mock');

  // Mock URL.createObjectURL / revokeObjectURL (jsdom doesn't support blob URLs)
  if (!URL.createObjectURL) {
    URL.createObjectURL = vi.fn(() => 'blob:mock-url');
  }
  if (!URL.revokeObjectURL) {
    URL.revokeObjectURL = vi.fn();
  }

  // Mock navigator.clipboard — jsdom defines it as a getter-only property so
  // Object.assign() throws. Use Object.defineProperty to install writable vi.fn()s.
  Object.defineProperty(navigator, 'clipboard', {
    value: {
      writeText: vi.fn().mockResolvedValue(undefined),
      readText: vi.fn().mockResolvedValue(''),
    },
    writable: true,
    configurable: true,
  });
}

// Suppress React act() warnings in test output
const originalError = console.error;
console.error = (...args: unknown[]) => {
  if (typeof args[0] === 'string' && args[0].includes('act(')) return;
  originalError(...args);
};
