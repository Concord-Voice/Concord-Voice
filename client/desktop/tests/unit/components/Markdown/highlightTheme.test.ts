import { describe, it, expect, beforeEach, vi } from 'vitest';

// Vitest doesn't process Vite's `?url` CSS imports the same way the dev server
// or build does — those imports resolve to empty strings in jsdom. Mock them
// so tests can assert on a realistic bundled-asset URL. The important contract
// is the `/styles/github*.css` path shape, which is what Vite emits in
// production (hashed or not, the basename survives).
vi.mock('highlight.js/styles/github-dark.css?url', () => ({
  default: '/assets/highlight-js-github-dark.css',
}));
vi.mock('highlight.js/styles/github.css?url', () => ({
  default: '/assets/highlight-js-github.css',
}));

import { loadHighlightTheme } from '@/renderer/components/Markdown/highlightTheme';

describe('highlightTheme', () => {
  beforeEach(() => {
    document.querySelectorAll('link[data-hljs-theme]').forEach((n) => n.remove());
  });

  it('injects a link tag for dark theme (bundled, not CDN)', () => {
    loadHighlightTheme('dark');
    const link = document.querySelector('link[data-hljs-theme]') as HTMLLinkElement | null;
    expect(link).not.toBeNull();
    expect(link?.href).toContain('github-dark');
    expect(link?.href).not.toContain('cdn.jsdelivr.net');
  });

  it('injects a link tag for light theme (bundled, not CDN)', () => {
    loadHighlightTheme('light');
    const link = document.querySelector('link[data-hljs-theme]') as HTMLLinkElement | null;
    expect(link).not.toBeNull();
    expect(link?.href).toContain('github');
    expect(link?.href).not.toContain('github-dark');
    expect(link?.href).not.toContain('cdn.jsdelivr.net');
  });

  it('switches themes by removing the old link and adding the new one', () => {
    loadHighlightTheme('dark');
    loadHighlightTheme('light');
    const links = document.querySelectorAll('link[data-hljs-theme]');
    expect(links.length).toBe(1);
    expect((links[0] as HTMLLinkElement).href).toContain('github');
    expect((links[0] as HTMLLinkElement).href).not.toContain('github-dark');
    expect((links[0] as HTMLLinkElement).href).not.toContain('cdn.jsdelivr.net');
  });

  it('is idempotent — repeated calls with the same theme do not churn the DOM', () => {
    loadHighlightTheme('dark');
    const linkBefore = document.querySelector('link[data-hljs-theme]');
    loadHighlightTheme('dark');
    loadHighlightTheme('dark');
    const links = document.querySelectorAll('link[data-hljs-theme]');
    // Still exactly one link…
    expect(links.length).toBe(1);
    // …and it is the SAME node — no swap happened (identity preserved).
    expect(links[0]).toBe(linkBefore);
  });
});
