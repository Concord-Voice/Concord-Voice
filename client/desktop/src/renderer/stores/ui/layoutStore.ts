import { create } from 'zustand';
import { persist, devtools, subscribeWithSelector } from 'zustand/middleware';
import { wrapStore } from '../../utils/runtime/createStore';

export interface ServerFolder {
  id: string;
  name: string;
  serverIds: string[];
}

export type SidebarContext = 'dm' | 'server';
export type SidebarSide = 'left' | 'right';

export interface SidebarDockPreference {
  width: number;
  pinned: boolean;
}

export interface SidebarProfile {
  left: SidebarDockPreference;
  right: SidebarDockPreference;
}

export interface SidebarProfiles {
  dm: SidebarProfile;
  server: SidebarProfile;
}

export const SIDEBAR_MIN_WIDTH = 44;
/**
 * The pre-#2653 rail width. A legacy `'collapsed'` panel migrates to this, not
 * to SIDEBAR_MIN_WIDTH — the user's stored intent was "the old rail", not the
 * new floor. Deliberately distinct from SIDEBAR_MIN_WIDTH; do not merge them.
 */
export const LEGACY_COLLAPSED_WIDTH = 56;
export const SIDEBAR_COMPACT_BREAKPOINT = 135;
export const LEFT_SIDEBAR_MAX_WIDTH = 400;
export const RIGHT_SIDEBAR_MAX_WIDTH = 340;

const DEFAULT_SIDEBAR_PROFILES: SidebarProfiles = {
  dm: {
    left: { width: 240, pinned: true },
    right: { width: 260, pinned: true },
  },
  server: {
    left: { width: 240, pinned: true },
    right: { width: 260, pinned: true },
  },
};

const isRecord = (candidate: unknown): candidate is Record<string, unknown> =>
  typeof candidate === 'object' && candidate !== null;

function omitObsoleteSidebarFields(candidate: Record<string, unknown>): Record<string, unknown> {
  const current = { ...candidate };
  for (const key of [
    'channelPanelPinned',
    'channelPanelWidth',
    'memberPanelMode',
    'memberPanelWidth',
    'friendsPanelMode',
    'channelPanelHoverVisible',
  ]) {
    delete current[key];
  }
  return current;
}

function clampSidebarWidth(side: SidebarSide, width: number): number {
  const maxWidth = side === 'left' ? LEFT_SIDEBAR_MAX_WIDTH : RIGHT_SIDEBAR_MAX_WIDTH;
  return Math.max(SIDEBAR_MIN_WIDTH, Math.min(maxWidth, width));
}

function normalizeDock(
  candidate: unknown,
  fallback: SidebarDockPreference,
  side: SidebarSide
): SidebarDockPreference {
  const dock = isRecord(candidate) ? candidate : {};
  return {
    width:
      typeof dock.width === 'number' && Number.isFinite(dock.width)
        ? clampSidebarWidth(side, dock.width)
        : fallback.width,
    pinned: typeof dock.pinned === 'boolean' ? dock.pinned : fallback.pinned,
  };
}

function isNormalizedDock(
  candidate: unknown,
  side: SidebarSide
): candidate is SidebarDockPreference {
  if (!isRecord(candidate)) return false;
  return (
    typeof candidate.width === 'number' &&
    Number.isFinite(candidate.width) &&
    candidate.width === clampSidebarWidth(side, candidate.width) &&
    typeof candidate.pinned === 'boolean'
  );
}

function normalizeProfile(candidate: unknown, fallback: SidebarProfile): SidebarProfile {
  const profile = isRecord(candidate) ? candidate : {};
  return {
    left: normalizeDock(profile.left, fallback.left, 'left'),
    right: normalizeDock(profile.right, fallback.right, 'right'),
  };
}

export function normalizeSidebarProfiles(candidate: unknown): SidebarProfiles {
  const profiles = isRecord(candidate) ? candidate : {};
  const dm = normalizeProfile(profiles.dm, DEFAULT_SIDEBAR_PROFILES.dm);
  return {
    dm,
    server: normalizeProfile(profiles.server, dm),
  };
}

function legacyWidth(
  key: string,
  fallback: number,
  side: SidebarSide,
  legacyMin: number,
  legacyMax: number
): number {
  const stored = globalThis.localStorage?.getItem(key);
  if (stored === null || stored === undefined || stored.trim() === '') return fallback;
  const width = Number(stored);
  return Number.isFinite(width) && width >= legacyMin && width <= legacyMax
    ? clampSidebarWidth(side, width)
    : fallback;
}

function legacyNumber(
  candidate: unknown,
  fallback: number,
  side: SidebarSide,
  legacyMin: number,
  legacyMax: number
): number {
  return typeof candidate === 'number' &&
    Number.isFinite(candidate) &&
    candidate >= legacyMin &&
    candidate <= legacyMax
    ? clampSidebarWidth(side, candidate)
    : fallback;
}

function legacyRightDock(mode: unknown, width: number): SidebarDockPreference {
  if (mode === 'collapsed') return { width: LEGACY_COLLAPSED_WIDTH, pinned: true };
  if (mode === 'hidden') return { width, pinned: false };
  return { width, pinned: true };
}

function resolveLegacySidebarProfiles(
  candidate: unknown,
  importDeviceLegacyWidths: boolean
): SidebarProfiles {
  const state = isRecord(candidate) ? candidate : {};
  const retained =
    state.sidebarProfiles ??
    (Object.hasOwn(state, 'dm') || Object.hasOwn(state, 'server') ? state : undefined);
  if (retained !== undefined) return normalizeSidebarProfiles(retained);

  const leftFallback = legacyNumber(
    state.channelPanelWidth,
    DEFAULT_SIDEBAR_PROFILES.dm.left.width,
    'left',
    180,
    400
  );
  const left: SidebarDockPreference = {
    width: importDeviceLegacyWidths
      ? legacyWidth('concord:channelPanelWidth', leftFallback, 'left', 180, 400)
      : leftFallback,
    pinned: typeof state.channelPanelPinned === 'boolean' ? state.channelPanelPinned : true,
  };
  const serverRightFallback = legacyNumber(
    state.memberPanelWidth,
    DEFAULT_SIDEBAR_PROFILES.server.right.width,
    'right',
    160,
    340
  );
  const serverRightWidth = importDeviceLegacyWidths
    ? legacyWidth('concord:memberPanelWidth', serverRightFallback, 'right', 160, 340)
    : serverRightFallback;
  const dmRightWidth = importDeviceLegacyWidths
    ? legacyWidth(
        'concord:friendsPanelWidth',
        DEFAULT_SIDEBAR_PROFILES.dm.right.width,
        'right',
        160,
        340
      )
    : DEFAULT_SIDEBAR_PROFILES.dm.right.width;

  return {
    dm: {
      left: { ...left },
      right: legacyRightDock(state.friendsPanelMode, dmRightWidth),
    },
    server: {
      left: { ...left },
      right: legacyRightDock(state.memberPanelMode, serverRightWidth),
    },
  };
}

/** Derive profiles from a portable legacy document without consulting this device's old keys. */
export function deriveLegacySidebarProfiles(candidate: unknown): SidebarProfiles {
  return resolveLegacySidebarProfiles(candidate, false);
}

/** One-time local migration that imports widths from the legacy per-device storage keys. */
export function migrateLegacySidebarState(candidate: unknown): SidebarProfiles {
  return resolveLegacySidebarProfiles(candidate, true);
}

interface LayoutState {
  sidebarProfiles: SidebarProfiles;
  sidebarLayoutsDecoupled: boolean;

  // Server/folder bar heights
  serverBarHeight: number;
  folderBarHeight: number;

  // Server organization
  serverFolders: ServerFolder[];
  serverOrder: string[]; // IDs of non-foldered servers in display order

  // Interface lock (#188) — local, per-device. When true the layout is
  // read-only: pin toggle hidden, resize handles disabled, current widths +
  // pin state frozen. Persisted locally (NOT synced to the server) so a user
  // can lock one device while leaving another free.
  interfaceLocked: boolean;

  // Actions — interface lock
  setInterfaceLocked: (locked: boolean) => void;

  // Actions — retained sidebar profiles
  setSidebarWidth: (context: SidebarContext, side: SidebarSide, width: number) => void;
  setSidebarPinned: (context: SidebarContext, side: SidebarSide, pinned: boolean) => void;
  setSidebarLayoutsDecoupled: (decoupled: boolean) => void;
  applySidebarPreferences: (profiles: SidebarProfiles, decoupled: boolean) => void;

  // Actions — bar heights
  setServerBarHeight: (height: number) => void;
  setFolderBarHeight: (height: number) => void;

  // Actions — reset user-specific content (preserves UI preferences like panel widths)
  clearUserContent: () => void;

  // Actions — server organization
  reorderServers: (serverIds: string[]) => void;
  createFolder: (name: string, serverIds?: string[]) => void;
  renameFolder: (folderId: string, name: string) => void;
  deleteFolder: (folderId: string) => void;
  moveServerToFolder: (serverId: string, folderId: string, insertIndex?: number) => void;
  removeServerFromFolder: (serverId: string) => void;
  reorderFolderServers: (folderId: string, serverIds: string[]) => void;
}

export function selectSidebarDock(
  state: Pick<LayoutState, 'sidebarProfiles' | 'sidebarLayoutsDecoupled'>,
  context: SidebarContext,
  side: SidebarSide
): SidebarDockPreference {
  const retainedDmDock = state.sidebarProfiles?.dm?.[side];
  const dmDock = isNormalizedDock(retainedDmDock, side)
    ? retainedDmDock
    : normalizeDock(retainedDmDock, DEFAULT_SIDEBAR_PROFILES.dm[side], side);
  if (!state.sidebarLayoutsDecoupled || context === 'dm') return dmDock;
  const retainedServerDock = state.sidebarProfiles?.server?.[side];
  return isNormalizedDock(retainedServerDock, side)
    ? retainedServerDock
    : normalizeDock(retainedServerDock, dmDock, side);
}

function updateDock(
  state: Pick<LayoutState, 'sidebarProfiles'>,
  context: SidebarContext,
  side: SidebarSide,
  patch: Partial<SidebarDockPreference>
): Pick<LayoutState, 'sidebarProfiles'> {
  return {
    sidebarProfiles: {
      ...state.sidebarProfiles,
      [context]: {
        ...state.sidebarProfiles[context],
        [side]: {
          ...state.sidebarProfiles[context][side],
          ...patch,
        },
      },
    },
  };
}

export const useLayoutStore = wrapStore(
  create<LayoutState>()(
    devtools(
      subscribeWithSelector(
        persist(
          (set) => ({
            sidebarProfiles: DEFAULT_SIDEBAR_PROFILES,
            sidebarLayoutsDecoupled: false,
            serverBarHeight: 48,
            folderBarHeight: 32,
            serverFolders: [],
            serverOrder: [],
            interfaceLocked: false,

            setInterfaceLocked: (locked: boolean) => set({ interfaceLocked: locked }),

            setSidebarWidth: (context, side, width) =>
              set((state) => {
                if (state.interfaceLocked || !Number.isFinite(width)) return state;
                const target = state.sidebarLayoutsDecoupled ? context : 'dm';
                return updateDock(state, target, side, {
                  width: clampSidebarWidth(side, width),
                });
              }),

            setSidebarPinned: (context, side, pinned) =>
              set((state) => {
                if (state.interfaceLocked) return state;
                const target = state.sidebarLayoutsDecoupled ? context : 'dm';
                return updateDock(state, target, side, { pinned });
              }),

            setSidebarLayoutsDecoupled: (decoupled) =>
              set((state) => {
                if (state.interfaceLocked) return state;
                return {
                  sidebarProfiles: decoupled
                    ? normalizeSidebarProfiles(state.sidebarProfiles)
                    : state.sidebarProfiles,
                  sidebarLayoutsDecoupled: decoupled,
                };
              }),

            applySidebarPreferences: (profiles, decoupled) =>
              set((state) => {
                if (state.interfaceLocked) return state;
                return {
                  sidebarProfiles: normalizeSidebarProfiles(profiles),
                  sidebarLayoutsDecoupled: decoupled,
                };
              }),

            setServerBarHeight: (height: number) =>
              set({ serverBarHeight: Math.max(36, Math.min(64, height)) }),

            setFolderBarHeight: (height: number) =>
              set({ folderBarHeight: Math.max(24, Math.min(48, height)) }),

            clearUserContent: () => set({ serverFolders: [], serverOrder: [] }),

            reorderServers: (serverIds: string[]) => set({ serverOrder: serverIds }),

            createFolder: (name: string, serverIds: string[] = []) =>
              set((state) => ({
                serverFolders: [
                  ...state.serverFolders,
                  { id: crypto.randomUUID(), name, serverIds },
                ],
                // Remove foldered servers from the top-level order
                serverOrder: state.serverOrder.filter((id) => !serverIds.includes(id)),
              })),

            renameFolder: (folderId: string, name: string) =>
              set((state) => ({
                serverFolders: state.serverFolders.map((f) =>
                  f.id === folderId ? { ...f, name } : f
                ),
              })),

            deleteFolder: (folderId: string) =>
              set((state) => {
                const folder = state.serverFolders.find((f) => f.id === folderId);
                return {
                  serverFolders: state.serverFolders.filter((f) => f.id !== folderId),
                  // Return servers to the top-level order
                  serverOrder: [...state.serverOrder, ...(folder?.serverIds || [])],
                };
              }),

            moveServerToFolder: (serverId: string, folderId: string, insertIndex?: number) => {
              const updateFolder = (f: ServerFolder) => {
                const filtered = f.serverIds.filter((id) => id !== serverId);
                if (f.id !== folderId) return { ...f, serverIds: filtered };
                if (insertIndex !== undefined && insertIndex >= 0) {
                  const newIds = [...filtered];
                  newIds.splice(insertIndex, 0, serverId);
                  return { ...f, serverIds: newIds };
                }
                return { ...f, serverIds: [...filtered, serverId] };
              };
              set((state) => ({
                serverFolders: state.serverFolders.map(updateFolder),
                serverOrder: state.serverOrder.filter((id) => id !== serverId),
              }));
            },

            removeServerFromFolder: (serverId: string) => {
              const removeFromIds = (f: ServerFolder) => ({
                ...f,
                serverIds: f.serverIds.filter((id) => id !== serverId),
              });
              set((state) => ({
                serverFolders: state.serverFolders.map(removeFromIds),
                serverOrder: [...state.serverOrder, serverId],
              }));
            },

            reorderFolderServers: (folderId: string, serverIds: string[]) =>
              set((state) => ({
                serverFolders: state.serverFolders.map((f) =>
                  f.id === folderId ? { ...f, serverIds } : f
                ),
              })),
          }),
          {
            name: 'concord-layout',
            version: 2,
            migrate: (persistedState: unknown) => {
              const state = isRecord(persistedState) ? persistedState : {};
              return {
                ...omitObsoleteSidebarFields(state),
                sidebarProfiles: migrateLegacySidebarState(state),
                sidebarLayoutsDecoupled:
                  typeof state.sidebarLayoutsDecoupled === 'boolean'
                    ? state.sidebarLayoutsDecoupled
                    : false,
              } as unknown as LayoutState;
            },
            merge: (persistedState, currentState) => {
              const state = isRecord(persistedState) ? persistedState : {};
              return {
                ...currentState,
                ...omitObsoleteSidebarFields(state),
                sidebarProfiles: normalizeSidebarProfiles(state.sidebarProfiles),
                sidebarLayoutsDecoupled:
                  typeof state.sidebarLayoutsDecoupled === 'boolean'
                    ? state.sidebarLayoutsDecoupled
                    : false,
              };
            },
            partialize: (state) => ({
              sidebarProfiles: state.sidebarProfiles,
              sidebarLayoutsDecoupled: state.sidebarLayoutsDecoupled,
              serverBarHeight: state.serverBarHeight,
              folderBarHeight: state.folderBarHeight,
              serverFolders: state.serverFolders,
              serverOrder: state.serverOrder,
              interfaceLocked: state.interfaceLocked,
            }),
          }
        )
      ),
      { name: 'LayoutStore' }
    )
  )
);
