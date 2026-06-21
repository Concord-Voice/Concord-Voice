import { useEffect } from 'react';
import { useServerStore } from '@/renderer/stores/serverStore';
import { usePermissionStore } from '@/renderer/stores/permissionStore';
import { INVITE } from '@/renderer/utils/permissions';
import './InviteServerPicker.css';

interface InviteServerPickerProps {
  onPick: (serverId: string) => void;
  onClose: () => void;
}

export function InviteServerPicker({ onPick, onClose }: Readonly<InviteServerPickerProps>) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose();
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [onClose]);

  const servers = useServerStore((s) => s.servers);
  const hasServerPermission = usePermissionStore((s) => s.hasServerPermission);
  const invitable = servers.filter((sv) => hasServerPermission(sv.id, INVITE));

  return (
    <div className="invite-server-picker">
      {invitable.length === 0 ? (
        <div className="invite-server-picker__empty">No servers you can invite to.</div>
      ) : (
        <ul className="invite-server-picker__list">
          {invitable.map((sv) => (
            <li key={sv.id}>
              <button
                type="button"
                className="invite-server-picker__item"
                onClick={() => onPick(sv.id)}
              >
                {sv.name}
              </button>
            </li>
          ))}
        </ul>
      )}
      <button
        type="button"
        className="invite-server-picker__close"
        onClick={onClose}
        aria-label="Close"
      >
        Close
      </button>
    </div>
  );
}
