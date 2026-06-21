import React, { useState, useRef, useEffect, useCallback } from 'react';
import { useSettingsOverlayStore } from '../../stores/settingsOverlayStore';
import ImageCropEditor from '../ui/ImageCropEditor';
import LoadingSpinner from '../Auth/LoadingSpinner';
import ToggleSwitch from '../Settings/ToggleSwitch';
import RoleEditorPanel from './RoleEditorPanel';
import MemberListPanel from './MemberListPanel';
import IconUploadArea from './IconUploadArea';
import BannerUploadArea from './BannerUploadArea';
import { useImageUpload } from '../../hooks/useImageUpload';
import {
  MAX_ICON_SIZE,
  MAX_BANNER_SIZE,
  ALLOWED_TYPES,
  NAME_MIN,
  NAME_MAX,
  type ServerFormErrors,
} from './serverConstants';
import { useServerStore } from '../../stores/serverStore';
import { useInviteStore } from '../../stores/inviteStore';
import { usePermissionStore } from '../../stores/permissionStore';
import { useMemberStore } from '../../stores/memberStore';
import { Permissions } from '../../utils/permissions';
import { apiFetch } from '../../services/apiClient';
import type { ServerInviteWithCreator, Role } from '../../types/server';
import './ServerSettingsPage.css';

const EMPTY_INVITES: ServerInviteWithCreator[] = [];
const EMPTY_ROLES: Role[] = [];

type SettingsSection = 'general' | 'roles' | 'members';

interface NavItem {
  id: SettingsSection;
  label: string;
  icon: React.ReactNode;
  permCheck?: boolean;
}

const NAV_SUBSECTIONS: Record<string, { id: string; label: string }[]> = {
  general: [
    { id: 'server-info', label: 'Server Info' },
    { id: 'content-safety', label: 'Content Safety' },
    { id: 'invite-code', label: 'Invite Code' },
  ],
  members: [{ id: 'member-list', label: 'Member List' }],
};

interface ServerSettingsPageProps {
  serverId: string;
}

const ServerSettingsPage: React.FC<ServerSettingsPageProps> = ({ serverId }) => {
  const closeOverlay = useSettingsOverlayStore((s) => s.close);
  const servers = useServerStore((s) => s.servers);
  const server = servers.find((s) => s.id === serverId);

  const [activeSection, setActiveSection] = useState<SettingsSection>('general');
  const [activeSubsection, setActiveSubsection] = useState<string | null>(null);
  const contentRef = useRef<HTMLDivElement>(null);

  // General tab state
  const [name, setName] = useState(server?.name || '');
  const [errors, setErrors] = useState<ServerFormErrors>({});
  const [isSubmitting, setIsSubmitting] = useState(false);
  const [successMessage, setSuccessMessage] = useState<string | null>(null);
  const [inviteCopied, setInviteCopied] = useState(false);
  const [isCreatingInvite, setIsCreatingInvite] = useState(false);
  const [allowEmbeddedContent, setAllowEmbeddedContent] = useState(
    server?.allow_embedded_content ?? false
  );

  // Image upload hooks
  const icon = useImageUpload({
    maxSize: MAX_ICON_SIZE,
    allowedTypes: ALLOWED_TYPES,
    onError: (msg) => setErrors((prev) => ({ ...prev, icon: msg })),
    initialUrl: server?.icon_url,
  });

  const banner = useImageUpload({
    maxSize: MAX_BANNER_SIZE,
    allowedTypes: ALLOWED_TYPES,
    onError: (msg) => setErrors((prev) => ({ ...prev, banner: msg })),
    initialUrl: server?.banner_url,
  });

  const hasServerPerm = usePermissionStore((s) => s.hasServerPermission);
  const canManageServer = server ? hasServerPerm(server.id, Permissions.MANAGE_SERVER) : false;
  const canManageInvites = server ? hasServerPerm(server.id, Permissions.INVITE) : false;
  const canManageRoles = server ? hasServerPerm(server.id, Permissions.MANAGE_ROLES) : false;
  const canAssignRoles = server ? hasServerPerm(server.id, Permissions.MANAGE_ROLES_ASSIGN) : false;
  const invites = useInviteStore((state) => state.invites[serverId] ?? EMPTY_INVITES);
  const fetchInvites = useInviteStore((state) => state.fetchInvites);
  const createInvite = useInviteStore((state) => state.createInvite);

  // Permission store - roles
  const serverRoles = usePermissionStore((s) => s.serverRoles[serverId] ?? EMPTY_ROLES);
  const fetchRoles = usePermissionStore((s) => s.fetchRoles);
  const createRole = usePermissionStore((s) => s.createRole);
  const updateRole = usePermissionStore((s) => s.updateRole);
  const deleteRole = usePermissionStore((s) => s.deleteRole);
  const assignRole = usePermissionStore((s) => s.assignRole);
  const unassignRole = usePermissionStore((s) => s.unassignRole);

  // Member store
  const members = useMemberStore((s) => s.members);
  const fetchMembers = useMemberStore((s) => s.fetchMembers);

  // Nav items (built dynamically based on permissions)
  const navItems: NavItem[] = [
    {
      id: 'general',
      label: 'General',
      icon: (
        <svg width="18" height="18" viewBox="0 0 18 18" fill="none">
          <path
            d="M9 11.25a2.25 2.25 0 100-4.5 2.25 2.25 0 000 4.5z"
            stroke="currentColor"
            strokeWidth="1.5"
          />
          <path
            d="M14.7 11.1a1.2 1.2 0 00.24 1.32l.04.04a1.46 1.46 0 11-2.06 2.06l-.04-.04a1.2 1.2 0 00-1.32-.24 1.2 1.2 0 00-.73 1.1v.12a1.46 1.46 0 01-2.91 0v-.06a1.2 1.2 0 00-.79-1.1 1.2 1.2 0 00-1.32.24l-.04.04a1.46 1.46 0 11-2.06-2.06l.04-.04a1.2 1.2 0 00.24-1.32 1.2 1.2 0 00-1.1-.73h-.12a1.46 1.46 0 010-2.91h.06a1.2 1.2 0 001.1-.79 1.2 1.2 0 00-.24-1.32l-.04-.04a1.46 1.46 0 112.06-2.06l.04.04a1.2 1.2 0 001.32.24h.06a1.2 1.2 0 00.73-1.1v-.12a1.46 1.46 0 012.91 0v.06a1.2 1.2 0 00.73 1.1 1.2 1.2 0 001.32-.24l.04-.04a1.46 1.46 0 112.06 2.06l-.04.04a1.2 1.2 0 00-.24 1.32v.06a1.2 1.2 0 001.1.73h.12a1.46 1.46 0 010 2.91h-.06a1.2 1.2 0 00-1.1.73z"
            stroke="currentColor"
            strokeWidth="1.5"
          />
        </svg>
      ),
    },
    {
      id: 'roles',
      label: 'Roles',
      permCheck: canManageRoles,
      icon: (
        <svg width="18" height="18" viewBox="0 0 18 18" fill="none">
          <path
            d="M9 1.5l1.35 2.74 3.02.44-2.19 2.13.52 3.01L9 8.36 6.3 9.82l.52-3.01-2.19-2.13 3.02-.44L9 1.5z"
            stroke="currentColor"
            strokeWidth="1.5"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
          <path
            d="M5.5 11.5v3.5a1 1 0 001 1h5a1 1 0 001-1v-3.5"
            stroke="currentColor"
            strokeWidth="1.5"
            strokeLinecap="round"
          />
        </svg>
      ),
    },
    {
      id: 'members',
      label: 'Members',
      permCheck: canAssignRoles,
      icon: (
        <svg width="18" height="18" viewBox="0 0 18 18" fill="none">
          <circle cx="7" cy="6" r="2.5" stroke="currentColor" strokeWidth="1.5" />
          <path
            d="M2 15c0-2.49 2.24-4.5 5-4.5s5 2.01 5 4.5"
            stroke="currentColor"
            strokeWidth="1.5"
            strokeLinecap="round"
          />
          <circle cx="13" cy="7" r="2" stroke="currentColor" strokeWidth="1.5" />
          <path
            d="M14 10.5c1.66.63 3 2.15 3 3.5"
            stroke="currentColor"
            strokeWidth="1.5"
            strokeLinecap="round"
          />
        </svg>
      ),
    },
  ];

  const visibleNavItems = navItems.filter((item) => item.permCheck === undefined || item.permCheck);

  // Find first active invite
  const activeInvite = invites.find((inv) => {
    if (inv.is_revoked) return false;
    if (inv.expires_at && new Date(inv.expires_at) < new Date()) return false;
    if (inv.max_uses !== null && inv.use_count >= inv.max_uses) return false;
    return true;
  });

  // Extract stable reset functions so the init effect can depend on them
  // without churning on every render (useImageUpload returns a fresh object
  // each render; reset itself is memoized with [] deps inside the hook).
  const iconReset = icon.reset;
  const bannerReset = banner.reset;

  // Reset form when the selected server changes. Keyed on server?.id so we
  // only reinitialize on server switch — not when the server record is
  // mutated in place (e.g. after this form saves a name change). Reading
  // server.name/icon_url/etc. inside is intentional: we want the snapshot
  // at switch time, not a live binding. Hence server/icon/banner are
  // deliberately omitted from deps.
  //
  // Rationale for the eslint-disable below:
  //  - Fields are read as a snapshot-at-switch so that an in-place mutation
  //    in the store (e.g., from this form's own save) does NOT reset the
  //    user's unsaved edits.
  //  - iconReset / bannerReset are stable useCallback references from
  //    useImageUpload (extracted at :190-191) and don't need to be listed.
  //  - Edge case: if `server` goes from undefined → defined without its id
  //    changing (e.g., a race where serverId is set before the store has
  //    the record), the `server?.id` key flips from `undefined` to a
  //    string, so this effect re-runs. Verified by inspection of the
  //    `servers.find(s => s.id === serverId)` upstream at :56.
  useEffect(() => {
    if (server) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets name from server prop on server identity change (snapshot-on-switch); not a render loop
      setName(server.name);
      iconReset(server.icon_url || null);
      bannerReset(server.banner_url || null);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears errors on server identity change; not a render loop
      setErrors({});
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets isSubmitting on server identity change; not a render loop
      setIsSubmitting(false);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears successMessage on server identity change; not a render loop
      setSuccessMessage(null);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets inviteCopied on server identity change; not a render loop
      setInviteCopied(false);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets isCreatingInvite on server identity change; not a render loop
      setIsCreatingInvite(false);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets allowEmbeddedContent from server prop on server identity change; not a render loop
      setAllowEmbeddedContent(server.allow_embedded_content ?? false);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets activeSection on server identity change; not a render loop
      setActiveSection('general');
    }
    // eslint-disable-next-line @eslint-react/exhaustive-deps -- snapshot-on-switch; see block comment above effect
  }, [server?.id]);

  // Fetch invites
  useEffect(() => {
    if (serverId && canManageInvites) {
      fetchInvites(serverId);
    }
  }, [serverId, canManageInvites, fetchInvites]);

  // Fetch roles when roles tab is active
  useEffect(() => {
    if (serverId && activeSection === 'roles' && canManageRoles) {
      fetchRoles(serverId);
    }
  }, [activeSection, canManageRoles, serverId, fetchRoles]);

  // Fetch members when members tab is active
  useEffect(() => {
    if (serverId && activeSection === 'members' && canAssignRoles) {
      fetchMembers(serverId);
      fetchRoles(serverId);
    }
  }, [activeSection, canAssignRoles, serverId, fetchMembers, fetchRoles]);

  // IntersectionObserver for subsection tracking
  const visibleSectionsRef = useRef<Map<string, IntersectionObserverEntry>>(new Map());

  useEffect(() => {
    visibleSectionsRef.current.clear();
    const root = contentRef.current?.closest('.settings-page-content') as HTMLElement | null;
    if (!root) return;

    // Held in this effect-scoped variable so the cleanup disconnects it
    // directly (no DOM expando — #483).
    let observer: IntersectionObserver | null = null;

    const timer = setTimeout(() => {
      observer = new IntersectionObserver(
        (entries) => {
          for (const entry of entries) {
            const id = entry.target.id;
            if (entry.isIntersecting) {
              visibleSectionsRef.current.set(id, entry);
            } else {
              visibleSectionsRef.current.delete(id);
            }
          }
          let best: IntersectionObserverEntry | null = null;
          for (const entry of visibleSectionsRef.current.values()) {
            if (!best || entry.boundingClientRect.top < best.boundingClientRect.top) {
              best = entry;
            }
          }
          if (best) {
            setActiveSubsection(best.target.id.replace('section-', ''));
          }
        },
        { root, threshold: [0, 0.1, 0.25, 0.5], rootMargin: '-10% 0px -50% 0px' }
      );

      const sections = root.querySelectorAll('[id^="section-"]');
      for (const el of sections) observer.observe(el);
    }, 50);

    return () => {
      clearTimeout(timer);
      observer?.disconnect();
    };
  }, [activeSection]);

  const scrollToSection = useCallback((sectionId: string) => {
    const el = document.getElementById(`section-${sectionId}`);
    if (!el) return;
    el.scrollIntoView({ behavior: 'smooth', block: 'start' });
  }, []);

  // Redirect if server not found
  if (!server) {
    return (
      <div className="view-container settings-fullpage">
        <div className="settings-page-content">
          <div className="settings-page-inner">
            <p style={{ color: 'var(--text-muted)', textAlign: 'center', padding: '40px' }}>
              Server not found.
            </p>
          </div>
        </div>
      </div>
    );
  }

  // ─── Form handlers ───

  const validateForm = (): boolean => {
    const newErrors: ServerFormErrors = {};
    const trimmed = name.trim();
    if (!trimmed) {
      newErrors.name = 'Server name is required';
    } else if (trimmed.length < NAME_MIN) {
      newErrors.name = `Server name must be at least ${NAME_MIN} characters`;
    } else if (trimmed.length > NAME_MAX) {
      newErrors.name = `Server name must be at most ${NAME_MAX} characters`;
    }
    setErrors(newErrors);
    return Object.keys(newErrors).length === 0;
  };

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setErrors({});
    setSuccessMessage(null);
    if (!validateForm()) return;
    setIsSubmitting(true);
    try {
      const body: {
        name: string;
        icon_url?: string | null;
        banner_url?: string | null;
        allow_embedded_content?: boolean;
      } = {
        name: name.trim(),
      };
      if (icon.removed) {
        body.icon_url = null;
      } else if (icon.imageUrl && icon.imageUrl !== server.icon_url) {
        body.icon_url = icon.imageUrl;
      }
      if (banner.removed) {
        body.banner_url = null;
      } else if (banner.imageUrl && banner.imageUrl !== server.banner_url) {
        body.banner_url = banner.imageUrl;
      }
      if (allowEmbeddedContent !== server.allow_embedded_content) {
        body.allow_embedded_content = allowEmbeddedContent;
      }
      const response = await apiFetch(`/api/v1/servers/${server.id}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      const data = await response.json();
      if (!response.ok) throw new Error(data.error || 'Failed to update server');
      useServerStore.getState().updateServer(server.id, {
        name: data.server.name,
        icon_url: data.server.icon_url,
        banner_url: data.server.banner_url,
        allow_embedded_content: data.server.allow_embedded_content,
        updated_at: data.server.updated_at,
      });
      setSuccessMessage('Server updated successfully!');
      setIsSubmitting(false);
    } catch (error) {
      setErrors({
        general:
          error instanceof Error ? error.message : 'Failed to update server. Please try again.',
      });
      setIsSubmitting(false);
    }
  };

  // ─── Roles handlers ───

  const handleCreateRole = async () => {
    const existingNames = new Set(serverRoles.map((r) => r.name));
    let roleName = 'New Role';
    let counter = 2;
    while (existingNames.has(roleName)) {
      roleName = `New Role ${counter++}`;
    }
    return await createRole(server.id, {
      name: roleName,
      color: '#99aab5',
      permissions: '0',
    });
  };

  const handleSaveRole = async (
    roleId: string,
    data: {
      name: string;
      color: string;
      emoji: string;
      permissions: string;
      display_separately: boolean;
      mentionable: boolean;
    }
  ) => {
    await updateRole(server.id, roleId, data);
  };

  const handleDeleteRole = async (roleId: string) => {
    await deleteRole(server.id, roleId);
  };

  // ─── Members handlers ───

  const handleToggleRole = async (userId: string, roleId: string, hasRole: boolean) => {
    if (hasRole) {
      await unassignRole(server.id, userId, roleId);
    } else {
      await assignRole(server.id, userId, roleId);
    }
    await fetchMembers(server.id);
  };

  const assignableRoles = serverRoles.filter((r) => !r.is_default);

  // ─── Section renderers ───

  const renderGeneralSection = () => (
    <form className="server-settings-form" onSubmit={handleSubmit}>
      <div className="settings-section" id="section-server-info">
        <h2 className="settings-section-title">Server Info</h2>

        <IconUploadArea
          preview={icon.preview}
          error={errors.icon}
          onClick={icon.handleClick}
          onKeyDown={icon.handleKeyDown}
          onRemove={icon.handleRemove}
          onFileChange={icon.handleChange}
          fileInputRef={icon.fileInputRef}
        />

        {/* Banner Upload */}
        <BannerUploadArea
          preview={banner.preview}
          error={errors.banner}
          onClick={banner.handleClick}
          onKeyDown={banner.handleKeyDown}
          onRemove={banner.handleRemove}
          onFileChange={banner.handleChange}
          fileInputRef={banner.fileInputRef}
          hint="PNG, JPEG, GIF, WebP — max 2MB. Optional."
        />

        {/* Server Name */}
        <div className="form-group">
          <label htmlFor="server-settings-name" className="form-label">
            Server Name
          </label>
          <input
            id="server-settings-name"
            type="text"
            className={`form-input ${errors.name ? 'error' : ''}`}
            placeholder="My Awesome Server"
            value={name}
            onChange={(e) => {
              setName(e.target.value);
              if (errors.name) setErrors((prev) => ({ ...prev, name: undefined }));
            }}
            disabled={isSubmitting}
            maxLength={NAME_MAX}
          />
          {errors.name && <span className="form-error">{errors.name}</span>}
          <span className="form-hint">
            {name.trim().length}/{NAME_MAX} characters
          </span>
        </div>

        {/* General Error */}
        {errors.general && (
          <div className="form-error-banner">
            <span>{errors.general}</span>
          </div>
        )}

        {/* Success Message */}
        {successMessage && (
          <div className="form-success-banner">
            <span>{successMessage}</span>
          </div>
        )}

        {/* Save button */}
        <div className="server-settings-actions">
          <button
            type="submit"
            className="server-settings-submit-btn"
            disabled={isSubmitting || !!successMessage}
          >
            {isSubmitting ? (
              <>
                Saving...
                <LoadingSpinner size="small" inline />
              </>
            ) : (
              'Save Changes'
            )}
          </button>
        </div>
      </div>

      {/* Content Safety Section */}
      <div className="settings-section" id="section-content-safety">
        <h2 className="settings-section-title">Content Safety</h2>
        <div className="settings-row">
          <div className="settings-row-info">
            <span className="settings-row-label">Allow Embedded Content</span>
            <span className="settings-row-hint">
              {allowEmbeddedContent
                ? 'Embeds are enabled. Messages may render link previews, images, and other embedded content. Moderators can still suppress embeds on individual messages.'
                : 'Embeds are blocked server-wide (default). All message embeds are suppressed before reaching any client. Enable to allow link previews and images in messages.'}
            </span>
            {!canManageServer && (
              <span className="settings-row-hint settings-permission-note">
                Requires Manage Server permission (admin or owner).
              </span>
            )}
          </div>
          <ToggleSwitch
            checked={allowEmbeddedContent}
            onChange={setAllowEmbeddedContent}
            disabled={isSubmitting || !canManageServer}
          />
        </div>
      </div>

      {/* Invite Code Section */}
      {canManageInvites && (
        <div className="settings-section" id="section-invite-code">
          <h2 className="settings-section-title">Invite Code</h2>
          {activeInvite ? (
            <div className="invite-code-row">
              <code className="invite-code-display">{activeInvite.code}</code>
              <button
                type="button"
                className="invite-copy-btn"
                onClick={() => {
                  navigator.clipboard.writeText(activeInvite.code);
                  setInviteCopied(true);
                  setTimeout(() => setInviteCopied(false), 2000);
                }}
              >
                {inviteCopied ? 'Copied!' : 'Copy'}
              </button>
            </div>
          ) : (
            <button
              type="button"
              className="invite-generate-btn"
              disabled={isCreatingInvite}
              onClick={async () => {
                setIsCreatingInvite(true);
                await createInvite(server.id);
                setIsCreatingInvite(false);
              }}
            >
              {isCreatingInvite ? (
                <>
                  Generating...
                  <LoadingSpinner size="small" inline />
                </>
              ) : (
                'Generate Invite Code'
              )}
            </button>
          )}
          <span className="form-hint">Invites default to 1 use, expire in 24 hours.</span>
        </div>
      )}
      {/* Icon Crop Editor */}
      <ImageCropEditor
        isOpen={icon.showCrop}
        onClose={icon.handleCropCancel}
        onConfirm={icon.handleCropConfirm}
        imageFile={icon.pendingFile}
        title="Crop Server Icon"
        cropShape={{ type: 'circle' }}
        output={{ width: 512, height: 512, quality: 0.9 }}
        upload={{
          endpoint: '/api/v1/media/upload/server-icon',
          extraFields: { server_id: server.id },
        }}
      />

      {/* Banner Crop Editor */}
      <ImageCropEditor
        isOpen={banner.showCrop}
        onClose={banner.handleCropCancel}
        onConfirm={banner.handleCropConfirm}
        imageFile={banner.pendingFile}
        title="Crop Server Banner"
        cropShape={{ type: 'rectangle' }}
        output={{ width: 1200, height: 240, quality: 0.9 }}
        upload={{
          endpoint: '/api/v1/media/upload/server-banner',
          extraFields: { server_id: server.id },
        }}
      />
    </form>
  );

  const renderRolesSection = () => (
    <div className="settings-section" id="section-role-list">
      <h2 className="settings-section-title">Roles</h2>
      <RoleEditorPanel
        roles={serverRoles}
        onCreateRole={handleCreateRole}
        onSaveRole={handleSaveRole}
        onDeleteRole={handleDeleteRole}
      />
    </div>
  );

  const renderMembersSection = () => (
    <div className="settings-section" id="section-member-list">
      <h2 className="settings-section-title">Member List</h2>
      <MemberListPanel
        members={members}
        assignableRoles={assignableRoles}
        onToggleRole={handleToggleRole}
        serverId={server.id}
        ownerUserId={server.owner_id}
      />
    </div>
  );

  const renderSection = () => {
    switch (activeSection) {
      case 'general':
        return renderGeneralSection();
      case 'roles':
        return canManageRoles ? renderRolesSection() : null;
      case 'members':
        return canAssignRoles ? renderMembersSection() : null;
      default:
        return null;
    }
  };

  return (
    <div className="view-container settings-fullpage">
      <div className="settings-page-content">
        <div className="settings-page-inner">
          <div className="settings-layout">
            {/* Left sidebar */}
            <nav className="settings-nav">
              <div className="settings-nav-scroll">
                <button className="settings-back-btn" onClick={closeOverlay}>
                  <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
                    <path
                      d="M10 12L6 8l4-4"
                      stroke="currentColor"
                      strokeWidth="1.5"
                      strokeLinecap="round"
                      strokeLinejoin="round"
                    />
                  </svg>
                  Back to app
                </button>

                <h1 className="settings-page-title">Server Settings</h1>

                {visibleNavItems.map((item) => {
                  const isActive = activeSection === item.id;
                  const subs = isActive ? NAV_SUBSECTIONS[item.id] : undefined;

                  return (
                    <div key={item.id} className="settings-nav-group">
                      <button
                        className={`settings-nav-item ${isActive ? 'active' : ''}`}
                        onClick={() => setActiveSection(item.id)}
                      >
                        <span className="settings-nav-item-icon">{item.icon}</span>
                        {item.label}
                      </button>

                      {subs && subs.length > 0 && (
                        <div className="settings-nav-tree">
                          {subs.map((sub, i) => (
                            <div
                              key={sub.id}
                              className={`settings-nav-tree-item-wrapper${i === subs.length - 1 ? ' settings-nav-tree-item-wrapper--last' : ''}`}
                            >
                              <button
                                className={`settings-nav-tree-item${activeSubsection === sub.id ? ' active' : ''}`}
                                onClick={() => scrollToSection(sub.id)}
                              >
                                {sub.label}
                              </button>
                            </div>
                          ))}
                        </div>
                      )}
                    </div>
                  );
                })}
              </div>
            </nav>

            {/* Right content */}
            <div ref={contentRef} className="settings-content">
              {renderSection()}
            </div>
          </div>
        </div>
      </div>
    </div>
  );
};

export default ServerSettingsPage;
