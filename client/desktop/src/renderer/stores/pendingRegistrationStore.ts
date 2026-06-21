import { create } from 'zustand';
import { persist, createJSONStorage } from 'zustand/middleware';
import { wrapStore } from '../utils/createStore';

export interface PendingRegistrationResponse {
  pending_id: string;
  email: string;
  expires_at: string;
  code_expires_at: string;
}

export interface ResendResponse {
  code_expires_at: string;
  resends_remaining: number;
}

interface PendingRegistrationState {
  pendingId: string | null;
  email: string | null;
  expiresAt: string | null;
  codeExpiresAt: string | null;
  resendsRemaining: number;
  lastResendAt: string | null;

  setPending: (data: PendingRegistrationResponse) => void;
  updateAfterResend: (resp: ResendResponse) => void;
  updateEmail: (newEmail: string, codeExpiresAt: string) => void;
  clearPending: () => void;
  isExpired: () => boolean;
}

const MAX_RESENDS = 4;

export const usePendingRegistrationStore = wrapStore(
  create<PendingRegistrationState>()(
    persist(
      (set, get) => ({
        pendingId: null,
        email: null,
        expiresAt: null,
        codeExpiresAt: null,
        resendsRemaining: MAX_RESENDS,
        lastResendAt: null,

        setPending: (data) =>
          set({
            pendingId: data.pending_id,
            email: data.email,
            expiresAt: data.expires_at,
            codeExpiresAt: data.code_expires_at,
            resendsRemaining: MAX_RESENDS,
            lastResendAt: null,
          }),

        updateAfterResend: (resp) =>
          set({
            codeExpiresAt: resp.code_expires_at,
            resendsRemaining: resp.resends_remaining,
            lastResendAt: new Date().toISOString(),
          }),

        updateEmail: (newEmail, codeExpiresAt) =>
          set({
            email: newEmail,
            codeExpiresAt,
            resendsRemaining: MAX_RESENDS,
            lastResendAt: null,
          }),

        clearPending: () =>
          set({
            pendingId: null,
            email: null,
            expiresAt: null,
            codeExpiresAt: null,
            resendsRemaining: MAX_RESENDS,
            lastResendAt: null,
          }),

        isExpired: () => {
          const { expiresAt } = get();
          if (!expiresAt) return false;
          return new Date(expiresAt).getTime() < Date.now();
        },
      }),
      {
        name: 'concord-pending-registration',
        storage: createJSONStorage(() => sessionStorage),
      }
    )
  )
);
