import React from 'react';
import MemberProfileCard from '../Members/MemberProfileCard';
import UserProfileModal from '../Members/UserProfileModal';
import { useMemberStore } from '../../stores/memberStore';
import type { UseMessageProfileCardResult } from '../../hooks/useMessageProfileCard';

interface MessageProfileCardHostProps {
  /**
   * State produced by {@link useMessageProfileCard}. Passed as a single object
   * so the consumer can do `<MessageProfileCardHost state={profileCardState} />`
   * without spreading every callback at the call site.
   */
  state: UseMessageProfileCardResult;
}

/**
 * Renders the profile-card and full-profile-modal subtree for a single chat
 * message, driven by the state produced by `useMessageProfileCard`.
 *
 * **Why this is a separate component:** Concord's architecture lint (Sonar
 * typescript:S6804) forbids hooks/ → components/ imports. `useMessageProfileCard`
 * is the pure state owner; this host carries the JSX coupling to
 * `<MemberProfileCard>` and `<UserProfileModal>` so the hook can stay in
 * the hooks/ layer.
 *
 * Render-time presence data (`userStatuses`, `lastSeenByUser`) is sourced here
 * rather than from the hook so the hook can stay subscription-light — the host
 * is the natural place to react to presence changes for the open card.
 */
export default function MessageProfileCardHost({
  state,
}: Readonly<MessageProfileCardHostProps>): React.ReactElement {
  const userStatuses = useMemberStore((s) => s.userStatuses);
  const lastSeenByUser = useMemberStore((s) => s.lastSeenByUser);

  const { selectedMember, fullProfileMember, closeProfileCard, openFullProfile, closeFullProfile } =
    state;

  return (
    <>
      {selectedMember && (
        <MemberProfileCard
          member={selectedMember.member}
          status={userStatuses.get(selectedMember.member.user_id) || 'offline'}
          lastSeen={lastSeenByUser.get(selectedMember.member.user_id)}
          position={selectedMember.position}
          onClose={closeProfileCard}
          onViewFullProfile={
            selectedMember.serverMember
              ? () => {
                  // Captured at render time; non-null per the ternary guard.
                  const target = selectedMember.serverMember;
                  if (target) openFullProfile(target);
                }
              : undefined
          }
        />
      )}

      {fullProfileMember && (
        <UserProfileModal
          isOpen={!!fullProfileMember}
          onClose={closeFullProfile}
          member={fullProfileMember}
          presenceStatus={userStatuses.get(fullProfileMember.user_id) || 'offline'}
          lastSeen={lastSeenByUser.get(fullProfileMember.user_id)}
        />
      )}
    </>
  );
}
