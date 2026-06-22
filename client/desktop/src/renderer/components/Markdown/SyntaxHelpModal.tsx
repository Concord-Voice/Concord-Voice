import React, { useEffect, useRef } from 'react';
import { useEntitlement } from '../../hooks/useEntitlement';
import { clampMessageCharsForTier } from '../../utils/entitlementLimits';
import './SyntaxHelpModal.css';

// jsdom (the vitest test environment) implements <dialog> but does NOT fire
// the native `cancel` / `close` events on Escape keypress when using
// .showModal() fallback. Production browsers (Electron 41 / Chrome 137+) DO
// fire them. We detect "native dialog cancel events don't work here" via a
// feature probe and mount a fallback globalThis keydown handler only in that
// case — avoiding duplicate onClose calls in production where both paths
// would otherwise fire.
const dialogCancelsOnEscape = (() => {
  if (typeof document === 'undefined') return true;
  // jsdom exposes HTMLDialogElement but its showModal() is a stub — the
  // `cancel`/`close` events don't bubble through the React synthetic layer
  // in that environment. Chrome/Electron wire these events as real DOM
  // MouseEvent-like objects that React subscribes to natively.
  return globalThis.navigator !== undefined && !/jsdom/i.test(globalThis.navigator.userAgent ?? '');
})();

interface Construct {
  source: string;
  rendered: string;
}

const CONSTRUCTS: Construct[] = [
  { source: '**bold**', rendered: 'bold (emphasized)' },
  { source: '*italic*', rendered: 'italic' },
  { source: '~~strikethrough~~', rendered: 'strikethrough' },
  { source: '`inline code`', rendered: 'inline code' },
  {
    source: '```\ncode block\n```',
    rendered: 'Fenced code block (opener and closer on their own lines)',
  },
  { source: '> quote', rendered: 'Indented quote' },
  { source: '- item', rendered: 'Bulleted list' },
  { source: '1. item', rendered: 'Numbered list' },
  { source: '[link](https://example.com)', rendered: 'link' },
  { source: 'https://example.com', rendered: 'auto-linked URL' },
  { source: '||spoiler||', rendered: '(click to reveal)' },
  { source: '# H1 / ## H2 / ### H3', rendered: 'Heading text' },
  { source: '---', rendered: '(horizontal rule)' },
  { source: ':smile:', rendered: '😄' },
];

interface Props {
  open: boolean;
  onClose: () => void;
}

const SyntaxHelpModal: React.FC<Props> = ({ open, onClose }) => {
  const dialogRef = useRef<HTMLDialogElement>(null);
  // Show the user's actual per-message cap (free 5120 / premium 10240), matching
  // the entitlement-driven limit MessageInput now enforces (#1299).
  const entitlementTier = useEntitlement((e) => e.tier);
  const maxMessageChars = useEntitlement((e) => e.maxMessageChars);
  const messageLimit = clampMessageCharsForTier(entitlementTier, maxMessageChars);

  // Fallback Escape handler — only installed when the runtime cannot rely on
  // <dialog>'s native cancel event (i.e., under jsdom). In Electron/Chrome
  // the dialog's onClose handler below receives Escape natively. This
  // conditional wiring avoids double-firing onClose in production.
  useEffect(() => {
    if (!open) return;
    if (dialogCancelsOnEscape) return;
    const handler = (e: KeyboardEvent): void => {
      if (e.key === 'Escape') onClose();
    };
    globalThis.addEventListener('keydown', handler);
    return () => {
      globalThis.removeEventListener('keydown', handler);
    };
  }, [open, onClose]);

  // Sync the imperative <dialog> open state with the React-controlled `open` prop.
  // showModal()/close() also moves focus into / out of the dialog for screen readers.
  useEffect(() => {
    const el = dialogRef.current;
    if (!el) return;
    if (open && !el.open) {
      // jsdom and some older renderers don't implement showModal — fall back to .show()
      if (typeof el.showModal === 'function') {
        try {
          el.showModal();
        } catch {
          el.setAttribute('open', '');
        }
      } else {
        el.setAttribute('open', '');
      }
    } else if (!open && el.open) {
      el.close();
    }
  }, [open]);

  if (!open) return null;

  // Close paths (two independent, both accessible):
  //   1. Escape key — in production, handled by <dialog> showModal()'s
  //      native cancel → close event (which fires React's onClose). Under
  //      jsdom the dialog doesn't wire those events, so the fallback
  //      globalThis keydown listener above installs (gated on
  //      `dialogCancelsOnEscape`) to cover that environment.
  //   2. Explicit X button in the header (below).
  // Backdrop-click-to-close is intentionally NOT provided: attaching an
  // onClick handler to the <dialog> trips jsx-a11y's rule that treats
  // <dialog> as non-interactive. The two close paths above cover the UX
  // without needing a click listener on the dialog element itself.

  return (
    <dialog
      ref={dialogRef}
      className="syntax-help-modal-backdrop"
      aria-modal="true"
      aria-labelledby="syntax-help-modal-title"
      onClose={onClose}
    >
      <div className="syntax-help-modal">
        <header>
          <h2 id="syntax-help-modal-title">Supported Markdown Syntax</h2>
          <button type="button" aria-label="Close" onClick={onClose}>
            ×
          </button>
        </header>
        <table>
          <thead>
            <tr>
              <th>Source</th>
              <th>Rendered</th>
            </tr>
          </thead>
          <tbody>
            {CONSTRUCTS.map((c) => (
              <tr key={c.source}>
                <td>
                  <code>{c.source}</code>
                </td>
                <td>{c.rendered}</td>
              </tr>
            ))}
          </tbody>
        </table>
        <footer>
          <p>
            Tip: Fenced code blocks need the opening and closing triple-backticks each on their own
            line. The text after the opening backticks is treated as the language identifier (used
            for syntax highlighting).
          </p>
          <p>
            Up to {messageLimit.toLocaleString('en-US')} characters per message. Longer messages are
            sent as a .md attachment.
          </p>
        </footer>
      </div>
    </dialog>
  );
};

export default SyntaxHelpModal;
