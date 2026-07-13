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

function rule(source: string, selector: string): string {
  const start = source.indexOf(`${selector} {`);
  const end = source.indexOf('}', start);
  return start === -1 || end === -1 ? '' : source.slice(start, end + 1);
}

function declaration(block: string, property: string): string | undefined {
  return block.match(new RegExp(`(?:^|\\n)\\s*${property}:\\s*([^;]+);`))?.[1]?.trim();
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
});
