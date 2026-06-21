import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render } from '@testing-library/react';
import { Titlebar } from '@/renderer/components/Titlebar/Titlebar';

const mockGet = vi.fn();
const mockOnChange = vi.fn();

beforeEach(() => {
  vi.clearAllMocks();
  (window as unknown as { electron: unknown }).electron = {
    version: {
      get: mockGet,
      onChange: mockOnChange,
    },
  };
  mockGet.mockResolvedValue({ appVersion: '0.1.43', spaHash: null });
  mockOnChange.mockReturnValue(() => {});
});

describe('Titlebar theme application', () => {
  it('does not use the hardcoded fallback color #1a1a1a in the inline style', () => {
    // The pre-fix CSS referenced var(--titlebar-bg, #1a1a1a) where --titlebar-bg
    // is never declared anywhere — so the fallback always won. After the fix
    // the rule uses var(--bg-secondary), an actual theme token.
    const { container } = render(<Titlebar />);
    const titlebar = container.querySelector('.titlebar') as HTMLElement | null;
    expect(titlebar).not.toBeNull();
    // No inline `style` should leak the hardcoded fallback hex. The pre-fix
    // bug was at the CSS-rule level, not inline-style, so this assertion is
    // a sanity check on the rendered DOM. The substantive check is the
    // grep-style assertion in Step 2 below: no `--titlebar-bg` references
    // remain in the source tree.
    const inlineStyle = titlebar!.getAttribute('style') ?? '';
    expect(inlineStyle).not.toMatch(/#1a1a1a/i);
    expect(inlineStyle).not.toMatch(/#ffffff/i);
  });

  it('the Titlebar component is rendered (smoke test for theme.test setup)', () => {
    const { container } = render(<Titlebar />);
    expect(container.querySelector('.titlebar')).not.toBeNull();
  });
});

import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

describe('Titlebar CSS source-tree invariants', () => {
  it('Titlebar.css does not reference --titlebar-bg or --titlebar-fg', () => {
    const cssPath = resolve(
      __dirname,
      '../../../../../src/renderer/components/Titlebar/Titlebar.css'
    );
    const css = readFileSync(cssPath, 'utf-8');
    expect(css).not.toMatch(/--titlebar-bg/);
    expect(css).not.toMatch(/--titlebar-fg/);
  });

  it('Titlebar.css uses --bg-secondary and --text-primary theme tokens', () => {
    const cssPath = resolve(
      __dirname,
      '../../../../../src/renderer/components/Titlebar/Titlebar.css'
    );
    const css = readFileSync(cssPath, 'utf-8');
    expect(css).toMatch(/var\(--bg-secondary\)/);
    expect(css).toMatch(/var\(--text-primary\)/);
  });

  it('index.css does not contain the dead .title-bar (hyphenated) rule', () => {
    const cssPath = resolve(__dirname, '../../../../../src/renderer/styles/index.css');
    const css = readFileSync(cssPath, 'utf-8');
    // Look for the rule selector, not just substring (since `.titlebar-title`
    // and other dot-prefixed selectors also contain the substring).
    expect(css).not.toMatch(/^\.title-bar\s*\{/m);
    expect(css).not.toMatch(/^\.title-bar-text\s*\{/m);
    expect(css).not.toMatch(/^\.title-bar-info\s*\{/m);
  });
});
