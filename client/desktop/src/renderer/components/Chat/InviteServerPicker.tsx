import { useServerStore } from '@/renderer/stores/serverStore';
import { usePermissionStore } from '@/renderer/stores/permissionStore';
import { INVITE } from '@/renderer/utils/permissions';
import ContextMenu from '@/renderer/components/ui/ContextMenu';
import './InviteServerPicker.css';

interface InviteServerPickerProps {
  position?: { x: number; y: number };
  onPick: (serverId: string) => void;
  onClose: () => void;
}

export function InviteServerPicker({
  position = { x: 0, y: 0 },
  onPick,
  onClose,
}: Readonly<InviteServerPickerProps>) {
  const servers = useServerStore((s) => s.servers);
  const hasServerPermission = usePermissionStore((s) => s.hasServerPermission);
  const invitable = servers.filter((sv) => hasServerPermission(sv.id, INVITE));

  return (
    <ContextMenu position={position} onClose={onClose}>
      <div className="invite-server-picker">
        {invitable.length === 0 ? (
          <div className="invite-server-picker__empty">No servers you can invite to.</div>
        ) : (
          invitable.map((sv) => (
            <ContextMenu.Item key={sv.id} label={sv.name} onClick={() => onPick(sv.id)} />
          ))
        )}
        <ContextMenu.Separator />
        <ContextMenu.Item label="Close" onClick={onClose} />
      </div>
    </ContextMenu>
  );
}
