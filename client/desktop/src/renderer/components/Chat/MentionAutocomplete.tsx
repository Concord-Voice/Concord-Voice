import React, {
  useState,
  useEffect,
  useRef,
  useCallback,
  useMemo,
  useImperativeHandle,
  forwardRef,
} from 'react';
import { resolveMediaUrl } from '../../utils/resolveMediaUrl';
import { AtSign, Users, Shield } from 'lucide-react';
import { useMemberStore } from '../../stores/memberStore';
import { useDMStore } from '../../stores/dmStore';
import { usePermissionStore } from '../../stores/permissionStore';
import { useUserStore } from '../../stores/userStore';
import {
  hasPermission,
  resolveChannelPermissions,
  MENTION_EVERYONE,
  MENTION_USERS,
  MENTION_ROLES,
} from '../../utils/permissions';
import type { ParsedMention } from '../../utils/mentions';
import './MentionAutocomplete.css';

export interface MentionAutocompleteProps {
  /** The current text input value */
  text: string;
  /** Cursor position in the text */
  cursorPosition: number;
  /** Server ID for permission checks (undefined = DM context, all mentions allowed) */
  serverId?: string;
  /**
   * Channel ID for SBAC override resolution. When provided alongside `serverId`,
   * the viewer's effective mention permissions are computed as
   * (server base ⊕ channel allow/deny overrides) via `resolveChannelPermissions`,
   * mirroring the control-plane resolver. Undefined in DM context.
   */
  channelId?: string;
  /** DM conversation ID — when set, source members from DM participants instead of server members */
  conversationId?: string;
  /** Called when a mention is selected */
  onSelect: (mention: ParsedMention, replacementText: string) => void;
  /** Called when the popup should close */
  onClose: () => void;
  /** Position reference element (the textarea) — reserved for future positioning */
  anchorRef?: React.RefObject<HTMLElement | null>;
}

interface MentionOption {
  type: 'user' | 'role' | 'everyone' | 'here';
  id?: string;
  label: string;
  sublabel?: string;
  avatarUrl?: string;
  color?: string;
}

export interface MentionAutocompleteHandle {
  handleKeyDown: (e: React.KeyboardEvent) => boolean;
}

/** Build special mention options (@all, @here) based on permissions and query. */
function buildSpecialMentions(q: string, canEveryone: boolean, canHere: boolean): MentionOption[] {
  const results: MentionOption[] = [];
  if (canEveryone && ('all'.startsWith(q) || 'everyone'.startsWith(q))) {
    results.push({ type: 'everyone', label: 'all', sublabel: 'Notify everyone in this channel' });
  }
  if (canHere && ('here'.startsWith(q) || 'online'.startsWith(q))) {
    results.push({ type: 'here', label: 'here', sublabel: 'Notify online members' });
  }
  return results;
}

/** Build role mention options based on query and server roles. */
function buildRoleMentions(
  q: string,
  roles: { id: string; name: string; mentionable: boolean; is_default: boolean; color?: string }[]
): MentionOption[] {
  const results: MentionOption[] = [];
  for (const role of roles) {
    if (!role.mentionable || role.is_default) continue;
    if (role.name.toLowerCase().startsWith(q)) {
      results.push({
        type: 'role',
        id: role.id,
        label: role.name,
        sublabel: 'Role',
        color: role.color || undefined,
      });
    }
  }
  return results;
}

/** Build user mention options from members matching the query. */
function buildUserMentions(
  q: string,
  members: { user_id: string; username: string; display_name?: string; avatar_url?: string }[]
): MentionOption[] {
  const results: MentionOption[] = [];
  for (const member of members) {
    const username = member.username.toLowerCase();
    const displayName = member.display_name?.toLowerCase() || '';
    if (username.startsWith(q) || displayName.startsWith(q)) {
      results.push({
        type: 'user',
        id: member.user_id,
        label: member.username,
        sublabel: member.display_name || undefined,
        avatarUrl: member.avatar_url || undefined,
      });
    }
  }
  return results;
}

// eslint-disable-next-line @eslint-react/no-forward-ref -- forwardRef retained intentionally; the React 19 ref-as-prop migration would force a full-component re-indent disproportionate to this change
const MentionAutocomplete = forwardRef<MentionAutocompleteHandle, MentionAutocompleteProps>(
  (
    {
      text,
      cursorPosition,
      serverId,
      channelId,
      onSelect,
      onClose,
      anchorRef: _anchorRef,
      conversationId,
    },
    ref
  ) => {
    const [selectedIndex, setSelectedIndex] = useState(0);
    const listRef = useRef<HTMLDivElement>(null);

    const serverMembers = useMemberStore((s) => s.members);
    const dmConversations = useDMStore((s) => s.conversations);

    // In DM context, source mention candidates from conversation participants
    // instead of server members (#6: prevents generating mentions for non-participants)
    const members = useMemo(() => {
      if (conversationId) {
        const conv = dmConversations.find((c) => c.id === conversationId);
        if (conv) {
          return conv.participants.map((p) => ({
            user_id: p.userId,
            username: p.username,
            display_name: p.displayName,
            avatar_url: p.avatarUrl,
            role: 'member' as const,
            joined_at: '',
            roles: [],
          }));
        }
      }
      return serverMembers;
    }, [conversationId, dmConversations, serverMembers]);
    const serverPermissions = usePermissionStore((s) => s.serverPermissions);
    const serverRoles = usePermissionStore((s) => s.serverRoles);
    const channelOverrides = usePermissionStore((s) => s.channelOverrides);
    const viewerUserId = useUserStore((s) => s.user?.id ?? '');

    // The viewer's role ids + owner flag in this server, used to resolve SBAC overrides.
    // Owner detection mirrors the backend owner-id bypass (resolver.go step 2): OwnerPermissions
    // does NOT carry the ADMINISTRATOR bit, so the resolver needs the explicit owner signal to
    // exempt owners from channel overrides the way the server does.
    const { viewerRoleIds, viewerIsOwner } = useMemo(() => {
      const self = serverMembers.find((m) => m.user_id === viewerUserId);
      return {
        viewerRoleIds: new Set((self?.roles ?? []).map((r) => r.role_id)),
        viewerIsOwner: self?.role === 'owner',
      };
    }, [serverMembers, viewerUserId]);

    // Extract the @query from text at cursor position
    const query = useMemo(() => {
      // Walk backwards from cursor to find the @ trigger
      let i = cursorPosition - 1;
      while (i >= 0 && text[i] !== '@' && text[i] !== ' ' && text[i] !== '\n') {
        i--;
      }
      if (i < 0 || text[i] !== '@') return null;
      // Ensure @ is at start of line or preceded by whitespace
      if (i > 0 && text[i - 1] !== ' ' && text[i - 1] !== '\n') return null;
      return {
        text: text.slice(i + 1, cursorPosition).toLowerCase(),
        startIndex: i,
      };
    }, [text, cursorPosition]);

    // Compute effective permissions (server base + channel SBAC overrides)
    const permissions = useMemo(() => {
      if (!serverId) {
        // DM context — users and @here allowed, but NOT @all/@everyone
        // (server strips addendum.Everyone in DMs — showing it would be misleading UX)
        return {
          canMentionEveryone: false,
          canMentionHere: true,
          canMentionUsers: true,
          canMentionRoles: false,
        };
      }
      const basePerm = serverPermissions[serverId] ?? 0n;
      // Fold channel-level SBAC overrides into the base permission when a channel is in
      // context, mirroring the control-plane resolver (internal/rbac/resolver.go). The
      // server is the enforcement boundary; this only narrows/widens what we *suggest*.
      const effectivePerm = channelId
        ? resolveChannelPermissions(
            basePerm,
            channelOverrides[channelId],
            viewerUserId,
            viewerRoleIds,
            viewerIsOwner
          )
        : basePerm;
      const hasMentionEveryone = hasPermission(effectivePerm, MENTION_EVERYONE);
      return {
        canMentionEveryone: hasMentionEveryone,
        canMentionHere: hasMentionEveryone, // @here requires same permission as @all in server context
        canMentionUsers: hasPermission(effectivePerm, MENTION_USERS),
        canMentionRoles: hasPermission(effectivePerm, MENTION_ROLES),
      };
    }, [
      serverId,
      channelId,
      serverPermissions,
      channelOverrides,
      viewerUserId,
      viewerRoleIds,
      viewerIsOwner,
    ]);

    // Build filtered options
    const options = useMemo(() => {
      if (!query) return [];
      const q = query.text;
      const results: MentionOption[] = [
        ...buildSpecialMentions(q, permissions.canMentionEveryone, permissions.canMentionHere),
        ...(permissions.canMentionRoles && serverId
          ? buildRoleMentions(q, serverRoles[serverId] || [])
          : []),
        ...(permissions.canMentionUsers ? buildUserMentions(q, members) : []),
      ];
      return results.slice(0, 15); // Cap at 15 results
    }, [query, permissions, members, serverRoles, serverId]);

    // Reset selection when options change
    useEffect(() => {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: reset the highlighted index to the top whenever the (single, memoized) option list changes
      setSelectedIndex(0);
    }, [options]);

    // Scroll selected item into view
    useEffect(() => {
      if (!listRef.current) return;
      const selected = listRef.current.children[selectedIndex] as HTMLElement | undefined;
      selected?.scrollIntoView({ block: 'nearest' });
    }, [selectedIndex]);

    const handleSelect = useCallback(
      (option: MentionOption) => {
        if (!query) return;

        // Use token format for users and roles to handle names with spaces/special chars.
        // Token format: <@userId> for users, <@&roleId> for roles, @all/@here for specials.
        // The Message renderer decodes tokens back to display names.
        let replacementText: string;
        let rawText: string;
        if (option.type === 'user' && option.id) {
          replacementText = `<@${option.id}> `;
          rawText = `<@${option.id}>`;
        } else if (option.type === 'role' && option.id) {
          replacementText = `<@&${option.id}> `;
          rawText = `<@&${option.id}>`;
        } else {
          replacementText = `@${option.label} `;
          rawText = `@${option.label}`;
        }

        const mention: ParsedMention = {
          type: option.type,
          raw: rawText,
          start: query.startIndex,
          end: cursorPosition,
          id: option.id,
          label: option.label,
        };

        onSelect(mention, replacementText);
      },
      [query, cursorPosition, onSelect]
    );

    // Keyboard navigation (called from MessageInput's onKeyDown)
    const handleKeyDown = useCallback(
      (e: React.KeyboardEvent) => {
        if (options.length === 0) return false;

        switch (e.key) {
          case 'ArrowDown':
            e.preventDefault();
            setSelectedIndex((i) => (i + 1) % options.length);
            return true;
          case 'ArrowUp':
            e.preventDefault();
            setSelectedIndex((i) => (i - 1 + options.length) % options.length);
            return true;
          case 'Enter':
          case 'Tab':
            e.preventDefault();
            handleSelect(options[selectedIndex]);
            return true;
          case 'Escape':
            e.preventDefault();
            onClose();
            return true;
          default:
            return false;
        }
      },
      [options, selectedIndex, handleSelect, onClose]
    );

    // Expose handleKeyDown to parent via ref
    useImperativeHandle(ref, () => ({ handleKeyDown }), [handleKeyDown]);

    if (!query || options.length === 0) return null;

    return (
      <div
        className="mention-autocomplete"
        role="listbox"
        aria-label="Mention suggestions"
        aria-activedescendant={options[selectedIndex] ? `mention-opt-${selectedIndex}` : undefined}
        tabIndex={-1}
        ref={listRef}
      >
        {options.map((option, i) => (
          <div
            key={`${option.type}-${option.id || option.label}`}
            id={`mention-opt-${i}`}
            className={`mention-option ${i === selectedIndex ? 'selected' : ''}`}
            role="option"
            tabIndex={-1}
            aria-selected={i === selectedIndex}
            onMouseDown={(e) => {
              e.preventDefault(); // Prevent textarea blur
              handleSelect(option);
            }}
            onMouseEnter={() => setSelectedIndex(i)}
          >
            <span className="mention-option-icon">
              {(() => {
                if (option.type === 'everyone' || option.type === 'here')
                  return <Users size={16} />;
                if (option.type === 'role')
                  return (
                    <Shield size={16} style={option.color ? { color: option.color } : undefined} />
                  );
                if (resolveMediaUrl(option.avatarUrl))
                  return (
                    <img
                      src={resolveMediaUrl(option.avatarUrl)}
                      alt=""
                      className="mention-option-avatar"
                    />
                  );
                return <AtSign size={16} />;
              })()}
            </span>
            <span className="mention-option-label">
              {option.type === 'role' && option.color ? (
                <span style={{ color: option.color }}>{option.label}</span>
              ) : (
                option.label
              )}
            </span>
            {option.sublabel && <span className="mention-option-sublabel">{option.sublabel}</span>}
          </div>
        ))}
      </div>
    );
  }
);

MentionAutocomplete.displayName = 'MentionAutocomplete';

export default MentionAutocomplete;
export type { MentionOption };
