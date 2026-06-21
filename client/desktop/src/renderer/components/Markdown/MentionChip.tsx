import React from 'react';
import { resolveMentionDisplay, type MentionLookup } from './mentionTypes';

interface MentionChipProps {
  token: string;
  lookup: MentionLookup;
}

const MentionChip: React.FC<MentionChipProps> = ({ token, lookup }) => {
  const display = resolveMentionDisplay(token, lookup);
  return <span className="mention-highlight">{display}</span>;
};

export default MentionChip;
