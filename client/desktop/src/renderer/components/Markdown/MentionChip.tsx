import React from 'react';
import { resolveMentionDisplay, type MentionLookup } from './mentionTypes';

interface MentionChipProps {
  token: string;
  lookup: MentionLookup;
  currentUserId?: string;
  currentUserRoleIds?: ReadonlySet<string>;
}

function tokenMentionsCurrentUser(
  token: string,
  currentUserId: string | undefined,
  currentUserRoleIds: ReadonlySet<string> | undefined
): boolean {
  if (currentUserId !== undefined && token === `<@${currentUserId}>`) return true;
  if (token === '@all' || token === '@everyone' || token === '@here' || token === '@online') {
    return true;
  }
  const roleMatch = /^<@&([\w-]+)>$/.exec(token);
  return roleMatch !== null && currentUserRoleIds?.has(roleMatch[1]) === true;
}

const MentionChip: React.FC<MentionChipProps> = ({
  token,
  lookup,
  currentUserId,
  currentUserRoleIds,
}) => {
  const display = resolveMentionDisplay(token, lookup);
  const isSelfMention = tokenMentionsCurrentUser(token, currentUserId, currentUserRoleIds);
  const className = `mention-highlight mention-highlight--${isSelfMention ? 'self' : 'other'}`;
  return <span className={className}>{display}</span>;
};

export default MentionChip;
