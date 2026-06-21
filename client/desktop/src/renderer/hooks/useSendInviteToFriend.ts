import { useCallback } from 'react';
import { useInviteStore } from '../stores/inviteStore';
import { useDMStore } from '../stores/dmStore';
import { buildInviteUrl } from '../utils/inviteUrl';
import { sendDMMessage } from '../services/dmMessageSender';
import { e2eeService } from '../services/e2eeService';
import type { Friend } from '../stores/friendStore';

export type SendInviteResult =
  | { ok: true; conversationId: string }
  | { ok: false; reason: 'not_ready' | 'dm_blocked' | 'mint_failed' };

/**
 * Orchestrates "Send to a Friend": mint a server invite, open the friend's DM, and drop the
 * canonical invite URL into it. Pure of routing (the modal navigates) and does NOT mount
 * `useMessaging` — it calls the extracted `sendDMMessage` directly, so no queue-processing
 * lifecycle side effects. See spec §3.0/§3.1.
 */
export function useSendInviteToFriend(serverId: string) {
  const createInvite = useInviteStore((s) => s.createInvite);
  const openDM = useDMStore((s) => s.openDM);

  const send = useCallback(
    async (friend: Friend): Promise<SendInviteResult> => {
      // Fail-closed before doing anything irreversible (mint) — #918 startup-race guard.
      if (!e2eeService.isInitialized) {
        return { ok: false, reason: 'not_ready' };
      }

      const invite = await createInvite(serverId);
      if (!invite) {
        // createInvite normalizes all errors (incl. a permission 403) to null.
        return { ok: false, reason: 'mint_failed' };
      }

      const url = buildInviteUrl(invite.code);

      let conversationId: string;
      try {
        const conv = await openDM(friend.userId);
        conversationId = conv.id;
      } catch {
        // openDM throws on dm_disabled / privacy_blocked (and other failures); the
        // dominant cause right after a successful mint is the privacy gate.
        return { ok: false, reason: 'dm_blocked' };
      }

      sendDMMessage(conversationId, url);
      return { ok: true, conversationId };
    },
    [serverId, createInvite, openDM]
  );

  return { send };
}
