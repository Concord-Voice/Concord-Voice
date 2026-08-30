import React, { useState, useRef, useEffect, useId } from 'react';
import { Plus, UserPlus, Settings } from 'lucide-react';
import { useInviteStore } from '../../stores/chat/inviteStore';
import { usePermissionStore } from '../../stores/chat/permissionStore';
import { Permissions, hasPermission } from '../../utils/permissions';
import type { ServerWithRole, ServerInviteWithCreator } from '../../types/server';
import { SendToFriendModal } from './SendToFriendModal';
import { AttributedPopover } from '../Layout/AttributedPopover';
import './ServerActionBar.css';

const EMPTY_INVITES: ServerInviteWithCreator[] = [];

interface ServerActionBarProps {
  compact?: boolean;
  server: ServerWithRole;
  onOpenCreateModal: () => void;
  onOpenCreateCategoryModal: () => void;
  onOpenSettings?: () => void;
}

interface AddActionProps {
  compact: boolean;
  popoverId: string;
  open: boolean;
  buttonRef: React.RefObject<HTMLButtonElement | null>;
  menuRef: React.RefObject<HTMLDivElement | null>;
  content: React.ReactNode;
  onToggle: () => void;
  onClose: () => void;
}

interface InviteActionProps {
  compact: boolean;
  popoverId: string;
  open: boolean;
  buttonRef: React.RefObject<HTMLButtonElement | null>;
  popupRef: React.RefObject<HTMLDivElement | null>;
  content: React.ReactNode;
  onToggle: () => void;
  onClose: () => void;
}

interface AddMenuContentProps {
  compact: boolean;
  onOpenChannel: () => void;
  onOpenCategory: () => void;
}

interface InvitePopupContentProps {
  compact: boolean;
  activeInvite?: ServerInviteWithCreator;
  copied: boolean;
  isGenerating: boolean;
  onCopyCode: () => void;
  onGenerate: () => void;
  onSendToFriend: () => void;
}

interface SettingsActionProps {
  compact: boolean;
  onOpenSettings?: () => void;
}

const AddMenuContent: React.FC<AddMenuContentProps> = ({
  compact,
  onOpenChannel,
  onOpenCategory,
}) => (
  <div className="add-menu-content">
    <button className="add-menu-item" autoFocus={compact} onClick={onOpenChannel}>
      <Plus size={14} />
      <span>Channel</span>
    </button>
    <button className="add-menu-item" onClick={onOpenCategory}>
      <Plus size={14} />
      <span>Category</span>
    </button>
  </div>
);

const InvitePopupContent: React.FC<InvitePopupContentProps> = ({
  compact,
  activeInvite,
  copied,
  isGenerating,
  onCopyCode,
  onGenerate,
  onSendToFriend,
}) => (
  <>
    <div className="invite-popup-section">
      <div className="invite-popup-header">Invite Code</div>
      {activeInvite ? (
        <>
          <code className="invite-popup-code">{activeInvite.code}</code>
          <button className="invite-popup-action-btn" autoFocus={compact} onClick={onCopyCode}>
            {copied ? 'Copied!' : 'Copy Code'}
          </button>
        </>
      ) : (
        <button
          className="invite-popup-action-btn"
          autoFocus={compact}
          onClick={onGenerate}
          disabled={isGenerating}
        >
          {isGenerating ? 'Generating...' : 'Generate Code'}
        </button>
      )}
    </div>

    <div className="invite-popup-divider" />

    <div className="invite-popup-section">
      <div className="invite-popup-header">Direct Invite</div>
      <button className="invite-popup-action-btn secondary" onClick={onSendToFriend}>
        <UserPlus size={14} />
        Send to a Friend
      </button>
    </div>
  </>
);

const AddAction: React.FC<AddActionProps> = ({
  compact,
  popoverId,
  open,
  buttonRef,
  menuRef,
  content,
  onToggle,
  onClose,
}) => (
  <>
    <button
      id={`${popoverId}-server-add`}
      ref={buttonRef}
      className={`channel-action-item ${open ? 'active' : ''}`}
      onClick={onToggle}
      title={compact ? 'Add' : 'Create a channel or category'}
      aria-label={compact ? 'Add' : undefined}
      aria-expanded={compact ? open : undefined}
      aria-controls={compact ? `${popoverId}-server-add-popover` : undefined}
    >
      <Plus size={16} />
      {!compact && <span>Add</span>}
    </button>
    {open &&
      (compact ? (
        <AttributedPopover
          id={`${popoverId}-server-add-popover`}
          anchor={buttonRef.current}
          label="Add channel or category"
          open
          placement="right"
          onClose={onClose}
        >
          {content}
        </AttributedPopover>
      ) : (
        <div className="add-menu-popup" ref={menuRef}>
          {content}
        </div>
      ))}
  </>
);

const InviteAction: React.FC<InviteActionProps> = ({
  compact,
  popoverId,
  open,
  buttonRef,
  popupRef,
  content,
  onToggle,
  onClose,
}) => (
  <>
    <button
      id={`${popoverId}-server-invite`}
      ref={buttonRef}
      className={`channel-action-item ${open ? 'active' : ''}`}
      onClick={onToggle}
      title={compact ? 'Invite' : 'Invite people to this server'}
      aria-label={compact ? 'Invite' : undefined}
      aria-expanded={compact ? open : undefined}
      aria-controls={compact ? `${popoverId}-server-invite-popover` : undefined}
    >
      <UserPlus size={16} />
      {!compact && <span>Invite</span>}
    </button>
    {open &&
      (compact ? (
        <AttributedPopover
          id={`${popoverId}-server-invite-popover`}
          anchor={buttonRef.current}
          label="Invite people to this server"
          open
          placement="right"
          onClose={onClose}
        >
          {content}
        </AttributedPopover>
      ) : (
        <div className="invite-popup" ref={popupRef}>
          {content}
        </div>
      ))}
  </>
);

const SettingsAction: React.FC<SettingsActionProps> = ({ compact, onOpenSettings }) => (
  <button
    className="channel-action-item"
    onClick={onOpenSettings}
    title={compact ? 'Settings' : 'Server settings'}
    aria-label={compact ? 'Settings' : undefined}
  >
    <Settings size={16} />
    {!compact && <span>Settings</span>}
  </button>
);

const ServerActionBar: React.FC<ServerActionBarProps> = ({
  compact = false,
  server,
  onOpenCreateModal,
  onOpenCreateCategoryModal,
  onOpenSettings,
}) => {
  const popoverId = useId();
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
    if (!showPopup || compact) return;
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
  }, [compact, showPopup]);

  // Close add menu on click outside
  useEffect(() => {
    if (!showAddMenu || compact) return;
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
  }, [compact, showAddMenu]);

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

  const addMenuContent = (
    <AddMenuContent
      compact={compact}
      onOpenChannel={() => {
        setShowAddMenu(false);
        onOpenCreateModal();
      }}
      onOpenCategory={() => {
        setShowAddMenu(false);
        onOpenCreateCategoryModal();
      }}
    />
  );

  const invitePopupContent = (
    <InvitePopupContent
      compact={compact}
      activeInvite={activeInvite}
      copied={copied}
      isGenerating={isGenerating}
      onCopyCode={handleCopyCode}
      onGenerate={handleGenerate}
      onSendToFriend={() => {
        setShowPopup(false);
        setShowSendToFriend(true);
      }}
    />
  );

  // No actions available — just a thin spacer
  if (buttonCount === 0) {
    return <div className="channel-actions-spacer" />;
  }

  return (
    <div
      className={`channel-actions-strip${compact ? ' channel-actions-strip--compact' : ''}${buttonCount === 1 ? ' single' : ''}`}
    >
      {canCreateChannel && (
        <AddAction
          compact={compact}
          popoverId={popoverId}
          open={showAddMenu}
          buttonRef={addBtnRef}
          menuRef={addMenuRef}
          content={addMenuContent}
          onToggle={() => setShowAddMenu(!showAddMenu)}
          onClose={() => setShowAddMenu(false)}
        />
      )}

      {canCreateChannel && canInvite && <div className="channel-action-divider" />}

      {canInvite && (
        <InviteAction
          compact={compact}
          popoverId={popoverId}
          open={showPopup}
          buttonRef={btnRef}
          popupRef={popupRef}
          content={invitePopupContent}
          onToggle={handleTogglePopup}
          onClose={() => setShowPopup(false)}
        />
      )}

      {(canCreateChannel || canInvite) && canManageServer && (
        <div className="channel-action-divider" />
      )}

      {canManageServer && <SettingsAction compact={compact} onOpenSettings={onOpenSettings} />}

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
