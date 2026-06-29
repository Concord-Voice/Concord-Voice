import {
  app,
  dialog,
  nativeImage,
  type MessageBoxSyncOptions,
  type MoveToApplicationsFolderOptions,
  type NativeImage,
} from 'electron';
import fs from 'node:fs';
import path from 'node:path';

const INSTALL_PREFERENCES_FILE = 'install-preferences.json';

export type ApplicationsFolderDecision = 'in-applications' | 'needs-move' | 'not-applicable';

export interface ApplicationsFolderCheckInput {
  isPackaged: boolean;
  platform: NodeJS.Platform;
  isInApplicationsFolder: boolean;
  envSkip?: string;
}

export interface ApplicationsFolderGateDeps {
  isPackaged: boolean;
  platform: NodeJS.Platform;
  envSkip?: string;
  isInApplicationsFolder: () => boolean;
  getVersion: () => string;
  getUserDataPath: () => string;
  getIcon: () => NativeImage | undefined;
  showMessageBoxSync: (options: MessageBoxSyncOptions) => number;
  moveToApplicationsFolder: (options: MoveToApplicationsFolderOptions) => boolean;
  now?: () => Date;
  warn?: (message: string) => void;
}

interface InstallPreferences {
  suppressedAt: string;
  suppressedForVersion: string;
}

type ConflictType = Parameters<NonNullable<MoveToApplicationsFolderOptions['conflictHandler']>>[0];

export function checkApplicationsFolder(
  input: ApplicationsFolderCheckInput
): ApplicationsFolderDecision {
  if (!input.isPackaged || input.platform !== 'darwin' || input.envSkip) return 'not-applicable';
  return input.isInApplicationsFolder ? 'in-applications' : 'needs-move';
}

export function maybePromptMove(deps: ApplicationsFolderGateDeps = createDefaultDeps()): boolean {
  if (!deps.isPackaged || deps.platform !== 'darwin' || deps.envSkip) return false;

  const decision = checkApplicationsFolder({
    isPackaged: deps.isPackaged,
    platform: deps.platform,
    envSkip: deps.envSkip,
    isInApplicationsFolder: deps.isInApplicationsFolder(),
  });
  if (decision !== 'needs-move') return false;

  const version = deps.getVersion();
  const preferences = readInstallPreferences(deps);
  if (preferences?.suppressedForVersion === version) return false;

  const isReprompt = Boolean(preferences?.suppressedForVersion);
  const response = deps.showMessageBoxSync(createPromptOptions(isReprompt, deps.getIcon()));
  if (response !== 1) {
    writeInstallPreferences(deps, version);
    return false;
  }

  try {
    const moved = deps.moveToApplicationsFolder({
      conflictHandler: (conflictType) => handleMoveConflict(conflictType, deps),
    });
    if (moved) clearInstallPreferences(deps);
    return moved;
  } catch (err) {
    deps.warn?.(`[ApplicationsFolderGate] move failed: ${(err as Error).message}`);
    return false;
  }
}

function createDefaultDeps(): ApplicationsFolderGateDeps {
  return {
    isPackaged: app.isPackaged,
    platform: process.platform,
    envSkip: process.env.CONCORD_DEV_SKIP_MOVE_PROMPT,
    isInApplicationsFolder: () => app.isInApplicationsFolder(),
    getVersion: () => app.getVersion(),
    getUserDataPath: () => app.getPath('userData'),
    getIcon: () => {
      const icon = nativeImage.createFromPath(path.join(app.getAppPath(), 'build', 'icon.icns'));
      return icon.isEmpty() ? undefined : icon;
    },
    showMessageBoxSync: (options) => dialog.showMessageBoxSync(options),
    moveToApplicationsFolder: (options) => app.moveToApplicationsFolder(options),
    warn: (message) => console.warn(message),
  };
}

function createPromptOptions(
  isReprompt: boolean,
  icon: NativeImage | undefined
): MessageBoxSyncOptions {
  if (isReprompt) {
    return {
      type: 'info',
      buttons: ['Remind Me Later', 'Move to Applications'],
      defaultId: 1,
      cancelId: 0,
      title: 'Move Concord Voice to Applications',
      message:
        'A new version is available, but updates may not install reliably while Concord Voice runs from outside Applications.',
      detail: 'Moving the app now lets future updates install seamlessly. Your data is preserved.',
      icon,
    };
  }

  return {
    type: 'question',
    buttons: ['Not Now', 'Move to Applications'],
    defaultId: 1,
    cancelId: 0,
    title: 'Move Concord Voice to Applications?',
    message:
      'Concord Voice is currently running from outside your Applications folder. Moving it ensures secure, reliable updates and proper macOS integration.',
    detail: 'Your messages, channel keys, and account settings will remain unchanged.',
    icon,
  };
}

function handleMoveConflict(conflictType: ConflictType, deps: ApplicationsFolderGateDeps): boolean {
  if (conflictType === 'exists') return true;

  deps.showMessageBoxSync({
    type: 'warning',
    buttons: ['OK'],
    defaultId: 0,
    cancelId: 0,
    title: 'Concord Voice is already running',
    message: 'Another copy of Concord Voice is currently running.',
    detail: 'Please quit the other copy and try again.',
    icon: deps.getIcon(),
  });
  return false;
}

function installPreferencesPath(deps: ApplicationsFolderGateDeps): string {
  return path.join(deps.getUserDataPath(), INSTALL_PREFERENCES_FILE);
}

function readInstallPreferences(deps: ApplicationsFolderGateDeps): InstallPreferences | null {
  try {
    const parsed = JSON.parse(fs.readFileSync(installPreferencesPath(deps), 'utf-8')) as unknown;
    if (!parsed || typeof parsed !== 'object') return null;
    const pref = parsed as Partial<InstallPreferences>;
    if (typeof pref.suppressedAt !== 'string') return null;
    if (typeof pref.suppressedForVersion !== 'string') return null;
    return { suppressedAt: pref.suppressedAt, suppressedForVersion: pref.suppressedForVersion };
  } catch {
    return null;
  }
}

function writeInstallPreferences(deps: ApplicationsFolderGateDeps, version: string): void {
  try {
    const filePath = installPreferencesPath(deps);
    fs.mkdirSync(path.dirname(filePath), { recursive: true });
    fs.writeFileSync(
      filePath,
      JSON.stringify({
        suppressedAt: (deps.now ?? (() => new Date()))().toISOString(),
        suppressedForVersion: version,
      }),
      'utf-8'
    );
  } catch (err) {
    deps.warn?.(
      `[ApplicationsFolderGate] failed to persist suppression: ${(err as Error).message}`
    );
  }
}

function clearInstallPreferences(deps: ApplicationsFolderGateDeps): void {
  try {
    fs.rmSync(installPreferencesPath(deps), { force: true });
  } catch {
    // Best effort: once the app is in /Applications, stale suppression is harmless.
  }
}
