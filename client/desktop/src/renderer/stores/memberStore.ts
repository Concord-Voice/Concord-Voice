import { create } from 'zustand';
import { devtools } from 'zustand/middleware';
import { wrapStore } from '../utils/createStore';
import { apiFetch } from '../services/apiClient';
import type { MemberRoleInfo } from '../types/server';

export type PresenceStatus = 'online' | 'offline' | 'dnd' | 'invisible';

export interface ServerMember {
  user_id: string;
  username: string;
  display_name?: string;
  bio?: string;
  avatar_url?: string;
  header_image_url?: string;
  color_scheme?: string;
  role: 'owner' | 'admin' | 'member';
  joined_at: string;
  last_seen?: number;
  roles: MemberRoleInfo[];
  server_muted?: boolean;
  server_deafened?: boolean;
  timed_out_until?: string | null;
}

interface MemberState {
  members: ServerMember[];
  onlineUserIds: Set<string>;
  userStatuses: Map<string, PresenceStatus>;
  lastSeenByUser: Map<string, number>;
  selfStatus: PresenceStatus;
  isLoading: boolean;
  error: string | null;

  fetchMembers: (serverId: string) => Promise<void>;
  addMember: (member: ServerMember) => void;
  removeMember: (userId: string) => void;
  updateMemberProfile: (
    userId: string,
    updates: Partial<
      Pick<
        ServerMember,
        'username' | 'display_name' | 'avatar_url' | 'header_image_url' | 'color_scheme'
      >
    >
  ) => void;
  setMemberTimeout: (userId: string, timedOutUntil: string | null) => void;
  setOnlineUsers: (userIds: string[]) => void;
  setUserOnline: (userId: string) => void;
  setUserOffline: (userId: string) => void;
  setUserStatus: (userId: string, status: PresenceStatus) => void;
  setUserLastSeen: (userId: string, timestamp: number) => void;
  setPresenceSnapshot: (users: Array<{ user_id: string; status: string }>) => void;
  setSelfStatus: (status: PresenceStatus) => void;
  clearMembers: () => void;
}

export const useMemberStore = wrapStore(create<MemberState>()(
  devtools(
    (set, get) => ({
      members: [],
      onlineUserIds: new Set<string>(),
      userStatuses: new Map<string, PresenceStatus>(),
      lastSeenByUser: new Map<string, number>(),
      selfStatus: 'online' as PresenceStatus,
      isLoading: false,
      error: null,

      fetchMembers: async (serverId: string) => {
        set({ isLoading: true, error: null });

        try {
          const response = await apiFetch(`/api/v1/servers/${serverId}/members`);

          if (!response.ok) {
            const data = await response.json();
            throw new Error(data.error || 'Failed to load members');
          }

          const data = await response.json();
          const members = data.members || [];

          // Populate lastSeenByUser from API response
          const lastSeenMap = new Map<string, number>();
          for (const m of members as ServerMember[]) {
            if (m.last_seen) {
              lastSeenMap.set(m.user_id, m.last_seen);
            }
          }

          set({ members, isLoading: false, lastSeenByUser: lastSeenMap });
        } catch (error) {
          set({
            error: error instanceof Error ? error.message : 'Failed to load members',
            isLoading: false,
          });
        }
      },

      addMember: (member: ServerMember) => {
        set((state) => {
          // Avoid duplicates
          if (state.members.some((m) => m.user_id === member.user_id)) {
            return state;
          }
          return { members: [...state.members, member] };
        });
      },

      removeMember: (userId: string) => {
        set((state) => ({
          members: state.members.filter((m) => m.user_id !== userId),
        }));
      },

      updateMemberProfile: (userId: string, updates) => {
        set((state) => {
          const idx = state.members.findIndex((m) => m.user_id === userId);
          if (idx < 0) return state;
          const updated = [...state.members];
          updated[idx] = { ...updated[idx], ...updates };
          return { members: updated };
        });
      },

      setMemberTimeout: (userId: string, timedOutUntil: string | null) => {
        set((state) => {
          const idx = state.members.findIndex((m) => m.user_id === userId);
          if (idx < 0) return state;
          const updated = [...state.members];
          updated[idx] = { ...updated[idx], timed_out_until: timedOutUntil };
          return { members: updated };
        });
      },

      setOnlineUsers: (userIds: string[]) => {
        const onlineSet = new Set(userIds);
        const existing = get().userStatuses;
        const statusMap = new Map<string, PresenceStatus>();
        for (const id of userIds) {
          // Preserve existing non-offline status (e.g. dnd, invisible)
          const current = existing.get(id);
          statusMap.set(id, current && current !== 'offline' ? current : 'online');
        }
        set({ onlineUserIds: onlineSet, userStatuses: statusMap });
      },

      setUserOnline: (userId: string) => {
        set((state) => {
          const nextOnline = new Set(state.onlineUserIds);
          nextOnline.add(userId);
          const nextStatuses = new Map(state.userStatuses);
          nextStatuses.set(userId, 'online');
          return { onlineUserIds: nextOnline, userStatuses: nextStatuses };
        });
      },

      setUserOffline: (userId: string) => {
        set((state) => {
          const nextOnline = new Set(state.onlineUserIds);
          nextOnline.delete(userId);
          const nextStatuses = new Map(state.userStatuses);
          nextStatuses.set(userId, 'offline');
          return { onlineUserIds: nextOnline, userStatuses: nextStatuses };
        });
      },

      setUserStatus: (userId: string, status: PresenceStatus) => {
        set((state) => {
          const nextStatuses = new Map(state.userStatuses);
          nextStatuses.set(userId, status);
          const nextOnline = new Set(state.onlineUserIds);
          if (status === 'online' || status === 'dnd') {
            nextOnline.add(userId);
          } else {
            nextOnline.delete(userId);
          }
          return { userStatuses: nextStatuses, onlineUserIds: nextOnline };
        });
      },

      setUserLastSeen: (userId: string, timestamp: number) => {
        set((state) => {
          const nextLastSeen = new Map(state.lastSeenByUser);
          nextLastSeen.set(userId, timestamp);
          return { lastSeenByUser: nextLastSeen };
        });
      },

      setPresenceSnapshot: (users: Array<{ user_id: string; status: string }>) => {
        const onlineSet = new Set<string>();
        const statusMap = new Map<string, PresenceStatus>();
        for (const u of users) {
          const status = u.status as PresenceStatus;
          statusMap.set(u.user_id, status);
          if (status === 'online' || status === 'dnd') {
            onlineSet.add(u.user_id);
          }
        }
        set({ onlineUserIds: onlineSet, userStatuses: statusMap });
      },

      setSelfStatus: (status: PresenceStatus) => {
        set({ selfStatus: status });
      },

      clearMembers: () => {
        // Preserve presence state — it's global, not tied to a specific server's member list
        set({
          members: [],
          isLoading: false,
          error: null,
        });
      },
    }),
    { name: 'MemberStore' }
  )
));
