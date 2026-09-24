import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

const changelogCss = readFileSync(
  resolve(__dirname, '../../../src/renderer/components/ChangelogModal/ChangelogModal.css'),
  'utf-8'
);
const outgoingCallCss = readFileSync(
  resolve(__dirname, '../../../src/renderer/components/Voice/OutgoingCallModal.css'),
  'utf-8'
);
const keyRecoveryCss = readFileSync(
  resolve(__dirname, '../../../src/renderer/components/Auth/KeyRecoveryPrompt.css'),
  'utf-8'
);
const attestationCss = readFileSync(
  resolve(__dirname, '../../../src/renderer/components/AttestationFailedModal.css'),
  'utf-8'
);
const forceUpdateCss = readFileSync(
  resolve(__dirname, '../../../src/renderer/components/ui/ForceUpdateOverlay.css'),
  'utf-8'
);
const connectionLostCss = readFileSync(
  resolve(__dirname, '../../../src/renderer/components/ui/ConnectionLostOverlay.css'),
  'utf-8'
);

function rule(source: string, selector: string): string {
  const start = source.indexOf(`${selector} {`);
  const end = source.indexOf('}', start);
  return start === -1 || end === -1 ? '' : source.slice(start, end + 1);
}

function declaration(block: string, property: string): string | undefined {
  return block.match(new RegExp(`(?:^|\\n)\\s*${property}:\\s*([^;]+);`))?.[1]?.trim();
}

// Every block for exactly this selector, not only the first one.
function rules(source: string, selector: string): string {
  const blocks: string[] = [];
  let at = source.indexOf(`${selector} {`);
  while (at !== -1) {
    const end = source.indexOf('}', at);
    blocks.push(source.slice(at, end + 1));
    at = source.indexOf(`${selector} {`, end);
  }
  return blocks.join('\n');
}

function declarations(block: string, property: string): string[] {
  return [...block.matchAll(new RegExp(`(?:^|\\n)\\s*${property}:\\s*([^;]+);`, 'g'))].map((m) =>
    m[1].trim()
  );
}

describe('dialog placement regression (#2223)', () => {
  const changelog = rule(changelogCss, '.changelog-modal');
  const outgoingCall = rule(outgoingCallCss, '.outgoing-call-modal__backdrop');

  it('centers the larger changelog while retaining its responsive gutter', () => {
    expect(declaration(changelog, 'margin')).toBe('auto');
    expect(declaration(changelog, 'max-width')).toBe('640px');
    expect(declaration(changelog, 'width')).toBe('calc(100vw - 48px)');
  });

  it('anchors the outgoing-call prompt to the bottom-right corner', () => {
    expect(declaration(outgoingCall, 'inset')).toBe('auto 16px 16px auto');
  });

  it('removes native outer chrome from the outgoing-call prompt', () => {
    expect(declaration(outgoingCall, 'border')).toBe('none');
    expect(declaration(outgoingCall, 'background')).toBe('transparent');
  });

  // regression: global overlay unreachable over Settings/ui/Modal
  // ([internal]rules/frontend.md § "A global overlay must be reachable over
  // the Settings dialog", rule 5) — every showModal() dialog shell must
  // declare `margin: auto` itself, because the global `* { margin: 0 }`
  // reset in styles/index.css removes the UA margin that would otherwise
  // center it. `.changelog-modal` above carries it too.
  const shells = [
    ['.key-recovery-prompt', keyRecoveryCss],
    ['.attestation-modal', attestationCss],
    ['.force-update-overlay', forceUpdateCss],
    ['.connection-lost-overlay', connectionLostCss],
  ] as const;

  it.each(shells)('%s declares margin: auto on its showModal() dialog shell', (selector, css) => {
    const block = rule(css, selector);
    expect(
      declaration(block, 'margin'),
      `${selector} must declare "margin: auto" so the global "* { margin: 0 }" reset does not strip the UA centring on its showModal() dialog`
    ).toBe('auto');
  });

  // A modal dialog scrolls its own overflow (UA `overflow: auto`, with a
  // max-height inside the viewport). `overflow: visible` removes that, and a
  // fixed-position box cannot be scrolled any other way: measured on Electron
  // 44.4.3, a dialog taller than a 288px viewport left its bottom button at
  // 619px, unreachable. A zoomed or small window hides the overlay's actions.
  it.each(shells)('%s keeps its showModal() dialog shell scrollable', (selector, css) => {
    const blocking = declarations(rules(css, selector), 'overflow').filter(
      (value) => value !== 'auto' && value !== 'scroll'
    );
    expect(
      blocking,
      `${selector} may only set overflow to auto or scroll: a dialog taller than the window must scroll to its buttons`
    ).toEqual([]);
  });

  // The UA hides a closed dialog with `dialog:not([open]) { display: none }`,
  // and any author `display` on the dialog itself wins over it. The closed
  // dialog then paints in page flow: for a frame before the effect calls
  // showModal(), and between Escape's queued close and the unmount. Layout
  // belongs on `<selector>[open]` or on an inner card.
  it.each([...shells, ['.changelog-modal', changelogCss]] as const)(
    '%s leaves display to the UA until the dialog is open',
    (selector, css) => {
      expect(
        declarations(rules(css, selector), 'display'),
        `${selector} must not set display: it would show the dialog while it is closed`
      ).toEqual([]);
    }
  );
});
