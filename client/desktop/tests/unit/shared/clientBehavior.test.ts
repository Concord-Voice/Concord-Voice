import { describe, it, expect } from 'vitest';
import {
  deriveCloseAction,
  deriveMinimizeAction,
  isOptionDisabled,
  type ClientBehavior,
} from '@/shared/clientBehavior';

const defaultCB: ClientBehavior = { toTray: 'close', toToolbar: 'minimize' };
const swapCB: ClientBehavior = { toTray: 'minimize', toToolbar: 'close' };
const quitCB: ClientBehavior = { toTray: 'none', toToolbar: 'minimize' };

describe('deriveCloseAction', () => {
  it('returns "tray" when [X] is the To-Tray button', () => {
    expect(deriveCloseAction(defaultCB)).toBe('tray');
  });

  it('returns "toolbar" when [X] is the To-Toolbar button', () => {
    expect(deriveCloseAction(swapCB)).toBe('toolbar');
  });

  it('returns "quit" when [X] is unclaimed (None+Minimize config)', () => {
    expect(deriveCloseAction(quitCB)).toBe('quit');
  });
});

describe('deriveMinimizeAction', () => {
  it('returns "toolbar" when [-] is the To-Toolbar button (default)', () => {
    expect(deriveMinimizeAction(defaultCB)).toBe('toolbar');
  });

  it('returns "tray" when [-] is the To-Tray button', () => {
    expect(deriveMinimizeAction(swapCB)).toBe('tray');
  });

  it('returns "toolbar" for the quit config (None+Minimize → [-] is toolbar)', () => {
    expect(deriveMinimizeAction(quitCB)).toBe('toolbar');
  });
});

describe('isOptionDisabled — mutex rule', () => {
  it('disables "close" on toTray when toToolbar is "close"', () => {
    expect(isOptionDisabled('toTray', 'close', swapCB)).toBe(true);
  });

  it('disables "minimize" on toTray when toToolbar is "minimize"', () => {
    expect(isOptionDisabled('toTray', 'minimize', defaultCB)).toBe(true);
  });

  it('disables "close" on toToolbar when toTray is "close"', () => {
    expect(isOptionDisabled('toToolbar', 'close', defaultCB)).toBe(true);
  });

  it('disables "minimize" on toToolbar when toTray is "minimize"', () => {
    expect(isOptionDisabled('toToolbar', 'minimize', swapCB)).toBe(true);
  });
});

describe('isOptionDisabled — no coverage violation possible (#1148)', () => {
  // deriveCloseAction / deriveMinimizeAction are total functions: every button
  // always resolves to a destination, so a "coverage violation" cannot occur.
  // The mutex is the only real disable rule. These configs are all valid and
  // must remain selectable.
  it('allows "close" on toToolbar when toTray is "none" (#1148 — both buttons → toolbar, quit via app-icon menu)', () => {
    expect(isOptionDisabled('toToolbar', 'close', quitCB)).toBe(false);
  });

  it('allows "none" on toTray when toToolbar is "close" (symmetric to #1148)', () => {
    const cb: ClientBehavior = { toTray: 'close', toToolbar: 'close' };
    expect(isOptionDisabled('toTray', 'none', cb)).toBe(false);
  });

  it('allows "none" on toTray when toToolbar is "minimize"', () => {
    expect(isOptionDisabled('toTray', 'none', defaultCB)).toBe(false);
  });
});

describe('isOptionDisabled — no false positives', () => {
  it('does not disable currently-selected options', () => {
    expect(isOptionDisabled('toTray', 'close', defaultCB)).toBe(false);
  });
});
