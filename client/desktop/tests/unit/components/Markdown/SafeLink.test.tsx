import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent } from '../../../test-utils';
import SafeLink from '@/renderer/components/Markdown/SafeLink';

// SafeLink reads from `window.electron.openExternal` when present. The preload
// bridge (client/desktop/src/preload/preload.ts) exposes the Electron API under
// `window.electron`; this test attaches a mock openExternal to that namespace
// (tests/setup.ts already installs a base `window.electron` mock). If the API
// is ever absent at runtime the component falls back to the raw anchor, which
// Electron's setWindowOpenHandler (src/main/main.ts) routes to
// shell.openExternal — double-layered defense.
const installOpenExternalMock = (openExternal: ReturnType<typeof vi.fn>): void => {
  const electron = (window as unknown as { electron?: Record<string, unknown> }).electron;
  if (!electron) {
    throw new Error('window.electron must be pre-installed by tests/setup.ts');
  }
  electron.openExternal = openExternal;
};

describe('SafeLink', () => {
  beforeEach(() => {
    installOpenExternalMock(vi.fn());
  });

  it('renders anchor with http protocol', () => {
    render(<SafeLink href="http://example.com">ex</SafeLink>);
    const link = screen.getByText('ex');
    expect(link.tagName).toBe('A');
    expect(link).toHaveAttribute('href', 'http://example.com');
  });

  it('renders anchor with https and mailto', () => {
    render(<SafeLink href="https://example.com">https</SafeLink>);
    expect(screen.getByText('https')).toHaveAttribute('href', 'https://example.com');
    render(<SafeLink href="mailto:a@b.com">mail</SafeLink>);
    expect(screen.getByText('mail')).toHaveAttribute('href', 'mailto:a@b.com');
  });

  it('renders as plain text for javascript: href', () => {
    render(<SafeLink href="javascript:alert(1)">bad</SafeLink>);
    expect(screen.getByText('bad').tagName).toBe('SPAN');
  });

  it('renders as plain text for data: href', () => {
    render(<SafeLink href="data:text/html,abc">bad</SafeLink>);
    expect(screen.getByText('bad').tagName).toBe('SPAN');
  });

  it('renders as plain text for file: and blob: hrefs', () => {
    render(<SafeLink href="file:///etc/passwd">f</SafeLink>);
    expect(screen.getByText('f').tagName).toBe('SPAN');
    render(<SafeLink href="blob:http://x/1">b</SafeLink>);
    expect(screen.getByText('b').tagName).toBe('SPAN');
  });

  it('calls window.electron.openExternal on click', () => {
    const openExternal = vi.fn();
    installOpenExternalMock(openExternal);
    render(<SafeLink href="https://example.com">ex</SafeLink>);
    fireEvent.click(screen.getByText('ex'));
    expect(openExternal).toHaveBeenCalledWith('https://example.com');
  });
});
