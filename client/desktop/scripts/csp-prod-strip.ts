/**
 * CSP loopback-entry stripper for production builds.
 *
 * The renderer's CSP in `index.html` carries loopback origins (the HTTP
 * forms `http://localhost:*` and `http://127.0.0.1:*`, plus the WebSocket
 * and HTTPS loopback equivalents) because they're load-bearing for dev
 * mode (Vite at port 3001, API at port 8080). In production builds those
 * entries serve no purpose — the
 * renderer runs at `app://concord/` (bundled) or an HTTPS origin (remote SPA),
 * never at `localhost`.
 *
 * Keeping the loopback entries in the production bundle is a small
 * defense-in-depth gap: a hypothetical SPA-XSS could use `<img src>` to reach
 * a local-machine HTTP service for a CSRF-style GET (constrained — no-cors,
 * opaque response — but still strictly more reach than necessary). Stripping
 * them at build time closes the gap with zero functional impact in production.
 *
 * The strip logic is exported as a pure function so it can be unit-tested
 * without spinning up Vite; the plugin wrapper applies it only when
 * `transformIndexHtml` is invoked outside dev-server context.
 */

import type { Plugin } from 'vite';

/**
 * Remove dev-only loopback origins from any CSP-bearing HTML string.
 *
 * Idempotent: running it twice on the same input is equivalent to running it
 * once. Returns input unchanged if no loopback entries are present (the
 * common case after the first call).
 *
 * The replacements are scoped to whole tokens (each preceded by a space) so a
 * future allowlist entry like `https://localhost.example` (FQDN that happens
 * to start with `localhost.`) is unaffected — only the bare loopback wildcard
 * forms match.
 */
export function stripLoopbackCspEntries(html: string): string {
  return html
    .replaceAll(' http://localhost:*', '')
    .replaceAll(' http://127.0.0.1:*', '')
    .replaceAll(' ws://localhost:*', '')
    .replaceAll(' wss://localhost:*', '')
    .replaceAll(' https://localhost:*', '')
    .replaceAll(' ws://127.0.0.1:*', '');
}

/**
 * Vite plugin: strip loopback CSP entries on production builds; no-op in dev.
 *
 * `transformIndexHtml.handler` receives a `ctx` object whose `server` property
 * is defined during dev-server invocation and undefined during `vite build`.
 * That distinction is the gate we use; no env-var sniffing or `mode` checks
 * are needed.
 */
export function cspProdStripPlugin(): Plugin {
  return {
    name: 'csp-prod-strip',
    transformIndexHtml: {
      order: 'pre',
      handler(html, ctx) {
        if (ctx.server !== undefined) return html;
        return stripLoopbackCspEntries(html);
      },
    },
  };
}
