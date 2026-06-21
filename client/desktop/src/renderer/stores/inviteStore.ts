import { create } from 'zustand';
import { devtools } from 'zustand/middleware';
import { wrapStore } from '../utils/createStore';
import {
  ServerInviteWithCreator,
  CreateInviteRequest,
  JoinServerResponse,
  InviteInfoResponse,
} from '../types/server';
import { apiFetch } from '../services/apiClient';

export type { InviteInfoResponse };

interface InviteState {
  invites: Record<string, ServerInviteWithCreator[]>; // keyed by serverId
  isLoading: boolean;
  error: string | null;

  fetchInvites: (serverId: string) => Promise<void>;
  createInvite: (
    serverId: string,
    opts?: CreateInviteRequest
  ) => Promise<ServerInviteWithCreator | null>;
  revokeInvite: (serverId: string, inviteId: string) => Promise<boolean>;
  joinServer: (code: string) => Promise<JoinServerResponse | null>;
  getInviteInfo: (code: string) => Promise<InviteInfoResponse | null>;
  clearInvites: () => void;
}

export const useInviteStore = wrapStore(
  create<InviteState>()(
    devtools(
      (set, _get) => ({
        invites: {},
        isLoading: false,
        error: null,

        fetchInvites: async (serverId: string) => {
          set({ isLoading: true, error: null });
          try {
            const res = await apiFetch(`/api/v1/servers/${serverId}/invites`);
            if (!res.ok) {
              const data = await res.json();
              throw new Error(data.error || 'Failed to fetch invites');
            }
            const data = await res.json();
            set((state) => ({
              invites: { ...state.invites, [serverId]: data.invites || [] },
              isLoading: false,
            }));
          } catch (error) {
            set({
              error: error instanceof Error ? error.message : 'Failed to fetch invites',
              isLoading: false,
            });
          }
        },

        createInvite: async (serverId: string, opts?: CreateInviteRequest) => {
          set({ error: null });
          try {
            const res = await apiFetch(`/api/v1/servers/${serverId}/invites`, {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify(opts || {}),
            });
            if (!res.ok) {
              const data = await res.json();
              throw new Error(data.error || 'Failed to create invite');
            }
            const data = await res.json();
            const invite = data.invite as ServerInviteWithCreator;

            // Append to local cache
            set((state) => {
              const existing = state.invites[serverId] || [];
              return { invites: { ...state.invites, [serverId]: [invite, ...existing] } };
            });

            return invite;
          } catch (error) {
            set({ error: error instanceof Error ? error.message : 'Failed to create invite' });
            return null;
          }
        },

        revokeInvite: async (serverId: string, inviteId: string) => {
          set({ error: null });
          try {
            const res = await apiFetch(`/api/v1/servers/${serverId}/invites/${inviteId}`, {
              method: 'DELETE',
            });
            if (!res.ok) {
              const data = await res.json();
              throw new Error(data.error || 'Failed to revoke invite');
            }

            // Update local cache
            set((state) => {
              const existing = state.invites[serverId] || [];
              return {
                invites: {
                  ...state.invites,
                  [serverId]: existing.map((inv) =>
                    inv.id === inviteId ? { ...inv, is_revoked: true } : inv
                  ),
                },
              };
            });

            return true;
          } catch (error) {
            set({ error: error instanceof Error ? error.message : 'Failed to revoke invite' });
            return false;
          }
        },

        joinServer: async (code: string) => {
          set({ isLoading: true, error: null });
          try {
            const res = await apiFetch('/api/v1/invites/join', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ code }),
            });
            const data = await res.json();
            if (!res.ok) {
              throw new Error(data.error || 'Failed to join server');
            }
            set({ isLoading: false });
            return data as JoinServerResponse;
          } catch (error) {
            set({
              error: error instanceof Error ? error.message : 'Failed to join server',
              isLoading: false,
            });
            return null;
          }
        },

        getInviteInfo: async (code: string) => {
          try {
            const res = await apiFetch(`/api/v1/invites/${code}`);
            if (!res.ok) {
              const data = await res.json();
              throw new Error(data.error || 'Invalid invite code');
            }
            return (await res.json()) as InviteInfoResponse;
          } catch (error) {
            set({ error: error instanceof Error ? error.message : 'Invalid invite code' });
            return null;
          }
        },

        clearInvites: () => {
          set({ invites: {}, isLoading: false, error: null });
        },
      }),
      { name: 'InviteStore' }
    )
  )
);
