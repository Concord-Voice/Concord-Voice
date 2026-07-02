import { afterEach, describe, expect, it } from 'vitest';

import { API_BASE, WS_BASE } from '@/renderer/config';
import {
  apiUrl,
  getApiBase,
  getWsBase,
  mediaUrl,
  resetRuntimeServerBase,
  setRuntimeServerBase,
} from '@/renderer/services/runtimeServerBase';

describe('runtimeServerBase', () => {
  afterEach(() => {
    resetRuntimeServerBase();
  });

  it('defaults to the build-time API and WebSocket bases', () => {
    expect(getApiBase()).toBe(API_BASE);
    expect(getWsBase()).toBe(WS_BASE);
    expect(apiUrl('/api/v1/users/me')).toBe(`${API_BASE}/api/v1/users/me`);
  });

  it('prepends a leading slash when the path lacks one', () => {
    expect(apiUrl('api/v1/users/me')).toBe(`${API_BASE}/api/v1/users/me`);
  });

  it('updates API and WebSocket bases from a validated HTTPS origin', () => {
    setRuntimeServerBase('https://homelab.lan:8443/setup');

    expect(getApiBase()).toBe('https://homelab.lan:8443');
    expect(getWsBase()).toBe('wss://homelab.lan:8443');
    expect(apiUrl('/api/v1/users/me')).toBe('https://homelab.lan:8443/api/v1/users/me');
  });

  it('maps localhost HTTP to ws for local self-hosted development', () => {
    setRuntimeServerBase('http://localhost:8080');

    expect(getApiBase()).toBe('http://localhost:8080');
    expect(getWsBase()).toBe('ws://localhost:8080');
  });

  it('resolves media paths against the active API base and leaves safe absolute URLs unchanged', () => {
    setRuntimeServerBase('https://homelab.lan');

    expect(mediaUrl('/api/v1/media/avatar.png')).toBe(
      'https://homelab.lan/api/v1/media/avatar.png'
    );
    expect(mediaUrl('https://cdn.example/avatar.png')).toBe('https://cdn.example/avatar.png');
    expect(mediaUrl('blob:app://concord/avatar')).toBe('blob:app://concord/avatar');
    expect(mediaUrl('javascript:alert(1)')).toBeUndefined();
  });

  it('resets to build-time defaults', () => {
    setRuntimeServerBase('https://homelab.lan');
    resetRuntimeServerBase();

    expect(getApiBase()).toBe(API_BASE);
    expect(getWsBase()).toBe(WS_BASE);
  });
});
