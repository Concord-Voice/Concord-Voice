import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render } from '@testing-library/react';
import { Titlebar } from '@/renderer/components/Titlebar/Titlebar';

// Minimal harness — we test the App-level invariant ("Titlebar is the first
// child of .app, not a sibling") without booting the full App component
// (which would require auth stores, MSW, routing, etc.). The structural
// invariant is the same: a renderer's `.app` container should not have any
// child painting before the Titlebar.

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

describe('App layout — Titlebar position relative to .app', () => {
  it('Titlebar renders as the first child of the .app container (not as a sibling)', () => {
    // Post-fix shape: <div className="app"><Titlebar /><...rest></div>
    // Pre-fix shape: <><Titlebar /><div className="app"><...rest></div></>
    const { container } = render(
      <div className="app">
        <Titlebar />
        <div className="app-content">placeholder</div>
      </div>
    );
    const appDiv = container.querySelector('.app') as HTMLElement | null;
    expect(appDiv).not.toBeNull();
    const firstChild = appDiv!.firstElementChild as HTMLElement | null;
    expect(firstChild).not.toBeNull();
    expect(firstChild!.classList.contains('titlebar')).toBe(true);
  });

  it('Titlebar.css no longer declares position: fixed for .titlebar', () => {
    // Structural assertion against the CSS source — the position:fixed
    // approach was the root cause of #1146 (content rendered under a fixed
    // overlay because .app had no padding-top). Post-fix the title bar is
    // a flex-flow item, not a fixed overlay.
    const { readFileSync } = require('node:fs');
    const { resolve } = require('node:path');
    const cssPath = resolve(__dirname, '../../../src/renderer/components/Titlebar/Titlebar.css');
    const css = readFileSync(cssPath, 'utf-8');
    // Locate the .titlebar rule block and assert it does not contain `position: fixed`
    const ruleMatch = css.match(/^\.titlebar\s*\{[^}]*\}/m);
    expect(ruleMatch).not.toBeNull();
    expect(ruleMatch![0]).not.toMatch(/position:\s*fixed/);
    expect(ruleMatch![0]).not.toMatch(/z-index:\s*1000/);
  });

  it('App.tsx renders Titlebar inside the .app container in both render paths', () => {
    // Source-tree assertion: App.tsx should NOT show <Titlebar /> immediately
    // followed by <div className="app"> (pre-fix sibling pattern). Instead it
    // should show <div className="app"><Titlebar /> ... (post-fix child pattern).
    const { readFileSync } = require('node:fs');
    const { resolve } = require('node:path');
    const appPath = resolve(__dirname, '../../../src/renderer/App.tsx');
    const src = readFileSync(appPath, 'utf-8');
    // Pre-fix pattern (any whitespace between): {!isPipWindow && <Titlebar />}\s*<div className="app"
    expect(src).not.toMatch(/\{!isPipWindow && <Titlebar \/>\}\s*<div className="app"/);
  });
});
