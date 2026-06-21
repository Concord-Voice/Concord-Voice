import React, { useState, useRef, useEffect } from 'react';
import { Plus, UserPlus, Settings } from 'lucide-react';
import { useInviteStore } from '../../stores/inviteStore';
import { usePermissionStore } from '../../stores/permissionStore';
import { Permissions, hasPermission } from '../../utils/permissions';
import type { ServerWithRole, ServerInviteWithCreator } from '../../types/server';
import { SendToFriendModal } from './SendToFriendModal';
import './ServerActionBar.css';

const EMPTY_INVITES: ServerInviteWithCreator[] = [];

interface ServerActionBarProps {
  server: ServerWithRole;
  onOpenCreateModal: () => void;
  onOpenCreateCategoryModal: () => void;
  onOpenSettings?: () => void;
}

const ServerActionBar: React.FC<ServerActionBarProps> = ({
  server,
  onOpenCreateModal,
  onOpenCreateCategoryModal,
  onOpenSettings,
}) => {
  const [showPopup, setShowPopup] = useState(false);
  const [showAddMenu, setShowAddMenu] = useState(false);
  const [showSendToFriend, setShowSendToFriend] = useState(false);
  const [copied, setCopied] = useState(false);
  const [isGenerating, setIsGenerating] = useState(false);
  const popupRef = useRef<HTMLDivElement>(null);
  const btnRef = useRef<HTMLButtonElement>(null);
  const addMenuRef = useRef<HTMLDivElement>(null);
  const addBtnRef = useRef<HTMLButtonElement>(null);

  const invites = useInviteStore((state) => state.invites[server.id] ?? EMPTY_INVITES);
  const fetchInvites = useInviteStore((state) => state.fetchInvites);
  const createInvite = useInviteStore((state) => state.createInvite);

  const serverPerms = usePermissionStore((s) => s.serverPermissions[server.id] ?? 0n);
  const canCreateChannel = hasPermission(serverPerms, Permissions.MANAGE_CHANNELS);
  const canInvite = hasPermission(serverPerms, Permissions.INVITE);
  const canManageServer = hasPermission(serverPerms, Permissions.MANAGE_SERVER);
  const buttonCount = (canCreateChannel ? 1 : 0) + (canInvite ? 1 : 0) + (canManageServer ? 1 : 0);

  const activeInvite = invites.find((inv) => {
    if (inv.is_revoked) return false;
    if (inv.expires_at && new Date(inv.expires_at) < new Date()) return false;
    if (inv.max_uses !== null && inv.use_count >= inv.max_uses) return false;
    return true;
  });

  // Close popup on click outside
  useEffect(() => {
    if (!showPopup) return;
    const handleClickOutside = (e: MouseEvent) => {
      const target = e.target as Node;
      if (
        popupRef.current &&
        !popupRef.current.contains(target) &&
        btnRef.current &&
        !btnRef.current.contains(target)
      ) {
        setShowPopup(false);
        setCopied(false);
      }
    };
    document.addEventListener('mousedown', handleClickOutside);
    return () => document.removeEventListener('mousedown', handleClickOutside);
  }, [showPopup]);

  // Close add menu on click outside
  useEffect(() => {
    if (!showAddMenu) return;
    const handleClickOutside = (e: MouseEvent) => {
      const target = e.target as Node;
      if (
        addMenuRef.current &&
        !addMenuRef.current.contains(target) &&
        addBtnRef.current &&
        !addBtnRef.current.contains(target)
      ) {
        setShowAddMenu(false);
      }
    };
    document.addEventListener('mousedown', handleClickOutside);
    return () => document.removeEventListener('mousedown', handleClickOutside);
  }, [showAddMenu]);

  const handleTogglePopup = async () => {
    if (showPopup) {
      setShowPopup(false);
      setCopied(false);
      return;
    }
    setShowPopup(true);
    setCopied(false);
    if (invites.length === 0) {
      await fetchInvites(server.id);
    }
  };

  const writeClipboard = async (text: string) => {
    if (globalThis.electron?.writeClipboard) {
      await globalThis.electron.writeClipboard(text);
    } else {
      await navigator.clipboard.writeText(text);
    }
  };

  const handleCopyCode = async () => {
    if (activeInvite) {
      await writeClipboard(activeInvite.code);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    }
  };

  const handleGenerate = async () => {
    setIsGenerating(true);
    const invite = await createInvite(server.id);
    setIsGenerating(false);
    if (invite) {
      await writeClipboard(invite.code);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    }
  };

  // No actions available — just a thin spacer
  if (buttonCount === 0) {
    return <div className="channel-actions-spacer" />;
  }

  return (
    <div className={`channel-actions-strip ${buttonCount === 1 ? 'single' : ''}`}>
      {canCreateChannel && (
        <button
          ref={addBtnRef}
          className={`channel-action-item ${showAddMenu ? 'active' : ''}`}
          onClick={() => setShowAddMenu(!showAddMenu)}
          title="Create a channel or category"
        >
          <Plus size={16} />
          <span>Add</span>
        </button>
      )}

      {showAddMenu && (
        <div className="add-menu-popup" ref={addMenuRef}>
          <button
            className="add-menu-item"
            onClick={() => {
              setShowAddMenu(false);
              onOpenCreateModal();
            }}
          >
            <Plus size={14} />
            <span>Channel</span>
          </button>
          <button
            className="add-menu-item"
            onClick={() => {
              setShowAddMenu(false);
              onOpenCreateCategoryModal();
            }}
          >
            <Plus size={14} />
            <span>Category</span>
          </button>
        </div>
      )}

      {canCreateChannel && canInvite && <div className="channel-action-divider" />}

      {canInvite && (
        <button
          ref={btnRef}
          className={`channel-action-item ${showPopup ? 'active' : ''}`}
          onClick={handleTogglePopup}
          title="Invite people to this server"
        >
          <UserPlus size={16} />
          <span>Invite</span>
        </button>
      )}

      {showPopup && (
        <div className="invite-popup" ref={popupRef}>
          <div className="invite-popup-section">
            <div className="invite-popup-header">Invite Code</div>
            {activeInvite ? (
              <>
                <code className="invite-popup-code">{activeInvite.code}</code>
                <button className="invite-popup-action-btn" onClick={handleCopyCode}>
                  {copied ? 'Copied!' : 'Copy Code'}
                </button>
              </>
            ) : (
              <button
                className="invite-popup-action-btn"
                onClick={handleGenerate}
                disabled={isGenerating}
              >
                {isGenerating ? 'Generating...' : 'Generate Code'}
              </button>
            )}
          </div>

          <div className="invite-popup-divider" />

          <div className="invite-popup-section">
            <div className="invite-popup-header">Direct Invite</div>
            <button
              className="invite-popup-action-btn secondary"
              onClick={() => {
                setShowPopup(false);
                setShowSendToFriend(true);
              }}
            >
              <UserPlus size={14} />
              Send to a Friend
            </button>
          </div>
        </div>
      )}

      {(canCreateChannel || canInvite) && canManageServer && (
        <div className="channel-action-divider" />
      )}

      {canManageServer && (
        <button className="channel-action-item" onClick={onOpenSettings} title="Server settings">
          <Settings size={16} />
          <span>Settings</span>
        </button>
      )}

      <SendToFriendModal
        serverId={server.id}
        serverName={server.name}
        open={showSendToFriend}
        onClose={() => setShowSendToFriend(false)}
      />
    </div>
  );
};

export default ServerActionBar;
