import React from 'react';
import { ShieldCheck } from 'lucide-react';
import { usePermissionStore } from '../../stores/chat/permissionStore';
import { openVerificationSetup } from '../../utils/ui/openVerificationSetup';
import ContextMenu from '../ui/ContextMenu';

interface MfaRestrictedMenuItemProps {
  serverId: string;
  onClose: () => void;
}

/**
 * The context menus' "Set up verification" item (#3456 §3.6): shown to a member
 * whose dangerous permissions the server is withholding until they enrol, in
 * the menus where those controls would otherwise just be missing.
 *
 * Render it in the upper group, before the destructive separator. It is not
 * dismissible, unlike the sidebar notice: a menu is something the user opened
 * looking for a control, so the answer stays there every time. It renders
 * nothing unless the flag is set for `serverId`, which is known only for the
 * active server (R15), so a menu opened on another server's icon shows none
 * until that server is opened.
 */
const MfaRestrictedMenuItem: React.FC<MfaRestrictedMenuItemProps> = ({ serverId, onClose }) => {
  const restricted = usePermissionStore((s) => s.mfaRestrictedByServer[serverId] === true);
  if (!restricted) return null;

  return (
    <ContextMenu.Item
      icon={<ShieldCheck size={16} />}
      label="Set up verification"
      onClick={() => {
        void openVerificationSetup({ returnTo: { kind: 'chat' } });
        onClose();
      }}
    />
  );
};

export default MfaRestrictedMenuItem;
