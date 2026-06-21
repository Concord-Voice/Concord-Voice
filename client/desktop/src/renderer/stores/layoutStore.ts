import { create } from 'zustand';
import { persist, devtools, subscribeWithSelector } from 'zustand/middleware';
import { wrapStore } from '../utils/createStore';

export interface ServerFolder {
  id: string;
  name: string;
  serverIds: string[];
}

export type MemberPanelMode = 'expanded' | 'collapsed' | 'hidden';

interface LayoutState {
  // Channel panel
  channelPanelPinned: boolean;
  channelPanelWidth: number;

  // Member panel
  memberPanelMode: MemberPanelMode;
  memberPanelWidth: number;

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

  // Transient (not persisted)
  channelPanelHoverVisible: boolean;

  // Actions — channel panel
  toggleChannelPin: () => void;
  setChannelPanelWidth: (width: number) => void;
  showChannelPanelHover: () => void;
  hideChannelPanelHover: () => void;

  // Actions — interface lock
  setInterfaceLocked: (locked: boolean) => void;

  // Actions — member panel
  setMemberPanelMode: (mode: MemberPanelMode) => void;
  cycleMemberPanelMode: () => void;
  setMemberPanelWidth: (width: number) => void;

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

export const useLayoutStore = wrapStore(
  create<LayoutState>()(
    devtools(
      subscribeWithSelector(
        persist(
          (set) => ({
            channelPanelPinned: true,
            channelPanelWidth: 240,
            memberPanelMode: 'expanded' as MemberPanelMode,
            memberPanelWidth: 260,
            serverBarHeight: 48,
            folderBarHeight: 32,
            serverFolders: [],
            serverOrder: [],
            interfaceLocked: false,
            channelPanelHoverVisible: false,

            setInterfaceLocked: (locked: boolean) => set({ interfaceLocked: locked }),

            toggleChannelPin: () =>
              set((state) => ({
                channelPanelPinned: !state.channelPanelPinned,
                channelPanelHoverVisible: false,
              })),

            setChannelPanelWidth: (width: number) =>
              set({ channelPanelWidth: Math.max(180, Math.min(400, width)) }),

            showChannelPanelHover: () => set({ channelPanelHoverVisible: true }),
            hideChannelPanelHover: () => set({ channelPanelHoverVisible: false }),

            setMemberPanelMode: (mode: MemberPanelMode) => set({ memberPanelMode: mode }),

            cycleMemberPanelMode: () =>
              set((state) => {
                const cycle: MemberPanelMode[] = ['expanded', 'collapsed', 'hidden'];
                const idx = cycle.indexOf(state.memberPanelMode);
                return { memberPanelMode: cycle[(idx + 1) % cycle.length] };
              }),

            setMemberPanelWidth: (width: number) =>
              set({ memberPanelWidth: Math.max(160, Math.min(340, width)) }),

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
            partialize: (state) => ({
              channelPanelPinned: state.channelPanelPinned,
              channelPanelWidth: state.channelPanelWidth,
              memberPanelMode: state.memberPanelMode,
              memberPanelWidth: state.memberPanelWidth,
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
