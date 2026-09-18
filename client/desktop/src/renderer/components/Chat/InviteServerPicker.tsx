import { useMemo } from 'react';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { useDMStore } from '@/renderer/stores/chat/dmStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useMutualServersForAll } from '@/renderer/hooks/messaging/useMutualServers';
import { INVITE, hasPermission, parsePermissions } from '@/renderer/utils/policy/permissions';
import type { ServerWithRole } from '@/renderer/types/server';
import ContextMenu from '@/renderer/components/ui/ContextMenu';
import './InviteServerPicker.css';

interface InviteServerPickerProps {
  position?: { x: number; y: number };
  /** The DM this invite is being composed in, used to resolve its recipients. */
  conversationId?: string;
  onPick: (serverId: string) => void;
  onClose: () => void;
}

/**
 * `servers[].permissions` is the authority here, and `permissionStore` is only a
 * fallback — the reverse of what this component did before #2372.
 *
 * `permissionStore.serverPermissions` has exactly one populator: MainView's
 * effect on `activeServerId`. This picker renders in the DM composer, and
 * `/app/dms` routes to `DirectMessagesView`, so MainView is not mounted at all
 * while it is on screen. `hasServerPermission` fails closed for every server it
 * has no entry for, which left the list holding only whatever server happened to
 * be active last — the reported defect.
 *
 * Fanning out `GET /servers/{id}/permissions` per server was the other candidate
 * and is not viable: it is capped at 30/min/user and MainView spends from the
 * same budget, so any account with more than ~30 servers would start failing
 * closed again, this time intermittently.
 *
 * The fallback stays for a control plane predating the field: there every row
 * arrives without it, and dropping to `permissionStore` reproduces the previous
 * behaviour rather than emptying the list a different way.
 *
 * Residual, accepted and narrow: a server joined in THIS session has neither
 * source — `JoinServerResponse` carries no permissions and `addServer` writes no
 * `permissionStore` entry — so it stays hidden until the next `fetchServers`.
 * `rbac.BasePermissions` excludes `PermInvite`, so a freshly joined member
 * ordinarily cannot invite anyway.
 */
function canInviteTo(
  server: ServerWithRole,
  hasServerPermission: (serverId: string, perm: bigint) => boolean
): boolean {
  // Truthiness, NOT `!== undefined`. An empty string is a third state the
  // tri-state contract never named: `'' !== undefined` takes the field branch,
  // and `BigInt('')` is `0n` rather than a throw, so the row would hide with no
  // fallback. `''` means "a producer did not compute this", which is the same
  // thing absence means — so it takes the same path. A genuine zero arrives as
  // `'0'`, which is truthy here and correctly denies.
  if (server.permissions) {
    return hasPermission(parsePermissions(server.permissions), INVITE);
  }
  return hasServerPermission(server.id, INVITE);
}

export function InviteServerPicker({
  position = { x: 0, y: 0 },
  conversationId,
  onPick,
  onClose,
}: Readonly<InviteServerPickerProps>) {
  const servers = useServerStore((s) => s.servers);
  const hasServerPermission = usePermissionStore((s) => s.hasServerPermission);
  const conversation = useDMStore((s) => s.conversations.find((c) => c.id === conversationId));
  const myUserId = useUserStore((s) => s.user?.id);

  // Everyone in the conversation except the sender. A group DM holds up to ten
  // participants, so this is bounded at nine by the product, not by a guess.
  const recipientIds = useMemo(
    () =>
      (conversation?.participants ?? [])
        .map((participant) => participant.userId)
        .filter((id) => id !== myUserId),
    [conversation, myUserId]
  );
  const alreadyIn = useMutualServersForAll(recipientIds);

  const invitable = servers.filter((sv) => canInviteTo(sv, hasServerPermission));

  return (
    <ContextMenu position={position} onClose={onClose}>
      <div className="invite-server-picker">
        {invitable.length === 0 ? (
          <div className="invite-server-picker__empty">No servers you can invite to.</div>
        ) : (
          invitable.map((sv) => {
            // Greyed, never removed. The server is still one the user can invite
            // to; what has changed is that this particular recipient is already
            // there. Hiding it would make the list silently differ per
            // conversation with nothing to explain why, which is the confusion
            // the whole picker defect was about.
            const joined = alreadyIn.has(sv.id);
            return (
              <ContextMenu.Item
                key={sv.id}
                label={joined ? `${sv.name} — already a member` : sv.name}
                ariaDisabled={joined}
                onClick={() => onPick(sv.id)}
              />
            );
          })
        )}
        <ContextMenu.Separator />
        <ContextMenu.Item label="Close" onClick={onClose} />
      </div>
    </ContextMenu>
  );
}
