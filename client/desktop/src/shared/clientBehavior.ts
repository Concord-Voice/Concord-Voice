export type ToTrayChoice = 'close' | 'minimize' | 'none';
export type ToToolbarChoice = 'minimize' | 'close';

export interface ClientBehavior {
  toTray: ToTrayChoice;
  toToolbar: ToToolbarChoice;
}

export type CloseAction = 'tray' | 'toolbar' | 'quit';
export type MinimizeAction = 'tray' | 'toolbar';

// Default (#1099): [×] hides to the system tray (close-to-tray), the behavior
// #806 designed for. src/main/tray.ts is the affordance to reopen (left-click)
// or quit (context menu). The #1383-era interim default (toTray:'none') is
// retired; persisted snapshots of the exact interim combo are migrated by
// settingsStore's persist version-1 migration.
export const DEFAULT_CLIENT_BEHAVIOR: ClientBehavior = {
  toTray: 'close',
  toToolbar: 'minimize',
};

export function deriveCloseAction(cb: ClientBehavior): CloseAction {
  if (cb.toTray === 'close') return 'tray';
  if (cb.toToolbar === 'close') return 'toolbar';
  return 'quit';
}

export function deriveMinimizeAction(cb: ClientBehavior): MinimizeAction {
  if (cb.toTray === 'minimize') return 'tray';
  return 'toolbar';
}

// Mutex is the ONLY disable rule: the same destination value cannot be assigned
// to both buttons (toTray and toToolbar). There is deliberately no "coverage"
// rule — deriveCloseAction and deriveMinimizeAction are total functions, so
// every button always resolves to a destination ([×] can fall through to Quit;
// [—] is always 'tray' or 'toolbar'). A config where a button is "unclaimed"
// cannot exist, so disabling options to prevent one was dead logic that wrongly
// blocked the valid {toTray:'none', toToolbar:'close'} config (#1148).

function isMutexConflict(
  setting: 'toTray' | 'toToolbar',
  option: string,
  currentCB: ClientBehavior
): boolean {
  const otherValue = setting === 'toTray' ? currentCB.toToolbar : currentCB.toTray;
  return option === otherValue;
}

export function isOptionDisabled(
  setting: 'toTray' | 'toToolbar',
  option: string,
  currentCB: ClientBehavior
): boolean {
  return isMutexConflict(setting, option, currentCB);
}
