import { useState, useCallback } from 'react';
import { MessageWithStatus, type ChatContextType } from '../types/chat';
import { useMemberStore, type ServerMember } from '../stores/memberStore';
import { useFriendStore } from '../stores/friendStore';
import { useSettingsStore } from '../stores/settingsStore';

const EMPTY_MEMBERS: never[] = [];

/**
 * Subset of {@link components/Members/MemberProfileCard.ProfileCardMember}
 * that this hook can resolve from chat-message context. Defined locally so
 * the hook does not need to import anything from the components layer — the
 * architectural lint rule (Sonar typescript:S6804) forbids hooks → components
 * dependencies. TypeScript structural typing lets {@link MessageProfileCardHost}
 * pass this directly to `<MemberProfileCard>` without a cast: every field here
 * is present (and optional) on the full ProfileCardMember type, so a
 * ResolvedProfileCardMember is assignable.
 */
export interface ResolvedProfileCardMember {
  user_id: string;
  username: string;
  display_name?: string;
  avatar_url?: string;
  color_scheme?: string;
}

export interface ProfileCardSelection {
  member: ResolvedProfileCardMember;
  serverMember: ServerMember | null;
  position: { x: number; y: number };
}

interface UseMessageProfileCardArgs {
  message: MessageWithStatus;
  currentUserId: string;
  chatContext: ChatContextType;
}

export interface UseMessageProfileCardResult {
  /** Open (or toggle-close) the profile card anchored at the given viewport point. */
  openProfileAt: (position: { x: number; y: number }) => void;
  /** Close the currently-open profile card, if any. */
  closeProfileCard: () => void;
  /** Currently-displayed profile-card selection, or null when closed. */
  selectedMember: ProfileCardSelection | null;
  /** Currently-displayed full-profile modal target, or null when closed. */
  fullProfileMember: ServerMember | null;
  /** Open the full-profile modal for `member` and close the profile card. */
  openFullProfile: (member: ServerMember) => void;
  /** Close the full-profile modal. */
  closeFullProfile: () => void;
}

/**
 * Owns the profile-card / full-profile-modal state for a single chat message
 * and resolves the best-available member data for the message author. Lifted
 * out of MessageAvatar (#226) so BOTH the avatar and the username can open the
 * same card from a single shared state — clicking either triggers the same
 * popover (which now carries the Send Friend Request action). State lives in
 * the Message parent; the avatar and username are pure triggers.
 *
 * **Architecture:** this hook is intentionally pure (no JSX, no component
 * imports). Rendering the resolved state into `<MemberProfileCard>` and
 * `<UserProfileModal>` is the job of `MessageProfileCardHost`, which sits in
 * the components layer. Splitting the responsibilities lets Concord's
 * architecture lint (Sonar typescript:S6804) hold the line that hooks/ does
 * not depend on components/.
 */
export function useMessageProfileCard({
  message,
  currentUserId,
  chatContext,
}: UseMessageProfileCardArgs): UseMessageProfileCardResult {
  const [selectedMember, setSelectedMember] = useState<ProfileCardSelection | null>(null);
  const [fullProfileMember, setFullProfileMember] = useState<ServerMember | null>(null);

  const isDM = chatContext === 'dm';
  const members = useMemberStore((state) => (isDM ? EMPTY_MEMBERS : state.members));

  const openProfileAt = useCallback(
    (position: { x: number; y: number }) => {
      // Toggle: if card is already open for this user, close it.
      if (selectedMember?.member.user_id === message.user_id) {
        setSelectedMember(null);
        return;
      }

      // In server contexts, prefer the server member store (full data: role,
      // joined_at, bio, etc.).
      if (chatContext !== 'dm') {
        const serverMember = members.find((m) => m.user_id === message.user_id);
        if (serverMember) {
          setSelectedMember({ member: serverMember, serverMember, position });
          return;
        }
      }

      // Fall back to the friend store (colorScheme, avatarUrl, displayName).
      const friend = useFriendStore.getState().friends.find((f) => f.userId === message.user_id);
      if (friend) {
        setSelectedMember({
          member: {
            user_id: friend.userId,
            username: friend.username,
            display_name: friend.displayName,
            avatar_url: friend.avatarUrl,
            color_scheme: friend.colorScheme,
          },
          serverMember: null,
          position,
        });
        return;
      }

      // Last resort: construct from message data. For the current user's own
      // author info, pull color_scheme from settings so the card themes right.
      let colorScheme: string | undefined;
      if (message.user_id === currentUserId) {
        const { colorScheme: scheme, customColors } = useSettingsStore.getState().appearance;
        const themeMode =
          useSettingsStore.getState().appearance.theme === 'light' ? 'light' : 'dark';
        if (scheme === 'custom' && customColors) {
          colorScheme = JSON.stringify({
            scheme: 'custom',
            themeMode,
            accentPrimary: customColors.accentPrimary,
            accentSecondary: customColors.accentSecondary,
          });
        } else {
          colorScheme = JSON.stringify({ scheme, themeMode });
        }
      }

      setSelectedMember({
        member: {
          user_id: message.user_id,
          username: message.username,
          display_name: message.display_name,
          avatar_url: message.avatar_url,
          color_scheme: colorScheme,
        },
        serverMember: null,
        position,
      });
    },
    [
      members,
      message.user_id,
      message.username,
      message.display_name,
      message.avatar_url,
      currentUserId,
      selectedMember,
      chatContext,
    ]
  );

  const closeProfileCard = useCallback(() => setSelectedMember(null), []);
  const openFullProfile = useCallback((member: ServerMember) => {
    setFullProfileMember(member);
    setSelectedMember(null);
  }, []);
  const closeFullProfile = useCallback(() => setFullProfileMember(null), []);

  return {
    openProfileAt,
    closeProfileCard,
    selectedMember,
    fullProfileMember,
    openFullProfile,
    closeFullProfile,
  };
}
