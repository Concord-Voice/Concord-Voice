import { readdirSync, readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { beforeEach, describe, expect, it } from 'vitest';
import { isSyncSuppressed } from '@/renderer/stores/colorSyncSuppression';
import { useDraftSettingsStore } from '@/renderer/stores/draftSettingsStore';
import { usePermissionStore } from '@/renderer/stores/permissionStore';
import { resetAllStores } from '../../helpers/store-helpers';

const storesDirectory = resolve(process.cwd(), 'src/renderer/stores');
const storeHelpersFile = resolve(process.cwd(), 'tests/helpers/store-helpers.ts');
const resetActions = new Set([
  'clearAccessToken',
  'clearAll',
  'clearChallenge',
  'clearChannels',
  'clearDMs',
  'clearFriends',
  'clearInvites',
  'clearMembers',
  'clearPending',
  'clearPrivacy',
  'clearServers',
  'clearUser',
  'dismiss',
  'reset',
  'setAllowNsfwContent',
  'setLoginNotice',
  'setRememberMe',
  'teardown',
]);

function resetBindingsIn(helperSource: string): Set<string> {
  const resetFunctionSource = helperSource.match(
    /^export function resetAllStores\(\): void \{[\s\S]*?^\}/m
  )?.[0];
  if (!resetFunctionSource) {
    throw new Error('resetAllStores() body not found');
  }

  const bindings: string[] = [];
  for (const match of resetFunctionSource.matchAll(
    /^\s*(?:(use\w+Store)\.setState\(\s*\{\s*(?=\w)|(use\w+Store)\.getState\(\)\.(\w+)\(|resetToInitialState\((use\w+Store)\))/gm
  )) {
    const binding = match[1] ?? match[2] ?? match[4];
    const action = match[3];
    if (binding && (!action || resetActions.has(action))) bindings.push(binding);
  }
  return new Set(bindings);
}

describe('resetAllStores', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('does not count non-reset store calls or property access as a reset', () => {
    const helperSource = `export function resetAllStores(): void {
  useFutureStore.getState;
  useFutureStore.setState;
  useFutureStore.getInitialState;
  useFutureStore.getState();
  useFutureStore.getInitialState();
  useFutureStore.getState().initialize();
  useFutureStore.setState({});
}`;

    expect(resetBindingsIn(helperSource)).toEqual(new Set());
  });

  it('covers every renderer store module with a reset call', () => {
    const storeModules = readdirSync(storesDirectory)
      .filter((name) => name.endsWith('Store.ts'))
      .map((name) => name.replace(/\.ts$/, ''))
      .sort();
    const helperSource = readFileSync(storeHelpersFile, 'utf8');
    const storeImports = [
      ...helperSource.matchAll(
        /import\s+\{\s*(use\w+Store)\s*\}\s+from\s+['"]\.\.\/\.\.\/src\/renderer\/stores\/([^'"]+Store)['"];?/g
      ),
    ].map((match) => ({ binding: match[1], module: match[2] }));

    expect(
      storeImports.map(({ module }) => module).sort(),
      'store-helpers must import every renderer store module'
    ).toEqual(storeModules);
    expect(
      [...resetBindingsIn(helperSource)].sort(),
      'resetAllStores must execute a reset for every imported store'
    ).toEqual(storeImports.map(({ binding }) => binding).sort());
  });

  it('clears permission state and persisted browser storage (#1637)', () => {
    usePermissionStore.setState({
      serverRoles: { 'server-1': [] },
      roleViewer: { 'server-1': { kind: 'owner' } },
      serverPermissions: { 'server-1': 1n },
      channelPermissions: { 'channel-1': 2n },
      channelOverrides: { 'channel-1': [] },
    });
    localStorage.setItem('store-reset-test', 'dirty');
    sessionStorage.setItem('store-reset-test', 'dirty');

    expect(
      usePermissionStore.getState().roleViewer,
      'fixture must seed permissionStore state'
    ).not.toEqual({});
    expect(localStorage.length, 'fixture must seed localStorage').toBe(1);
    expect(sessionStorage.length, 'fixture must seed sessionStorage').toBe(1);
    resetAllStores();

    expect(localStorage.length, 'resetAllStores must clear localStorage').toBe(0);
    expect(sessionStorage.length, 'resetAllStores must clear sessionStorage').toBe(0);

    const permissionState = usePermissionStore.getState();
    expect(
      {
        serverRoles: permissionState.serverRoles,
        roleViewer: permissionState.roleViewer,
        serverPermissions: permissionState.serverPermissions,
        channelPermissions: permissionState.channelPermissions,
        channelOverrides: permissionState.channelOverrides,
      },
      'resetAllStores must clear permissionStore state'
    ).toEqual({
      serverRoles: {},
      roleViewer: {},
      serverPermissions: {},
      channelPermissions: {},
      channelOverrides: {},
    });
  });

  it('tears down external draft-settings state (#1637)', () => {
    useDraftSettingsStore.getState().initialize();
    expect(isSyncSuppressed(), 'fixture must enable color-sync suppression').toBe(true);

    try {
      resetAllStores();
      expect(
        isSyncSuppressed(),
        'resetAllStores must clear draft-settings color-sync suppression'
      ).toBe(false);
    } finally {
      useDraftSettingsStore.getState().teardown();
    }
  });
});
