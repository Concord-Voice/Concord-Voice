// Vite imports the CSS files as URLs to bundled asset files. This keeps all
// code-block highlighting offline (no CDN requests, no IP leak, no CSP
// expansion). The CSS files ship from highlight.js npm package.
import darkThemeUrl from 'highlight.js/styles/github-dark.css?url';
import lightThemeUrl from 'highlight.js/styles/github.css?url';

const THEME_URLS: Record<'light' | 'dark', string> = {
  light: lightThemeUrl,
  dark: darkThemeUrl,
};

export function loadHighlightTheme(mode: 'light' | 'dark'): void {
  const existing = document.querySelector<HTMLLinkElement>('link[data-hljs-theme]');
  if (existing?.dataset.hljsTheme === mode) {
    return; // Already mounted with the correct theme; nothing to do.
  }
  // Remove any stale theme link before adding the new one.
  document.querySelectorAll('link[data-hljs-theme]').forEach((n) => n.remove());
  const link = document.createElement('link');
  link.rel = 'stylesheet';
  link.href = THEME_URLS[mode];
  link.dataset.hljsTheme = mode;
  document.head.appendChild(link);
}
