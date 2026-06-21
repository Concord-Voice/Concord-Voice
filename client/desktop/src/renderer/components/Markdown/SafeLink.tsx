import React, { type ReactNode, type MouseEvent } from 'react';

/**
 * SafeLink — defense-in-depth link renderer for the Markdown pipeline.
 *
 * Although rehype-sanitize already rejects non-http(s)/mailto protocols at the
 * hast level, SafeLink re-validates the protocol just before React emits the
 * anchor. This catches schema drift, allowlist typos, or any future bypass
 * where an unsafe href reaches the renderer.
 *
 * For safe hrefs we render `<a target="_blank" rel="noopener noreferrer">` and
 * additionally try to route the click through `window.electron.openExternal`
 * (when the preload bridge exposes it) so the OS browser is used explicitly.
 * If the bridge is absent we fall back to the anchor's default behavior —
 * Electron's main-process `setWindowOpenHandler` (see src/main/main.ts) then
 * re-validates and routes to `shell.openExternal`. Three layers of defense:
 * sanitizer → SafeLink → main-process window-open handler.
 */

// Only http, https, and mailto are considered safe. Anything else (javascript,
// data, file, blob, vbscript, ...) is rendered as inert text.
const SAFE_PROTOCOLS = /^(https?|mailto):/i;

// Narrow a value to a Promise-like type without leaning on `as` assertions.
// The preload bridge's openExternal return type is `Promise<unknown> | void`
// and we only want to attach a rejection handler when it actually resolved
// to a Promise. `in` + typeof is a TS-friendly narrowing path.
function isPromiseLike(value: unknown): value is Promise<unknown> {
  if (value === null || typeof value !== 'object') return false;
  if (!('catch' in value)) return false;
  return typeof value.catch === 'function';
}

interface SafeLinkProps {
  href?: string;
  title?: string;
  children?: ReactNode;
}

const SafeLink: React.FC<SafeLinkProps> = ({ href, title, children }) => {
  const isSafe = href !== undefined && SAFE_PROTOCOLS.test(href);
  if (!isSafe) {
    return <span className="unsafe-link">{children}</span>;
  }

  const handleClick = (e: MouseEvent<HTMLAnchorElement>): void => {
    // The preload bridge may expose an openExternal shortcut; if so, call it
    // directly so the click doesn't depend on Electron's setWindowOpenHandler
    // fallback. Guard every step — preload shape can drift between versions.
    // The main-process handler is authoritative on validation (returns
    // {ok, reason?}); we silently swallow the result here because the main
    // process also logs any denials. Attach .catch to prevent unhandled
    // rejection warnings if the IPC bridge ever throws (network / shutdown).
    const api = (
      globalThis as unknown as {
        electron?: { openExternal?: (url: string) => Promise<unknown> | void };
      }
    ).electron;
    if (api && typeof api.openExternal === 'function' && href) {
      e.preventDefault();
      const result: Promise<unknown> | void = api.openExternal(href);
      if (isPromiseLike(result)) {
        result.catch(() => {
          /* main-process logged the failure; renderer treats as no-op */
        });
      }
    }
    // Otherwise we let the default anchor activation happen; main-process
    // setWindowOpenHandler picks it up and routes to shell.openExternal.
  };

  return (
    <a href={href} title={title} rel="noopener noreferrer" target="_blank" onClick={handleClick}>
      {children}
    </a>
  );
};

export default SafeLink;
